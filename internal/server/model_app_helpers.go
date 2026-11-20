// package server provides HTTP handlers for the zzrouter host server.
// Provider discovery and helper functions

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/affinity"
	"github.com/stperic/zzrouter/pkg/dispatch/chain"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	obsgenai "github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	obsproxy "github.com/stperic/zzrouter/pkg/observability/proxy"
	"github.com/stperic/zzrouter/pkg/protocol"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stperic/zzrouter/pkg/utils"
)

// resolveCanonicalModelName returns the cache's canonical name for a
// model, following SourceID aliases. Returns the input unchanged when
// the cache misses or returns the same name. Best-effort: callers
// don't error on miss — downstream lookups handle "not found" via
// their own paths.
func (s *Server) resolveCanonicalModelName(model string) string {
	if model == "" || s.model.Cache == nil {
		return model
	}
	replicas, err := s.model.Cache.LookupReplicas(context.Background(), model, "", "", "")
	if err == nil {
		for _, replica := range replicas {
			if s.node.IsLocalNode(replica.Node) {
				return replica.Model
			}
		}
	}
	return model
}

// modelSource returns the registry the catalog records model's weights
// as coming from, or "" when the catalog does not know the model.
func (s *Server) modelSource(ctx context.Context, model string) string {
	if s.model.Cache == nil {
		return ""
	}
	cm, err := s.LookupModel(ctx, model)
	if err != nil || cm == nil {
		return ""
	}
	return cm.SourceRepo
}

// requestDefaults resolves the provider's request blocks for text generation.
// Embeddings, reranking and audio skip defaults because engines may refuse sampling fields there.
// The addressed model keys the lookup so a variant sharing its base's process keeps its own defaults.
//
// instanceRequest applies defaults for a run, and the fallback chain applies them for each selected replica.
// Setting only absent fields makes reapplication on a worker a no-op when the coordinator already set them.
func (s *Server) requestDefaults(ctx context.Context, provider, model, path string) map[string]any {
	if s.configStore == nil || model == "" {
		return nil
	}
	if op := obsgenai.OperationName(path); op != obsgenai.OperationChat && op != obsgenai.OperationTextCompletion {
		return nil
	}
	sc, ok := s.providerConfig(provider)
	if !ok {
		return nil
	}
	// An alias (a repo id, a file stem) reaches the same model entry its
	// canonical name does; a variant is its own entry.
	if s.model != nil && s.model.Cache != nil {
		if cm, err := s.LookupModel(ctx, model); err == nil && cm != nil && cm.Name != "" {
			model = cm.Name
		}
	}
	resolved := sc.ResolveEndpoint("", model, "").Request
	if len(resolved) == 0 {
		return nil
	}
	values := make(map[string]any, len(resolved))
	for k, v := range resolved {
		values[k] = v.Value
	}
	return values
}

// providerConfig looks a provider up by any name a launch accepts for it,
// since an instance's Provider keeps the caller's spelling.
func (s *Server) providerConfig(name string) (pkgConfig.ServiceConfig, bool) {
	if s.providers.appMgr != nil {
		return s.providers.appMgr.ProviderConfig(name)
	}
	if s.configStore == nil {
		return pkgConfig.ServiceConfig{}, false
	}
	return s.configStore.Config().LookupApp(name)
}

type instanceRequest = chain.InstanceRequest

func newInstanceRequest(req *http.Request, body []byte) instanceRequest {
	header := req.Header
	if header == nil {
		header = make(http.Header)
	}
	ctx := req.Context()
	hints, _ := ctx.Value(CtxKeyRequestHints).(*keepalive.Override)
	return instanceRequest{
		Context: ctx, Method: req.Method, Path: req.URL.Path, RawQuery: req.URL.RawQuery,
		Header: header, OriginalBody: body, ClientModel: clientModelFromContext(ctx), Hints: hints,
	}
}

func (s *Server) openInstance(req instanceRequest, inst *instance.Instance) (*instance.Lease, error) {
	if req.Context == nil || req.Header == nil {
		return nil, errors.New("instance request requires a context and header")
	}
	if req.Method == "" || req.Path == "" {
		return nil, errors.New("instance request requires a method and path")
	}
	if req.Synthetic {
		return inst.AcquireSynthetic(req.Context, req.Hints)
	}
	return inst.Acquire(req.Context, req.Hints)
}

// openRun leases a local run and resolves its target; on failure it answers through fail unless the client is gone.
func (s *Server) openRun(req instanceRequest, inst *instance.Instance, recorder *llm.InferenceRecorder, fail func(httperr.Error)) (*instance.Lease, *url.URL, backend.Upstream, bool) {
	lease, err := s.openInstance(req, inst)
	if err != nil {
		var admission *instance.AdmissionError
		if errors.As(err, &admission) {
			fail(failureWire(err, http.StatusServiceUnavailable))
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Debug("Client disconnected while waiting for concurrency slot", "instance", inst.ID)
		} else {
			fail(instanceSendFailure(recorder, err))
		}
		return nil, nil, backend.Upstream{}, false
	}
	base, up, err := s.providers.appMgr.Upstream(inst)
	var target *url.URL
	if err == nil {
		target, err = url.Parse(backend.PassthroughTarget(base, req.Path, req.RawQuery))
	}
	if err != nil {
		lease.Release()
		fail(instanceSendFailure(recorder, err))
		return nil, nil, backend.Upstream{}, false
	}
	if expected, bound := affinity.Expected(req.Context); bound && (expected.RunID != inst.ID || !backend.SameEndpoint(expected.Endpoint, base)) {
		lease.Release()
		fail(httperr.Error{Status: http.StatusConflict, Type: "invalid_request_error", Code: "response_origin_unavailable", Message: "The response's originating run is no longer available"})
		return nil, nil, backend.Upstream{}, false
	}
	affinity.Observe(req.Context, affinity.Origin{ProviderKey: inst.Provider, RunID: inst.ID, Endpoint: base, Model: wire.ModelFromBody(req.OriginalBody)})
	return lease, target, up, true
}

// sendToInstance applies the lease's first-byte and idle deadlines when the request streams.
func (s *Server) sendToInstance(req instanceRequest, lease *instance.Lease, body []byte, target *url.URL, up backend.Upstream) (*http.Response, error) {
	ctx := lease.Context()
	var wrap func(io.ReadCloser) io.ReadCloser
	if wire.StreamFromBody(body) {
		ctx, wrap = lease.WatchStream(ctx)
	}
	proxyReq, err := http.NewRequestWithContext(ctx, req.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	proxyReq.Header = upstreamHeaders(req.Context, req.Header, up)
	// Engine servers refuse a JSON body sent without its type; a multipart body always names its own.
	if proxyReq.Header.Get("Content-Type") == "" {
		proxyReq.Header.Set("Content-Type", "application/json")
	}
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		proxyReq.Header.Del(header)
	}
	resp, err := s.httpStreamingClient.Do(proxyReq)
	if err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, instance.ErrFirstByteTimeout) {
			return nil, cause
		}
		return nil, err
	}
	if wrap != nil {
		resp.Body = wrap(resp.Body)
	}
	return resp, nil
}

func instanceSendFailure(recorder *llm.InferenceRecorder, err error) httperr.Error {
	failure := httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Message: "failed to connect to model instance", Code: "instance_unavailable"}
	errType := "backend_unreachable"
	if errors.Is(err, instance.ErrFirstByteTimeout) {
		failure.Status, failure.Code, failure.Message = http.StatusGatewayTimeout, string(httperr.CodeUpstreamTimeout), "The model instance did not begin responding in time."
		errType = failure.Code
	}
	recordWireError(recorder, errType, err)
	return failure
}

func (s *Server) proxyToInstance(w http.ResponseWriter, req *http.Request, runRequest instanceRequest, inst *instance.Instance, quiet bool) {
	model, _ := req.Context().Value(CtxKeyModel).(string)
	ctx, attempt := llm.StartAttempt(req.Context(), inst.Provider, model)
	defer attempt.End()
	req = req.WithContext(ctx)
	runRequest.Context = ctx
	recorder := requestRecorder(req.Context(), model, inst.Provider)
	lease, target, up, ok := s.openRun(runRequest, inst, recorder, func(e httperr.Error) {
		attempt.SetError(e.Code, nil)
		failBeforeBackend(w, req, runRequest.OriginalBody, e)
	})
	if !ok {
		return
	}
	defer lease.Release()
	attempt.SetTarget(target.String())
	recorder.SetServer(target.Hostname(), portFromURL(target))

	inflightToken := obsproxy.IncInflight(req.Context(),
		string(obsgenai.OperationName(req.URL.Path)),
		inst.Provider, model, target.Hostname())
	defer obsproxy.DecInflight(req.Context(), inflightToken)

	body, restoreModel := s.instanceRequest(runRequest, inst)
	recorder.SetUpstreamRequestData(body)
	if !quiet {
		slog.Info("Forwarding request to instance", "id", inst.ID, "target", target.Host, "path", req.URL.Path)
	}
	resp, err := s.sendToInstance(runRequest, lease, body, target, up) //nolint:contextcheck // The lease derives its context from runRequest.
	if err != nil {
		attempt.SetError("transport_failure", err)
		slog.Error("Proxy error for instance", "id", inst.ID, "error", err)
		writeError(w, req, instanceSendFailure(recorder, err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	attempt.SetStatus(resp.StatusCode)
	if !quiet {
		utils.LogDebugf("Proxy Response: Status=%d, ContentType=%s, ContentLength=%d, TransferEncoding=%v",
			resp.StatusCode, resp.Header.Get("Content-Type"), resp.ContentLength, resp.TransferEncoding)
	}

	mergeUpstreamHeaders(w.Header(), resp.Header)
	stripUpstreamIdentifyingHeaders(w.Header())
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set(constants.HeaderServingProvider, inst.Provider)
	servingNode := s.GetNodename()
	w.Header().Set(constants.HeaderServingNode, servingNode)
	if resp.StatusCode >= 400 {
		wire.RecordPreByteCopyErrorFromStatus(recorder, resp.StatusCode)
		writeRewrittenError(w, req, resp, inst.FailureClassifier, func() bool { return protocol.HasImages(body) })
		return
	}

	meta := s.localRoutingMetadata(req, inst, servingNode, restoreModel)
	meta.SuppressUsageFrame = suppressUsageFrameFromContext(req.Context())
	if !isStreamingResponse(resp) {
		result := wire.RelayBuffered(w, resp, normalizedBody(s.inference.normalizers, inst.Provider, meta, recorder), recorder)
		result.ObserveAttempt(attempt)
		failBrokenBody(w, req, result)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	result := streamWithUsageInject(req, w, resp, s.inference.normalizers, inst.Provider, meta, recorder)
	result.ObserveAttempt(attempt)
	if result.Outcome == wire.ClientGone {
		panic(http.ErrAbortHandler)
	}
}

// failBeforeBackend reports a failure that happens before any backend
// byte. A streaming request whose dialect streams failures in band gets
// it inside a fresh 200 stream (a bare status leaves OpenWebUI stuck);
// everything else gets the status.
func failBeforeBackend(w http.ResponseWriter, req *http.Request, body []byte, e httperr.Error) {
	d := dialectOf(req)
	if streamer, inBand := d.(httperr.InBandStreamer); inBand && wire.StreamFromBody(body) {
		openEventStream(w)
		streamer.StreamError(w, e)
		return
	}
	d.WriteError(w, req, e)
}

// openEventStream commits the response to 200 text/event-stream.
func openEventStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}

// previewProviderError uses the target node's inventory to select one recovery action.
func previewProviderError(mgr *prov_apps.ProviderAppManager, name string) error {
	name = utils.NormalizeAppType(name)
	provider, ok := mgr.AppsConfig().LookupApp(name)
	if !ok {
		return newProblemError(http.StatusBadRequest, "Bad Request", fmt.Sprintf("provider %q is not configured on this node; choose a configured provider from GET /zzrouter/v1/providers or configure one from /zzrouter/v1/providers/catalog", name))
	}
	if provider.IsEnabled() {
		return nil
	}
	next := fmt.Sprintf("install it with POST /zzrouter/v1/providers/%s/install", name)
	for _, info := range mgr.LocalInfo("") {
		if info.Key == name && info.State.IsInstalled() {
			next = fmt.Sprintf("enable it with PATCH /zzrouter/v1/providers/%s with enabled=true", name)
			break
		}
	}
	return newProblemError(http.StatusBadRequest, "Bad Request", fmt.Sprintf("provider %q is configured but disabled on this node; %s, then retry", name, next))
}

// instanceRequest prepares the body and response model name for direct and fallback run dispatch.
// Every path that posts to an instance uses it, so what an engine receives cannot differ by path.
// Defaults use the body model before it is replaced with the engine's own token.
// The answer uses the caller's decorated model name when present, or the body model otherwise.
// A nil body means there is no prepared payload to rewrite or send.
func (s *Server) instanceRequest(req instanceRequest, inst *instance.Instance) (body []byte, answerAs string) {
	clientModel := wire.ModelFromBody(req.OriginalBody)
	if len(req.OriginalBody) > 0 {
		body = wire.SetDefaultFields(req.OriginalBody, s.requestDefaults(req.Context, inst.Provider, clientModel, req.Path))
		if clientModel != "" {
			body = wire.SetModelIfPresent(body, s.providers.appMgr.RequestModel(inst, clientModel))
		}
	}
	answerAs = req.ClientModel
	if answerAs == "" {
		answerAs = clientModel
	}
	return body, answerAs
}

// forwardToRun relays a native request to a leased local run, with the run's body at the run's target.
func (s *Server) forwardToRun(w http.ResponseWriter, req *http.Request, body []byte, inst *instance.Instance) {
	model, _ := req.Context().Value(CtxKeyModel).(string)
	runRequest := newInstanceRequest(req, body)
	lease, target, up, ok := s.openRun(runRequest, inst, requestRecorder(req.Context(), model, inst.Provider), func(e httperr.Error) {
		failBeforeBackend(w, req, runRequest.OriginalBody, e)
	})
	if !ok {
		return
	}
	defer lease.Release()
	req = req.WithContext(lease.Context()) //nolint:contextcheck // The lease adds run cancellation to the request context.
	runRequest.Context = req.Context()
	body, _ = s.instanceRequest(runRequest, inst)
	s.proxy.ForwardToBackend(w, req, inst.Provider, target.String(), up, body)
}

// hasRuns reports whether provider serves its models through runs; one without is forwarded to as it is.
func (s *Server) hasRuns(provider string) bool {
	svc, ok := s.providerConfig(provider)
	return ok && prov_apps.Lifecycle(svc) != prov_apps.LifecycleNone
}

// modelInstance preserves direct target identity; groups and affinity still resolve their aliases.
func (s *Server) modelInstance(resolved *resolver.Resolved, endpoint string) (*instance.Instance, bool) {
	if s.providers.appMgr == nil {
		return nil, false
	}
	if resolved.RequestModel != "" {
		return s.providers.appMgr.GetInstanceByCanonicalModel(resolved.Provider, resolved.ModelName, prov_apps.Endpoint(endpoint))
	}
	return s.providers.appMgr.GetInstanceByModel(resolved.Provider, resolved.ModelName, prov_apps.Endpoint(endpoint))
}
