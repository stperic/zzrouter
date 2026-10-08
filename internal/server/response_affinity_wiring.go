// Wiring between the /v1/responses handlers and the responseAffinity
// store. Kept separate from response_affinity.go so the store stays a
// pure data structure unit-testable without a Server.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/routing"
	"github.com/stperic/zzrouter/pkg/utils"
)

// responseIDCapturingWriter tees a prefix of the outgoing body into an
// internal buffer so the handler can extract the top-level `id` field
// after dispatch. Streaming responses and large bodies are bailed out
// of as soon as they exceed the cap so the buffer cannot grow
// unboundedly.
type responseIDCapturingWriter struct {
	gin.ResponseWriter
	buf         bytes.Buffer
	captureCap  int
	dropCapture bool
}

const defaultResponseBodyCaptureCap = 64 * 1024

func newResponseIDCapturingWriter(w gin.ResponseWriter) *responseIDCapturingWriter {
	return &responseIDCapturingWriter{
		ResponseWriter: w,
		captureCap:     defaultResponseBodyCaptureCap,
	}
}

func (w *responseIDCapturingWriter) Write(p []byte) (int, error) {
	if !w.dropCapture && w.buf.Len() < w.captureCap {
		remaining := w.captureCap - w.buf.Len()
		if len(p) <= remaining {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:remaining])
			w.dropCapture = true
		}
	}
	return w.ResponseWriter.Write(p)
}

// ExtractedID returns the `id` field of the response.
//
// Two payload shapes are supported:
//
//   - Non-streaming create: Content-Type is application/json and the
//     body is a single Response object with a top-level `id`.
//   - Streaming create: Content-Type is text/event-stream and the
//     first SSE event is `response.created`, whose JSON payload
//     carries the new id under `response.id`.
//
// Returns "" when the status is non-2xx, the buffer overflowed
// before an id appeared, or neither shape matches.
func (w *responseIDCapturingWriter) ExtractedID() string {
	status := w.ResponseWriter.Status()
	if status != 0 && (status < 200 || status >= 300) {
		return ""
	}
	if w.dropCapture {
		return ""
	}

	ct := w.Header().Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		return extractResponseIDFromSSE(w.buf.Bytes())
	case ct == "" || strings.HasPrefix(ct, "application/json"):
		var peek struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(w.buf.Bytes(), &peek); err != nil {
			return ""
		}
		return peek.ID
	default:
		return ""
	}
}

// extractResponseIDFromSSE scans an SSE buffer for the first
// `response.created` event and returns response.id from its payload.
// OpenAI's Responses API emits response.created as the very first
// event in a streaming create, so scanning the captured prefix is
// sufficient even with the 64 KiB body cap.
func extractResponseIDFromSSE(buf []byte) string {
	for _, line := range bytes.Split(buf, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var evt struct {
			Type     string `json:"type"`
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
			// Fallback: some backends emit the id at top level on the
			// first frame instead of nested under "response".
			ID string `json:"id"`
		}
		if err := json.Unmarshal(payload, &evt); err != nil {
			continue
		}
		if evt.Type == "response.created" && evt.Response.ID != "" {
			return evt.Response.ID
		}
		if evt.Response.ID != "" {
			return evt.Response.ID
		}
		if evt.ID != "" {
			return evt.ID
		}
	}
	return ""
}

// wireEndpointMode classifies how a model's provider serves a wire
// endpoint, so a surface handler can pick passthrough, a translation
// shim, or a refusal.
type wireEndpointMode int

const (
	// wireModeUnknown — model lookup miss; let the dispatch path's
	// existing 404 handle the unknown-model envelope.
	wireModeUnknown wireEndpointMode = iota
	// wireModeNative — the provider serves the endpoint itself;
	// passthrough.
	wireModeNative
	// wireModeCompat — the provider declares the endpoint's translation
	// shim (e.g. "responses_compat").
	wireModeCompat
	// wireModeUnsupported — the provider declares neither.
	wireModeUnsupported
)

// providerWireMode is the fallback for targets without node-reported endpoints.
func (s *Server) providerWireMode(provider, native, compat string) wireEndpointMode {
	if s.appsConfig == nil {
		return wireModeUnknown
	}
	svc, ok := s.appsConfig.LookupApp(provider)
	if !ok {
		return wireModeUnknown
	}
	if svc.Capabilities.SupportsWireEndpoint(native) {
		return wireModeNative
	}
	if compat != "" && svc.Capabilities.SupportsWireEndpoint(compat) {
		return wireModeCompat
	}
	return wireModeUnsupported
}

// handleResponses handles POST /v1/responses. When the client carries
// a previous_response_id and zzrouter remembers the provider that
// produced it, the chained turn is force-routed there so the backend's
// stored state chain stays intact. Otherwise model-routing picks the
// backend, and the response id is captured post-dispatch and recorded
// in the affinity map for future retrieval calls.
func (s *Server) handleResponses(c *gin.Context) {
	var req struct {
		Model              string `json:"model"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	body, ok := readOpenAIBody(c, &req)
	if !ok {
		return
	}
	if req.Model == "" {
		param := "model"
		OpenAIInvalidRequest(c, "Model field is required for routing", &param)
		return
	}

	var nodeHint string
	req.Model, body, nodeHint = s.stripNodeHint(c, req.Model, body)
	recorder, _ := c.Request.Context().Value(CtxKeyInferenceRecorder).(*llm.InferenceRecorder)
	release, ok := s.enforceAndAttribute(c, req.Model, recorder)
	defer release()
	if !ok {
		return
	}
	if req.PreviousResponseID != "" && s.inference.affinity != nil {
		if providerKey, hit := s.inference.affinity.Lookup(req.PreviousResponseID); hit {
			targetModel := s.responseAffinityTarget(req.Model, providerKey)
			for _, name := range []string{req.Model, targetModel.ModelName} {
				if err := s.validateModel(c.Request.Context(), name); err != nil {
					writeModelAdmissionError(c.Writer, c.Request, err)
					return
				}
			}
			mode := s.targetWireMode(c.Request.Context(), targetModel.ModelName, providerKey, targetModel.Node, "responses", "responses_compat")
			if resolved, ok := s.backend.Resolve(providerKey); ok && (mode == wireModeNative || mode == wireModeUnknown) {
				if !s.admitImages(c, req.Model, body, targetModel) {
					return
				}
				stashInferenceContext(c, req.Model, body)
				target := backend.PassthroughTarget(resolved.Endpoint, c.Request.URL.Path, c.Request.URL.RawQuery)
				capturer := newResponseIDCapturingWriter(c.Writer)
				c.Writer = capturer
				// Affinity hit dispatches to the cached provider without
				// running the resolver. Outcome is local from the
				// node's perspective — the routing decision was "send
				// to the previously-pinned backend" and that backend
				// is reached via the configured endpoint, not via a
				// peer node.
				routing.RecordDecision(c.Request.Context(),
					s.routingStrategy(), routing.OutcomeLocal,
					string(genai.ProviderName(providerKey)),
					string(genai.OperationName(c.FullPath())))
				s.proxy.ForwardToBackend(c.Writer, c.Request, providerKey, target, resolved.Upstream, body)
				if id := capturer.ExtractedID(); id != "" {
					s.inference.affinity.Record(id, providerKey)
				}
				return
			}
		}
	}

	resolved, ok := s.resolveTarget(c, req.Model, nodeHint)
	if !ok {
		return
	}
	if !s.admitImages(c, req.Model, body, resolved) {
		return
	}
	mode, ok := s.narrowToWireEndpoint(c, req.Model, resolved, "responses", "responses_compat")
	if !ok {
		return
	}
	if mode == wireModeCompat {
		s.handleResponsesViaTranslation(c, resolved, req.Model, body)
		return
	}

	stashInferenceContext(c, req.Model, body)
	capturer := newResponseIDCapturingWriter(c.Writer)
	c.Writer = capturer
	s.dispatchTo(c, resolved, req.Model, body, "chat")

	if s.inference.affinity != nil {
		if id := capturer.ExtractedID(); id != "" {
			if providerKey, _ := c.Request.Context().Value(CtxKeyProvider).(string); providerKey != "" {
				s.inference.affinity.Record(id, providerKey)
			}
		}
	}
}

// handleResponsesViaTranslation services /v1/responses for providers
// whose wire_endpoints does not declare "responses". The incoming
// body is rewritten to a Chat Completions body, dispatched through
// the standard chat path, and the upstream chat envelope is
// translated back into a Responses envelope on the way out (buffered
// for non-streaming, line-by-line SSE state machine for streaming).
func (s *Server) handleResponsesViaTranslation(c *gin.Context, resolved *resolver.Resolved, model string, body []byte) {
	chatBody, parsed, terr := translateResponsesRequest(body)
	if terr != nil {
		terr.write(c)
		return
	}

	defer asChatRequest(c, chatBody)()
	stashInferenceContext(c, model, chatBody)

	originalWriter := c.Writer
	if parsed.Stream {
		sw := newResponsesStreamWriter(originalWriter, model)
		c.Writer = sw
		s.dispatchTo(c, resolved, model, chatBody, "chat")
		sw.Close()
		c.Writer = originalWriter
		if s.inference.affinity != nil && sw.Status() >= 200 && sw.Status() < 300 {
			if providerKey, _ := c.Request.Context().Value(CtxKeyProvider).(string); providerKey != "" {
				s.inference.affinity.Record(sw.respID, providerKey)
			}
		}
		return
	}

	bw := newResponsesBufferedWriter(originalWriter, model)
	c.Writer = bw
	s.dispatchTo(c, resolved, model, chatBody, "chat")
	bw.Finalize()
	c.Writer = originalWriter

	if s.inference.affinity != nil && bw.respID != "" && bw.status >= 200 && bw.status < 300 {
		if providerKey, _ := c.Request.Context().Value(CtxKeyProvider).(string); providerKey != "" {
			s.inference.affinity.Record(bw.respID, providerKey)
		}
	}
}

// routeToResponsesBackend handles GET/DELETE /v1/responses/{id},
// POST /v1/responses/{id}/cancel, and GET /v1/responses/{id}/input_items.
// Affinity hits forward to the backend that created the response; on a
// miss the request falls through to the configured default backend
// (the pre-affinity behaviour).
func (s *Server) routeToResponsesBackend(c *gin.Context) {
	responseID := c.Param("response_id")
	if responseID == "" {
		s.routeToDefaultOpenAIBackend(c)
		return
	}

	providerKey, hit := "", false
	if s.inference.affinity != nil {
		providerKey, hit = s.inference.affinity.Lookup(responseID)
	}
	if !hit {
		// No affinity record. Without a stateful default backend the
		// response demonstrably does not exist on this server — return
		// 404 instead of routing through the default-backend path
		// which would emit a misleading 503 "no_default_backend_configured".
		// /v1/responses POST forces store=false, so retrievable state
		// is never created locally; an unknown id is genuinely missing.
		if s.config.OpenAICompat.DefaultBackend == "" {
			param := "response_id"
			code := "response_not_found"
			c.JSON(http.StatusNotFound, utils.NewOpenAIError(
				"invalid_request_error",
				fmt.Sprintf("Response with id '%s' not found", responseID),
				&param,
				&code,
			))
			return
		}
		s.routeToDefaultOpenAIBackend(c)
		return
	}

	resolved, ok := s.backend.Resolve(providerKey)
	if !ok {
		// Recorded provider is no longer registered — fall back to
		// default rather than failing hard, so operators can rewire
		// without breaking in-flight clients. Asymmetric with the
		// no-affinity branch above (which 404s when DefaultBackend is
		// empty): stale affinity is a soft signal that the response
		// once existed, so falling through preserves operator agency
		// during a provider rewire. A no-affinity miss with no default
		// backend means the id was never recorded at all and we can
		// be definitive.
		s.routeToDefaultOpenAIBackend(c)
		return
	}

	body, bodyOK := s.readPassthroughBody(c)
	if !bodyOK {
		return
	}
	target := backend.PassthroughTarget(resolved.Endpoint, c.Request.URL.Path, c.Request.URL.RawQuery)
	s.proxy.ForwardToBackend(c.Writer, c.Request, providerKey, target, resolved.Upstream, body)

	// Optimistic flush on DELETE. ForwardToBackend has already written
	// to the client, so we cannot branch on the upstream status code
	// without wrapping the writer. Flushing regardless means that on
	// an upstream 4xx/5xx the next retrieval misses the affinity map
	// and is routed to the default backend — acceptable because the
	// caller will see the upstream's own 404 whether we flushed or
	// not, and keeping a stale entry would hide real backend state
	// from future callers.
	if c.Request.Method == http.MethodDelete {
		s.inference.affinity.Delete(responseID)
	}
}
