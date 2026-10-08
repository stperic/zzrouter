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
	"net/http/httputil"
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
	cm, err := s.LookupModel(context.Background(), model)
	if err != nil || cm == nil || cm.Name == "" {
		return model
	}
	if cm.VariantOf != "" && s.variantSharesBase(cm) {
		// Served by its base's process, the way a SourceID alias is. One
		// level only: a base is weights, never another variant.
		if b, err := s.LookupModel(context.Background(), cm.VariantOf); err == nil && b != nil && b.Name != "" && b.VariantOf == "" {
			return b.Name
		}
		return cm.VariantOf
	}
	return cm.Name
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

// variantSharesBase reports whether a variant resolves to the same launch
// as its base on this node, so its base's process can serve it. The
// decision is this node's: node tiers can split the two on one node and
// not on another. See docs/plan_model_templates_and_variants.md.
func (s *Server) variantSharesBase(v *cache.CachedModel) bool {
	sc, ok := s.providerConfig(v.Provider)
	return ok && sc.SameLaunch(s.node.Nodename(), v.Name, v.VariantOf)
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

// forwardToInstance forwards request to a running instance via reverse proxy
// Handles both local and remote instances using getInstanceBackendNode
// proxyToInstance is the shared implementation for forwarding requests to running instances.
// When quiet is true, request/response logging is suppressed (used for the fast path).
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
	// Read out here rather than inside ModifyResponse: whether the caller
	// asked to see usage is a property of the request, and the response
	// closure has no business reaching back into the inbound context for
	// it.
	suppressUsage := suppressUsageFrameFromContext(req.Context())

	// Create reverse proxy
	// FlushInterval = -1 flushes immediately after every write to the client.
	// Required for SSE (text/event-stream) and chunked JSON streaming.
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targetURL)
			pr.Out.Host = targetURL.Host
			// An engine zzRouter launched takes no credential: the caller's
			// key is for zzRouter, and the engine must not be able to log it.
			scrubForUpstream(pr.Out.Header, backend.Engine())
		},
		FlushInterval: -1,
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		if !quiet {
			utils.LogDebugf("Proxy Response: Status=%d, ContentType=%s, ContentLength=%d, TransferEncoding=%v",
				resp.StatusCode, resp.Header.Get("Content-Type"), resp.ContentLength, resp.TransferEncoding)
		}

		resp.Header.Set("X-Accel-Buffering", "no")
		// Same normalization ForwardToBackend does: drop the engine's own
		// identity before stamping ours. A local instance was answering
		// with the engine's Server header and no node at all, so the one
		// header agents use to confirm where a request ran was missing on
		// exactly the path that serves local models.
		stripUpstreamIdentifyingHeaders(resp.Header)
		resp.Header.Set(constants.HeaderServingProvider, inst.Provider)
		// Translate the loopback sentinel to the real coord hostname so
		// agents see a stable cluster identity. For genuinely-remote
		// instances the field already carries the worker node name.
		servingNode := getInstanceBackendNode(inst)
		if servingNode == constants.Localhost {
			servingNode = s.GetNodename()
		}
		resp.Header.Set(constants.HeaderServingNode, servingNode)

		// Backend errors: rewrite to the surface-appropriate envelope
		// before the reverse proxy flushes to the client. /api/*
		// (Ollama-native) keeps the upstream's flat {"error":"..."}
		// shape; /v1/* (OpenAI-compat) gets the closed-vocab normalizer.
		// applyRewrittenError handles the response-mutation idiom that
		// httputil.ReverseProxy.ModifyResponse requires.
		if resp.StatusCode >= 400 {
			applyRewrittenError(req, resp)
			return nil
		}

		if resp.Header.Get("Content-Type") == "text/event-stream" {
			resp.Header.Set("Cache-Control", "no-cache")
			resp.Header.Set("Connection", "keep-alive")
		}
		if len(resp.TransferEncoding) > 0 {
			resp.Header.Set("Cache-Control", "no-cache")
			resp.Header.Set("Connection", "keep-alive")
			if !quiet {
				utils.LogDebugf("Detected chunked transfer encoding")
			}
		}

		// Local providers have no marginal cost — surface that via the
		// cost-truth contract's "zzrouter, 0" pair rather than omitting
		// the cost block (which would be ambiguous with "we forgot to
		// record"). Set on the meta directly so it can't get clobbered
		// by recordProxyMetrics' SetExtendedUsage(_,_,0,"") overwrite
		// during captureNonStreamingMetrics.
		meta := s.localRoutingMetadata(req, inst, servingNode, restoreModel)
		meta.SuppressUsageFrame = suppressUsage

		if !isStreamingResponse(resp) {
			captureNonStreamingMetrics(resp, recorder)
			if normalize := s.inference.normalizers.Body(inst.Provider); normalize != nil {
				if err := applyBodyNormalizer(resp, normalize); err != nil && !quiet {
					utils.LogDebugf("Body normalizer failed for %s: %v", inst.Provider, err)
				}
			}
			// Inject the root zzrouter block. Local-provider responses
			// end up here without going through
			// ProxyClient.ForwardToBackend, so this is the inject point
			// that gives them parity with cloud-routed inference
			// responses.
			if body, err := io.ReadAll(resp.Body); err == nil {
				_ = resp.Body.Close()
				body = wire.MaybeInjectRoutingMetadata(body, meta, recorder)
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
				resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			}
		} else {
			// Meter first, then transform: the injection closure reads the
			// live recorder, so it has to run after the chunk it stamps has
			// been observed. Unconditional, because a stream with no
			// transform still has to be recorded — that asymmetry is why
			// local streaming inference never reached the inference log.
			resp.Body = wire.NewMeteringReader(resp.Body, recorder)
			if transform := s.localStreamTransform(meta, inst.Provider, recorder); transform != nil {
				resp.Body = newNormalizingReader(resp.Body, transform)
				// The rewrite changes the body's length, so the engine's
				// declared one would cut the client off mid-stream.
				resp.Header.Del("Content-Length")
				resp.ContentLength = -1
			}
		}
		return nil
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		slog.Error("Proxy error for instance", "id", inst.ID, "error", err)
		writeError(w, req, httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Message: "failed to connect to model instance", Code: "instance_unavailable"})
	}

	if !quiet {
		slog.Info("Forwarding request to instance", "id", inst.ID, "instance_url", instanceURL, "path", req.URL.Path)
	}
	proxy.ServeHTTP(w, req)
	if !quiet {
		slog.Info("Request forwarded successfully to instance", "id", inst.ID)
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
