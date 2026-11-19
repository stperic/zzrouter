// Wiring between the /v1/responses handlers and the responseAffinity
// store. Kept separate from response_affinity.go so the store stays a
// pure data structure unit-testable without a Server.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/dispatch/affinity"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// responseIDCapturingWriter extracts identifiers without retaining response payloads.
// JSON is scanned incrementally; SSE retains only a bounded event prefix.
type responseIDCapturingWriter struct {
	gin.ResponseWriter
	buf         bytes.Buffer
	captureCap  int
	dropCapture bool
	id          string
	onID        func(string)
	jsonID      responseJSONIDScanner
}

const defaultResponseBodyCaptureCap = 64 * 1024

func newResponseIDCapturingWriter(w gin.ResponseWriter) *responseIDCapturingWriter {
	return &responseIDCapturingWriter{
		ResponseWriter: w,
		captureCap:     defaultResponseBodyCaptureCap,
	}
}

func (w *responseIDCapturingWriter) Write(p []byte) (int, error) {
	ct := w.Header().Get("Content-Type")
	if w.id == "" {
		switch {
		case strings.HasPrefix(ct, "text/event-stream"):
			if !w.dropCapture {
				remaining := w.captureCap - w.buf.Len()
				if len(p) > remaining {
					w.buf.Write(p[:remaining])
					w.dropCapture = true
				} else {
					w.buf.Write(p)
				}
			}
			w.id = extractResponseIDFromSSE(w.buf.Bytes())
		case ct == "" || strings.HasPrefix(ct, "application/json"):
			w.id = w.jsonID.write(p, w.captureCap)
		}
	}
	n, err := w.ResponseWriter.Write(p)
	if w.onID != nil {
		if id := w.ExtractedID(); id != "" {
			w.onID(id)
			w.onID = nil
		}
	}
	return n, err
}

func (w *responseIDCapturingWriter) WriteString(p string) (int, error) { return w.Write([]byte(p)) }

// ExtractedID returns the observed identifier only for a successful response.
func (w *responseIDCapturingWriter) ExtractedID() string {
	status := w.ResponseWriter.Status()
	if status != 0 && (status < 200 || status >= 300) {
		return ""
	}
	return w.id
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
	apps := s.appsConfig()
	if apps == nil {
		return wireModeUnknown
	}
	svc, ok := apps.LookupApp(provider)
	if !ok {
		return wireModeUnknown
	}
	return wireModeForEndpoints(svc.Capabilities.EffectiveWireEndpoints(), native, compat)
}

// handleResponses admits one turn and records the target the dispatcher actually used.
func (s *Server) handleResponses(c *gin.Context) {
	var req struct {
		Model              string `json:"model"`
		PreviousResponseID string `json:"previous_response_id"`
		Store              *bool  `json:"store"`
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
	owner := responseOwner(c)
	carryResponseOwner(c, owner)
	var previous affinity.Origin
	recorder, _ := c.Request.Context().Value(CtxKeyInferenceRecorder).(*llm.InferenceRecorder)
	resolved, release, ok := s.enforceAndAttribute(c, req.Model, recorder, func() (*resolver.Resolved, bool) {
		if req.PreviousResponseID == "" {
			return s.prepareTarget(c, req.Model, body, "responses", "responses_compat")
		}
		var hit bool
		previous, hit = s.inference.affinity.Lookup(req.PreviousResponseID, owner)
		if !hit {
			responseNotFound(c)
			return nil, false
		}
		if previous.ClientModel != req.Model {
			responseOriginError(c, "response_model_mismatch", "Continue the response with the model that created it")
			return nil, false
		}
		target := responseOriginTarget(previous)
		for _, name := range []string{req.Model, target.ModelName} {
			if err := s.validateModel(c.Request.Context(), name); err != nil {
				writeModelAdmissionError(c.Writer, c.Request, err)
				return nil, false
			}
		}
		if !s.admitAccessTargets(c, target) || !s.admitImages(c, req.Model, body, target) {
			return nil, false
		}
		if !s.responseOriginAvailable(c, previous) {
			return nil, false
		}
		s.quoteTargets(c, target)
		return target, true
	})
	defer release()
	if !ok {
		return
	}
	if req.PreviousResponseID == "" && s.targetWireMode(c.Request.Context(), resolved, "responses", "responses_compat") == wireModeCompat {
		s.handleResponsesViaTranslation(c, resolved, req.Model, body)
		return
	}
	stored := req.Store == nil || *req.Store || req.PreviousResponseID != ""
	if stored && owner.ID == "" {
		responseOriginUnauthorized(c)
		return
	}
	var origin affinity.Origin
	c.Request = c.Request.WithContext(affinity.WithObserver(c.Request.Context(), func(selected affinity.Origin) {
		selected.ClientModel, selected.Route = req.Model, resolved.GroupName
		origin = selected
	}))
	stashInferenceContext(c, req.Model, body)
	capturer := newResponseIDCapturingWriter(c.Writer)
	if stored {
		capturer.onID = func(id string) { s.inference.affinity.Record(id, owner, origin) }
	}
	c.Writer = capturer
	if req.PreviousResponseID != "" {
		origin = previous
		body = s.resolvedRequestBody(c, resolved, req.Model, body)
		s.forwardResponseOrigin(c, previous, body)
	} else {
		s.dispatchTo(c, resolved, req.Model, body, "chat")
	}
	if stored {
		s.inference.affinity.Record(capturer.ExtractedID(), owner, origin)
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
		sw := newResponsesStreamWriter(originalWriter, model, parsed.toolNames)
		c.Writer = sw
		s.dispatchTo(c, resolved, model, chatBody, "chat")
		sw.Close()
		c.Writer = originalWriter
		return
	}

	bw := newResponsesBufferedWriter(originalWriter, model, parsed.toolNames)
	c.Writer = bw
	s.dispatchTo(c, resolved, model, chatBody, "chat")
	bw.Finalize()
	c.Writer = originalWriter
}

// routeToResponsesBackend requires caller-scoped ownership for every resource operation.
func (s *Server) routeToResponsesBackend(c *gin.Context) {
	owner := responseOwner(c)
	carryResponseOwner(c, owner)
	origin, hit := s.inference.affinity.Lookup(c.Param("response_id"), owner)
	if !hit {
		responseNotFound(c)
		return
	}
	if !s.access.Authorize(c, origin.ClientModel) || !s.admitAccessTargets(c, responseOriginTarget(origin)) {
		return
	}
	if !s.responseOriginAvailable(c, origin) {
		return
	}
	body, ok := s.readPassthroughBody(c)
	if !ok {
		return
	}
	stashInferenceContext(c, origin.Model, body)
	s.forwardResponseOrigin(c, origin, body)
	if c.Request.Method == http.MethodDelete && c.Writer.Status() >= 200 && c.Writer.Status() < 300 {
		s.inference.affinity.Delete(c.Param("response_id"), owner)
	}
}
