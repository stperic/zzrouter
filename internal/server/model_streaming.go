// package server provides HTTP handlers for the zzrouter host server.
// Model streaming orchestrators — the wire-level framing, copy, commit,
// injection, and error helpers live in pkg/dispatch/wire.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	obsgenai "github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/proxy"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stperic/zzrouter/pkg/utils"
)

// HandleLocalModel implements the LocalModelHandler interface for the Server.
// HandleLocalModel handles requests for local models. endpoint is the
// per-request endpoint label ("chat" for callers that don't know).
func (s *Server) HandleLocalModel(w http.ResponseWriter, req *http.Request, model, endpoint string) error {
	if err := s.validateModel(req.Context(), model); err != nil {
		writeModelAdmissionError(w, req, err)
		return err
	}
	if endpoint == "" {
		endpoint = "chat"
	}

	// Alias resolution: rewrite to the cache canonical (indexByName key)
	// before any instance/provider lookup so SourceID aliases (e.g.
	// HF repo path "Qwen/.../GGUF") and the file-stem name resolve to
	// the same instance / launch keys. Best-effort — a cache miss leaves
	// `model` untouched and downstream "model_not_found" still applies.
	if canonical := s.resolveCanonicalModelName(model); canonical != "" && canonical != model {
		model = canonical
	}

	// FAST PATH: Check if model is already running (on-demand instances
	// launched via runs API). Must be checked BEFORE findCapableProvider,
	// as dynamically launched models may not be in the model registry but
	// are ready to serve.
	if s.providers.appMgr != nil {
		registry := s.providers.appMgr.Instances()
		existingInstance, found := registry.GetByModelEndpoint(model, endpoint)
		if found && existingInstance.GetStatus() == instance.StatusRunning {
			s.proxyToInstance(w, req, existingInstance, true)
			return nil
		}
	}

	// Check if provider was passed from coordinator (workers receive this)
	providerType := req.Header.Get(constants.HeaderServingProvider)

	// If no provider header, find it locally. A model that has no capable
	// provider maps to OpenAI's canonical "model_not_found" envelope —
	// type=invalid_request_error, code=model_not_found, HTTP 404 — so
	// stock OpenAI SDK clients can switch on error.type without needing
	// to learn a zzRouter-specific value.
	if providerType == "" {
		var err error
		providerType, _, err = s.findCapableProvider(model)
		if err != nil {
			if problem := s.providerInventoryFailure(s.missingInventoryNode(req), "", true); problem != nil {
				recordErrorOnContext(req.Context(), problem.Code, problem.Message)
				writeError(w, req, *problem)
				return fmt.Errorf("%s", problem.Message)
			}
			recordErrorOnContext(req.Context(), "model_not_found", err.Error())
			writeError(w, req, httperr.Error{Status: http.StatusNotFound, Type: "invalid_request_error", Message: err.Error(), Code: "model_not_found"})
			return err
		}
	}
	if problem := s.providerInventoryFailure(s.node.Nodename(), providerType, false); problem != nil {
		recordErrorOnContext(req.Context(), problem.Code, problem.Message)
		writeError(w, req, *problem)
		return fmt.Errorf("%s", problem.Message)
	}

	// A provider with its own endpoint (external, service, cloud) is
	// forwarded to; only on-demand providers go on to a running instance.
	if resolved, ok := s.backend.Resolve(providerType); ok {
		body, _ := req.Context().Value(CtxKeyOriginalBody).([]byte)
		target := backend.PassthroughTarget(resolved.Endpoint, req.URL.Path, req.URL.RawQuery)
		s.proxy.ForwardToBackend(w, req, providerType, target, resolved.Upstream, body)
		return nil
	}

	// For on-demand apps, check if model is already running (also handles
	// starting status).
	registry := s.providers.appMgr.Instances()
	existingInstance, found := registry.GetByModelEndpoint(model, endpoint)

	// Only this handler's own surfaces (/v1 and the responses/realtime
	// wrappers) reach here, and OpenAI's `stream` defaults to false when
	// absent. Ollama's defaults to true and is read by
	// ollamaStreamRequested on its own dispatch path — do not "unify"
	// the two readings.
	isStreaming := false
	if originalBody, ok := req.Context().Value(CtxKeyOriginalBody).([]byte); ok {
		var reqBody map[string]any
		if json.Unmarshal(originalBody, &reqBody) == nil {
			isStreaming, _ = reqBody["stream"].(bool)
		}
	}

	// A streaming request whose instance is cold used to answer 200 +
	// text/event-stream before the load was even attempted, which cost
	// twice: a load failure could then only be reported inside a stream
	// the client had already been told was fine, and every cold start
	// spent two SSE frames on status updates that are not chat chunks
	// (a stock OpenAI client yields them as chunks with no choices, so
	// the canonical chunk.choices[0] panics on the happy path).
	//
	// So load first and commit to a response shape once the answer is
	// known. Only a load still running after the grace period falls back
	// to the old behaviour, where progress frames and a warm connection
	// are worth more than a status code the caller cannot get anyway.
	//
	// The fallback is the dialect's to allow: it reports a load failure
	// inside the stream, which only an in-band streamer wants.
	sseStarted := false
	var inst *instance.Instance
	var err error
	dialect := dialectOf(req)
	streamer, inBand := dialect.(httperr.InBandStreamer)

	if isStreaming && inBand &&
		(!found || existingInstance.GetStatus() != instance.StatusRunning) {
		type loadOutcome struct {
			inst *instance.Instance
			err  error
		}
		// Buffered so the load goroutine never blocks on a caller that
		// disconnected; the instance it loaded stays up for the next one.
		done := make(chan loadOutcome, 1)
		go func() {
			i, e := s.loadModelViaNodeAPI(model, providerType, endpoint)
			done <- loadOutcome{i, e}
		}()

		grace := time.NewTimer(constants.ModelLoadGrace)
		defer grace.Stop()

		select {
		case out := <-done:
			inst, err = out.inst, out.err
		case <-grace.C:
			openEventStream(w)
			sseStarted = true
			streamer.StreamStatus(w, "loading_model", fmt.Sprintf("Loading model %s ...please wait.\n", model))

			// Committed to 200 now, so the only remaining risks are
			// silence and a caller nobody is listening to. Heartbeat
			// through the wait, and stop waiting if the client hangs up
			// — the load itself keeps running for whoever asks next,
			// which is the point of the buffered channel above.
			heartbeat := time.NewTicker(constants.ModelLoadHeartbeat)
			defer heartbeat.Stop()
		waitForLoad:
			for {
				select {
				case out := <-done:
					inst, err = out.inst, out.err
					break waitForLoad
				case <-heartbeat.C:
					sendSSEComment(w, "loading model")
				case <-req.Context().Done():
					return req.Context().Err()
				}
			}
		}
	} else {
		inst, err = s.loadModelViaNodeAPI(model, providerType, endpoint)
	}

	if err != nil {
		lf := classifyLoadFailure(err)
		// proxy.Middleware derives error.type from the status when nobody
		// says otherwise, and its 5xx default is server_error — right for
		// loadInternal, wrong for the 503 an unavailable provider gets.
		// Stashing the classified type unconditionally keeps the metric
		// label equal to the body's error.type on every branch.
		*req = *proxy.SetErrorTypeOnRequest(req, lf.errType)
		recordErrorOnContext(req.Context(), lf.code, lf.message)
		failure := httperr.Error{Status: lf.status, Type: lf.errType, Message: lf.message, Code: lf.code}
		if sseStarted {
			// Already committed to 200: the error can only travel inside
			// the stream. Same type/code/message as the header path, so a
			// caller reading error.code gets one answer either way.
			streamer.StreamError(w, failure)
			return err
		}
		// Only meaningful on the header path: once the stream is
		// committed there is no header left to set.
		if lf.retryAfterSecs > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(lf.retryAfterSecs))
		}
		dialect.WriteError(w, req, failure)
		return err
	}

	// Forward to instance
	if sseStarted {
		streamer.StreamStatus(w, "model_ready", "Model loaded. Generating response...")
		s.streamFromInstance(w, req, inst, streamer)
	} else {
		// Including a streaming request whose load beat the grace: it
		// takes the same path a warm one does, which is also the path
		// that records an inference-log entry.
		s.proxyToInstance(w, req, inst, false)
	}
	return nil
}

// loadModelViaNodeAPI loads a LOCAL model directly via executor. No
// routing needed — we're already on the correct node.
func (s *Server) loadModelViaNodeAPI(modelName, providerType, endpoint string) (*instance.Instance, error) {
	executor := s.newLoadExecutor()
	req := &LoadModelRequest{
		ModelName: modelName,
		Provider:  providerType,
		Endpoint:  endpoint,
	}

	resp, err := executor.LoadLocalModel(context.Background(), req)
	if err != nil {
		return nil, classifyLoadFailure(err)
	}

	// Handle error responses (but 409 Conflict is OK — means model is
	// already running). The executor's own status code is what separates
	// a model that was never downloaded from an engine that died, so
	// carry it instead of flattening the response to its message.
	if resp.Error != "" && resp.StatusCode != http.StatusConflict {
		message := resp.Error
		if resp.Details != "" {
			message = fmt.Sprintf("%s: %s", resp.Error, resp.Details)
		}
		return nil, newLoadFailure(resp.StatusCode, message)
	}

	// Get instance from local registry
	inst, found := s.providers.appMgr.Instances().Get(resp.InstanceID)
	if !found {
		return nil, loadInternal(fmt.Sprintf("instance %s not found after loading", resp.InstanceID))
	}

	// Wait for local instance to be ready if still starting
	if inst.GetStatus() == instance.StatusStarting {
		slog.Info("Model is starting locally, waiting for ready", "model_name", modelName)
		return s.waitForLocalInstance(inst)
	}

	return inst, nil
}

// waitForLocalInstance waits for a local instance to become ready.
//
// The budget runs from when the INSTANCE started, not from when this
// request arrived. A provider may declare a readiness window far longer
// than a request should be held open (MLX allows 600s against this
// package's 120s), and doLoadLocalModel hands an already-starting
// instance straight back, so measuring per-request gave every caller in
// that window a fresh full-length hang against the same slow load.
// Measuring from the instance means the first caller waits, and the ones
// behind it get told to come back instead of queueing on it.
func (s *Server) waitForLocalInstance(inst *instance.Instance) (*instance.Instance, error) {
	budget := constants.ModelStreamingTimeout
	if started := inst.GetStartedAt(); !started.IsZero() {
		budget -= utils.Now().Sub(started)
	}
	if budget <= 0 {
		return nil, loadStillWarming(fmt.Sprintf(
			"instance '%s' is still loading; retry shortly", inst.ID))
	}

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	ticker := time.NewTicker(constants.ProgressPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, loadStillWarming(fmt.Sprintf(
				"instance '%s' did not become ready in time; the load is still running, retry shortly", inst.ID))
		case <-ticker.C:
			currentInstance, found := s.providers.appMgr.Instances().Get(inst.ID)
			if !found {
				return nil, loadInternal(fmt.Sprintf("instance '%s' disappeared while waiting", inst.ID))
			}

			switch currentInstance.GetStatus() {
			case instance.StatusRunning:
				slog.Info("Instance is now ready", "id", currentInstance.ID, "port", currentInstance.Port)
				return currentInstance, nil
			case instance.StatusFailed:
				// GetErrorMessage takes the instance mutex; direct field
				// access races with MarkFailed's write (Status atomic and
				// the ErrorMessage write are not co-synchronized).
				return nil, newLoadFailure(http.StatusBadGateway,
					fmt.Sprintf("instance '%s' failed to start: %s", currentInstance.ID, currentInstance.GetErrorMessage()))
			default:
				// Still starting, continue polling.
			}
		}
	}
}

// getInstanceBackendNode extracts the backend host from instance.
// Returns "localhost" for local instances, or the remote host from
// HealthURL for remote instances.
func getInstanceBackendNode(inst *instance.Instance) string {
	if inst.HealthURL != "" && !strings.Contains(inst.HealthURL, constants.Localhost) {
		// Extract host from HealthURL (e.g., "http://192.0.2.10:8080/health" → "192.0.2.10")
		if u, err := url.Parse(inst.HealthURL); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return constants.Localhost
}

// streamFromInstance streams response from instance when SSE headers are
// already committed (200 + text/event-stream). Errors travel inside the
// stream through streamer. Only called from HandleLocalModel after the
// SSE preamble, which only an in-band streamer opens.
func (s *Server) streamFromInstance(w http.ResponseWriter, req *http.Request, inst *instance.Instance, streamer httperr.InBandStreamer) {
	// Concurrency gating: acquire a slot before streaming.
	if err := inst.AcquireConcurrency(req.Context()); err != nil {
		if errors.Is(err, instance.ErrAtCapacity) {
			streamer.StreamError(w, httperr.Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error",
				Message: capacityErrorMessage(inst), Code: "capacity_exceeded"})
		}
		// Context cancelled = client disconnected, nothing to write.
		return
	}
	defer inst.ReleaseConcurrency()

	// Only update activity for local instances, applying any per-request
	// keep_alive override parsed at the handler edge (see
	// CtxKeyRequestHints).
	if getInstanceBackendNode(inst) == constants.Localhost {
		ov, _ := req.Context().Value(CtxKeyRequestHints).(*keepalive.Override)
		_ = s.providers.appMgr.Instances().UpdateActivity(inst.ID, ov)
	}

	body, answerAs := s.instanceRequest(req, inst)
	rec, _ := req.Context().Value(CtxKeyInferenceRecorder).(*llm.InferenceRecorder)
	rec.SetUpstreamRequestData(body)
	backendNode := getInstanceBackendNode(inst)
	rec.SetServer(backendNode, inst.Port)
	backendURL := fmt.Sprintf("http://%s:%d%s", backendNode, inst.Port, req.URL.Path)

	// In-flight saturation: cold-load streaming dispatches via
	// httpStreamingClient.Do directly, not through proxyToInstance, so
	// it needs its own Inc/Dec wrap. Without this, every cold-load
	// streaming request would be invisible to saturation alerts.
	model, _ := req.Context().Value(CtxKeyModel).(string)
	inflightToken := proxy.IncInflight(req.Context(),
		string(obsgenai.OperationName(req.URL.Path)),
		inst.Provider, model, backendNode)
	defer proxy.DecInflight(req.Context(), inflightToken)

	// Create a streaming context derived from request context. This
	// ensures the backend request is cancelled if the client disconnects
	// but removes any timeout (streaming can run indefinitely).
	streamCtx, cancel := context.WithCancel(req.Context())
	defer cancel()

	backendReq, _ := http.NewRequestWithContext(streamCtx, req.Method, backendURL, bytes.NewReader(body))
	backendReq.Header = upstreamHeaders(req.Header, backend.Engine())
	backendReq.Header.Set("Content-Type", "application/json")

	resp, err := s.httpStreamingClient.Do(backendReq)
	if err != nil {
		// Same cause the header path calls api_error/backend_unreachable
		// (proxy_client.go): the backend was unreachable, which is upstream
		// visible rather than a zzRouter fault.
		streamer.StreamError(w, httperr.Error{Status: http.StatusBadGateway, Type: "api_error",
			Message: fmt.Sprintf("Backend error: %v", err), Code: "backend_unreachable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		failure := httperr.NormalizeUpstreamResponse(resp, dialectOf(req))
		if rec != nil {
			rec.SetError(failure.Type, failure.Message)
			rec.RecordCompletion(0, 0)
		}
		streamer.StreamError(w, failure)
		return
	}

	// Metered and shaped exactly like the warm path. This used to be
	// CopyWithFlush, which is CopyWithMetrics with a nil recorder: a
	// cold load that outran ModelLoadGrace streamed its whole answer
	// while the inference log recorded no tokens and no cost, and the
	// terminal usage frame reached callers who never asked for it.
	// Same local instances, same surface, opposite behaviour from its
	// sibling in proxyToInstance.
	servingNode := backendNode
	if servingNode == constants.Localhost {
		servingNode = s.GetNodename()
	}
	meta := s.localRoutingMetadata(req, inst, servingNode, answerAs)
	copyStreamWithMetrics(w, resp.Body, rec, s.localStreamTransform(meta, inst.Provider, rec))
}

// capacityErrorMessage returns a standardized capacity error message for
// a given instance.
func capacityErrorMessage(inst *instance.Instance) string {
	return fmt.Sprintf("This model's provider (%s) is at capacity (%d concurrent). Please retry shortly.",
		inst.Provider, inst.ConcurrencyLimit())
}
