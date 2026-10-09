// package server provides HTTP handlers for the zzrouter host server.
// Provider discovery and helper functions

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	obsgenai "github.com/stperic/zzrouter/pkg/observability/genai"
	obsproxy "github.com/stperic/zzrouter/pkg/observability/proxy"
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
	replicas, err := s.model.Cache.LookupReplicas(context.Background(), model, "", "")
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

// applyRequestDefaults fills in the request-time defaults for the model
// the request addressed and leaves every field the client sent as sent.
// See requestDefaults.
func (s *Server) applyRequestDefaults(ctx context.Context, provider, model, path string, body []byte) []byte {
	return wire.SetDefaultFields(body, s.requestDefaults(ctx, provider, model, path))
}

// requestDefaults is the request-body defaults the provider's config
// resolves for model (its request blocks: model defaults, then the
// model's own, then a variant's), on a route that generates text; other
// routes (embeddings, rerank, audio) get none, since an engine may refuse
// sampling fields there. The addressed model keys the lookup, not the
// serving instance's, so a variant served by its base's process still
// gets its own defaults.
//
// A body reaches an engine through instanceRequest (every post to an
// instance) or, for a model group, the fallback chain's per-replica
// rewrite, which picks its replica itself; both apply these.
// Setting only absent fields makes a second application on the worker of
// a coordinator-routed request a no-op.
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

// findCapableProvider finds a provider that can handle the given model.
// Returns (provider, canonicalName, error). canonicalName is the cache's
// indexByName key for the resolved model — it differs from modelName when
// the caller addressed the model via SourceID alias (e.g. HF repo path).
// Downstream callers must use canonicalName for instance lookups so the
// existing-instance fast path matches across alias forms.
//
// LookupModel handles worker vs coordinator transparently and blocks on
// one populate when the cache is invalid, so post-pull inference races
// resolve correctly.
func (s *Server) findCapableProvider(modelName string) (string, string, error) {
	ctx := context.Background()
	cachedModel, err := s.LookupModel(ctx, modelName)
	if err != nil {
		var notFound cache.ErrModelNotFound
		if errors.As(err, &notFound) {
			// Hint at the cloud-deployment on-ramp — agents often pass a
			// catalog ID (e.g. "openai/gpt-4o") expecting it to work
			// without a prior registration step. The same hint already
			// fires from /runs/ensure for cloud-only providers.
			return "", "", fmt.Errorf(
				"model %q not found in registry; if this is a cloud-served model, register it via POST /zzrouter/v1/deployments first",
				modelName)
		}
		return "", "", err
	}

	canonical := cachedModel.Name
	if canonical == "" {
		canonical = modelName
	}

	// Check if model already has assigned provider
	if cachedModel.Provider != "" {
		utils.LogInfof("Model '%s' (canonical=%q) assigned to provider '%s'", modelName, canonical, cachedModel.Provider)
		return cachedModel.Provider, canonical, nil
	}

	// Auto-assign based on format, falling back to tags when format is unknown
	modelFormat := cachedModel.Format
	if modelFormat == "" {
		modelFormat = modelregistry.DetectFormatFromName(modelName)
	}
	if modelFormat == "" {
		if tags := extractTagsFromExtra(cachedModel.Extra); len(tags) > 0 {
			modelFormat = modelregistry.DetectFormatFromTags(tags)
		}
	}
	utils.LogInfof("Model '%s' has format '%s', finding capable provider...", modelName, modelFormat)

	provider, _ := modelregistry.AutoAssignProvider(modelName, modelFormat, s.appsConfig)
	if provider == "" {
		return "", canonical, fmt.Errorf("no enabled provider supports format '%s' (model: %s)", modelFormat, modelName)
	}

	return provider, canonical, nil
}

func (s *Server) proxyToInstance(w http.ResponseWriter, req *http.Request, inst *instance.Instance, quiet bool) {
	// Concurrency gating: acquire a slot before proxying
	if err := inst.AcquireConcurrency(req.Context()); err != nil {
		if errors.Is(err, instance.ErrAtCapacity) {
			failBeforeBackend(w, req, httperr.Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error",
				Message: capacityErrorMessage(inst), Code: "capacity_exceeded"})
			return
		}
		// Context cancelled (client disconnect)
		slog.Debug("Client disconnected while waiting for concurrency slot", "instance", inst.ID)
		return
	}
	defer inst.ReleaseConcurrency()

	// Record activity for local instances, applying any per-request hints
	// parsed at the handler edge (see CtxKeyRequestHints).
	if getInstanceBackendNode(inst) == constants.Localhost {
		ov, _ := req.Context().Value(CtxKeyRequestHints).(*keepalive.Override)
		if err := s.providers.appMgr.Instances().UpdateActivity(inst.ID, ov); err != nil && !quiet {
			utils.LogErrorf("Warning: Failed to record activity for instance %s: %v", inst.ID, err)
		}
	}

	// Build instance URL (handles local and remote instances)
	backendNode := getInstanceBackendNode(inst)
	instanceURL := fmt.Sprintf("http://%s:%d", backendNode, inst.Port)
	targetURL, err := url.Parse(instanceURL)
	if err != nil {
		writeError(w, req, httperr.Error{Status: http.StatusInternalServerError, Type: "server_error", Message: "invalid instance URL", Code: "invalid_instance_url"})
		return
	}

	model, _ := req.Context().Value(CtxKeyModel).(string)
	recorder := requestRecorder(req.Context(), model, inst.Provider)
	recorder.SetServer(backendNode, inst.Port)

	inflightToken := obsproxy.IncInflight(req.Context(),
		string(obsgenai.OperationName(req.URL.Path)),
		inst.Provider, model, backendNode)
	defer obsproxy.DecInflight(req.Context(), inflightToken)

	body, restoreModel := s.instanceRequest(req, inst)
	if body != nil {
		recorder.SetUpstreamRequestData(body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}

	proxyReq := req.Clone(req.Context())
	proxyReq.URL.Scheme = targetURL.Scheme
	proxyReq.URL.Host = targetURL.Host
	proxyReq.Host = targetURL.Host
	proxyReq.RequestURI = ""
	proxyReq.Close = false
	proxyReq.Header = upstreamHeaders(req.Header, backend.Engine())
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		proxyReq.Header.Del(header)
	}
	if !quiet {
		slog.Info("Forwarding request to instance", "id", inst.ID, "instance_url", instanceURL, "path", req.URL.Path)
	}
	resp, err := s.httpStreamingClient.Do(proxyReq)
	if err != nil {
		slog.Error("Proxy error for instance", "id", inst.ID, "error", err)
		recordWireError(recorder, "backend_unreachable", err)
		writeError(w, req, httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Message: "failed to connect to model instance", Code: "instance_unavailable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if !quiet {
		utils.LogDebugf("Proxy Response: Status=%d, ContentType=%s, ContentLength=%d, TransferEncoding=%v",
			resp.StatusCode, resp.Header.Get("Content-Type"), resp.ContentLength, resp.TransferEncoding)
	}

	mergeUpstreamHeaders(w.Header(), resp.Header)
	stripUpstreamIdentifyingHeaders(w.Header())
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set(constants.HeaderServingProvider, inst.Provider)
	servingNode := backendNode
	if servingNode == constants.Localhost {
		servingNode = s.GetNodename()
	}
	w.Header().Set(constants.HeaderServingNode, servingNode)
	if resp.StatusCode >= 400 {
		wire.RecordPreByteCopyErrorFromStatus(recorder, resp.StatusCode)
		writeRewrittenError(w, req, resp, inst.FailureClassifier)
		return
	}

	meta := s.localRoutingMetadata(req, inst, servingNode, restoreModel)
	meta.SuppressUsageFrame = suppressUsageFrameFromContext(req.Context())
	if !isStreamingResponse(resp) {
		failBrokenBody(w, req, wire.RelayBuffered(w, resp, normalizedBody(s.inference.normalizers, inst.Provider, meta, recorder), recorder))
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	result := streamWithUsageInject(req, w, resp, s.inference.normalizers, inst.Provider, meta, recorder)
	if result.Outcome == wire.ClientGone {
		panic(http.ErrAbortHandler)
	}
}

// capacityRetryAfterSeconds is the Retry-After for a full instance: slots
// free as soon as an in-flight request finishes.
const capacityRetryAfterSeconds = 5

// failBeforeBackend reports a failure that happens before any backend
// byte. A streaming request whose dialect streams failures in band gets
// it inside a fresh 200 stream (a bare status leaves OpenWebUI stuck);
// everything else gets the status with the capacity retry delay.
func failBeforeBackend(w http.ResponseWriter, req *http.Request, e httperr.Error) {
	d := dialectOf(req)
	if streamer, inBand := d.(httperr.InBandStreamer); inBand && isStreamingRequest(req) {
		openEventStream(w)
		streamer.StreamError(w, e)
		return
	}
	w.Header().Set("Retry-After", strconv.Itoa(capacityRetryAfterSeconds))
	d.WriteError(w, req, e)
}

// openEventStream commits the response to 200 text/event-stream.
func openEventStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}

// isStreamingRequest checks if the original request body has "stream": true.
func isStreamingRequest(req *http.Request) bool {
	body, ok := req.Context().Value(CtxKeyOriginalBody).([]byte)
	if !ok {
		return false
	}
	var r struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &r) == nil {
		return r.Stream
	}
	return false
}

// extractTagsFromExtra extracts HuggingFace tags from model Extra metadata.
// Tags are stored at Extra["huggingface"]["tags"] as []any (string elements).
func extractTagsFromExtra(extra map[string]any) []string {
	if extra == nil {
		return nil
	}
	hf, ok := extra["huggingface"].(map[string]any)
	if !ok {
		return nil
	}
	rawTags, ok := hf["tags"].([]any)
	if !ok {
		return nil
	}
	tags := make([]string, 0, len(rawTags))
	for _, t := range rawTags {
		if s, ok := t.(string); ok {
			tags = append(tags, s)
		}
	}
	return tags
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

// isProviderEnabledInConfig checks if provider is enabled in provider config
func isProviderEnabledInConfig(appsConfig *pkgConfig.AppsConfig, providerType string) bool {
	normalizedType := utils.NormalizeAppType(providerType)
	providerConfig, exists := appsConfig.LookupApp(normalizedType)
	return exists && providerConfig.IsEnabled()
}

// instanceRequest is what an instance is sent for this request, and the
// model name to answer in. Every path that posts a request to an instance
// uses it, so what an engine receives cannot differ by path.
//
// The body is the client's, with the addressed model's request defaults,
// then the engine's own token for the model (see instance.WireModel):
// zzRouter speaks that token inbound and puts the client's name back on
// the way out, so no engine's naming reaches a client. Both steps are
// no-ops when there is nothing to add and the engine honors the client's
// name. The answer name is the id the caller used: the decorated one when
// a @node hint was stripped, otherwise the body's. A request with no
// stashed body gets a nil body and is sent as it came.
func (s *Server) instanceRequest(req *http.Request, inst *instance.Instance) (body []byte, answerAs string) {
	clientModel := ""
	if original, ok := req.Context().Value(CtxKeyOriginalBody).([]byte); ok && len(original) > 0 {
		clientModel = wire.ModelFromBody(original)
		body = s.applyRequestDefaults(req.Context(), inst.Provider, clientModel, req.URL.Path, original)
		if translateModel(inst, clientModel) {
			body = wire.SetModelIfPresent(body, inst.WireModel)
		}
	}
	answerAs = clientModelFromContext(req.Context())
	if answerAs == "" {
		answerAs = clientModel
	}
	return body, answerAs
}

// translateModel reports whether this instance's engine keys on a token
// other than the one the client sent, i.e. whether the model name has to
// be rewritten on the way in.
//
// The outbound direction is not gated on this. WireModel is only set for
// instances zzRouter launches, so an external engine leaves it empty --
// and an external engine still answers with its own token. Ollama echoes
// its registry name, which drops the @node suffix the catalog advertises,
// so the id in the response no longer matches any catalog entry. The
// response is restored to whatever the client sent whenever that is
// known; which concrete replica served it is reported by the zzrouter
// block, not by overwriting the client's own handle for the model.
func translateModel(inst *instance.Instance, clientModel string) bool {
	return clientModel != "" && inst.WireModel != "" && inst.WireModel != clientModel
}

// modelInstance keeps provider selection intact when reusing an endpoint's run.
func (s *Server) modelInstance(model, endpoint, provider string) (*instance.Instance, bool) {
	if s.providers.appMgr == nil {
		return nil, false
	}
	inst, found := s.providers.appMgr.Instances().GetByModelEndpoint(model, endpoint)
	if found && provider != "" && utils.NormalizeAppType(inst.Provider) != utils.NormalizeAppType(provider) {
		return nil, false
	}
	return inst, found
}
