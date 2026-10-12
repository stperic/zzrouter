// package server provides HTTP handlers for the zzrouter host server.
// Model streaming orchestrators — the wire-level framing, copy, commit,
// injection, and error helpers live in pkg/dispatch/wire.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	obsgenai "github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/proxy"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

// HandleLocalModel handles requests for local models. endpoint is the
// per-request endpoint label ("chat" for callers that don't know).
func (s *Server) HandleLocalModel(w http.ResponseWriter, req *http.Request, resolved *resolver.Resolved, endpoint string) error {
	model, providerType := resolved.ModelName, resolved.Provider
	if err := s.validateModel(req.Context(), model); err != nil {
		writeModelAdmissionError(w, req, err)
		return err
	}
	if endpoint == "" {
		endpoint = "chat"
	}

	// A bound running model may be absent from the catalog.
	existingInstance, found := s.modelInstance(resolved, endpoint)
	if found && existingInstance.GetStatus() == instance.StatusRunning {
		s.proxyToInstance(w, req, existingInstance, true)
		return nil
	}

	if providerType == "" {
		if problem := s.providerInventoryFailure(s.missingInventoryNode(req), "", true); problem != nil {
			recordErrorOnContext(req.Context(), problem.Code, problem.Message)
			writeError(w, req, *problem)
			return fmt.Errorf("%s", problem.Message)
		}
		err := fmt.Errorf("model %q not found in registry; if this is a cloud-served model, register it via POST /zzrouter/v1/deployments first", model)
		recordErrorOnContext(req.Context(), "model_not_found", err.Error())
		writeError(w, req, httperr.Error{Status: http.StatusNotFound, Type: "invalid_request_error", Message: err.Error(), Code: "model_not_found"})
		return err
	}
	if problem := s.providerInventoryFailure(s.node.Nodename(), providerType, false); problem != nil {
		recordErrorOnContext(req.Context(), problem.Code, problem.Message)
		writeError(w, req, *problem)
		return fmt.Errorf("%s", problem.Message)
	}

	// A provider without runs is forwarded to; every other one serves through a run.
	if !s.hasRuns(providerType) {
		if resolved, ok := s.backend.Resolve(providerType); ok {
			body, _ := req.Context().Value(CtxKeyOriginalBody).([]byte)
			target := backend.PassthroughTarget(resolved.Endpoint, req.URL.Path, req.URL.RawQuery)
			s.proxy.ForwardToBackend(w, req, providerType, target, resolved.Upstream, body)
			return nil
		}
	}

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
		failure := loadFailureWire(err)
		*req = *proxy.SetErrorTypeOnRequest(req, failure.Type)
		recordErrorOnContext(req.Context(), failure.Code, failure.Message)
		if sseStarted {
			// Already committed to 200: the error can only travel inside
			// the stream. Same type/code/message as the header path, so a
			// caller reading error.code gets one answer either way.
			streamer.StreamError(w, failure)
			return err
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

// loadModelViaNodeAPI is a local model's ready run, started if need be: we are already on its node.
func (s *Server) loadModelViaNodeAPI(modelName, providerType, endpoint string) (*instance.Instance, error) {
	inst, err := s.ensureLocalRun(prov_apps.EnsureRequest{Model: modelName, Provider: providerType, Endpoint: prov_apps.Endpoint(endpoint)})
	if err == nil && inst == nil {
		return nil, loadInternal("the provider serves this model without a run")
	}
	return inst, err
}

// ensureLocalRun is req's ready run, nil when its provider serves the model without one; the start outlives the request.
func (s *Server) ensureLocalRun(req prov_apps.EnsureRequest) (*instance.Instance, error) {
	req.Origin = prov_apps.OriginRequest
	result, err := s.providers.appMgr.EnsureRun(context.Background(), req)
	if err != nil {
		return nil, err
	}
	if inst := result.Run; inst != nil {
		if status := inst.GetStatus(); status == instance.StatusStarting || status.IsTerminal() {
			return s.waitForLocalInstance(inst)
		}
	}
	return result.Run, nil
}

// waitForLocalInstance waits for a local instance to become ready.
//
// The budget runs from when the INSTANCE started, not from when this
// request arrived. A provider may declare a readiness window far longer
// than a request should be held open (MLX allows 600s against this
// package's 120s), and EnsureRun hands an already-starting
// instance straight back, so measuring per-request gave every caller in
// that window a fresh full-length hang against the same slow load.
// Measuring from the instance means the first caller waits, and the ones
// behind it get told to come back instead of queueing on it.
func (s *Server) waitForLocalInstance(inst *instance.Instance) (*instance.Instance, error) {
	// Wait on the run held, never the registry: a failed run that was replaced still has its cause.
	select {
	case <-inst.LeftStarting():
	default:
		budget := constants.ModelStreamingTimeout
		if started := inst.GetStartedAt(); !started.IsZero() {
			budget -= utils.Now().Sub(started)
		}
		if budget <= 0 {
			return nil, loadStillWarming(fmt.Sprintf(
				"instance '%s' is still loading; retry shortly", inst.ID)).forRun(inst)
		}
		timer := time.NewTimer(budget)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil, loadStillWarming(fmt.Sprintf(
				"instance '%s' did not become ready in time; the load is still running, retry shortly", inst.ID)).forRun(inst)
		case <-inst.LeftStarting():
		}
	}
	if s.providers.readinessWoken != nil {
		s.providers.readinessWoken()
	}
	// The failure first: replacing a failed run stops it, which moves its status past Failed.
	if cause := inst.FailureCause(); cause != nil {
		return nil, fmt.Errorf("instance %q failed to start: %w", inst.ID, cause)
	}
	if message := inst.GetErrorMessage(); message != "" {
		return nil, loadEngineFailed(fmt.Sprintf("instance '%s' failed to start: %s", inst.ID, message))
	}
	if inst.GetStatus() == instance.StatusRunning {
		slog.Info("Instance is now ready", "id", inst.ID, "port", inst.Port)
		return inst, nil
	}
	// Stopped while waiting: a retry finds the model's next run.
	return nil, &instance.AdmissionError{Reason: instance.ErrStopping, RetryAfter: instance.AdmissionRetryAfter}
}

// streamFromInstance streams response from instance when SSE headers are
// already committed (200 + text/event-stream). Errors travel inside the
// stream through streamer. Only called from HandleLocalModel after the
// SSE preamble, which only an in-band streamer opens.
func (s *Server) streamFromInstance(w http.ResponseWriter, req *http.Request, inst *instance.Instance, streamer httperr.InBandStreamer) {
	model, _ := req.Context().Value(CtxKeyModel).(string)
	rec := requestRecorder(req.Context(), model, inst.Provider)
	lease, target, up, ok := s.openRun(req, inst, rec, func(e httperr.Error) { streamer.StreamError(w, e) })
	if !ok {
		return
	}
	defer lease.Release()

	body, answerAs := s.instanceRequest(req, inst)
	rec.SetUpstreamRequestData(body)
	rec.SetServer(target.Hostname(), portFromURL(target))

	inflightToken := proxy.IncInflight(req.Context(),
		string(obsgenai.OperationName(req.URL.Path)),
		inst.Provider, model, target.Hostname())
	defer proxy.DecInflight(req.Context(), inflightToken)

	resp, err := s.sendToInstance(req, lease, body, target, up)
	if err != nil {
		streamer.StreamError(w, instanceSendFailure(rec, err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		failure := httperr.NormalizeUpstreamResponse(resp, dialectOf(req), inst.FailureClassifier)
		if rec != nil {
			rec.SetError(failure.Type, failure.Message)
			rec.RecordCompletion(0, 0)
		}
		streamer.StreamError(w, failure)
		return
	}

	meta := s.localRoutingMetadata(req, inst, s.GetNodename(), answerAs)
	result := wire.CopyStream(req.Context(), w, resp.Body, rec, wire.NewStreamTransform(s.inference.normalizers.Stream(inst.Provider), meta, rec).Func())
	endBrokenStream(w, req, result)
}
