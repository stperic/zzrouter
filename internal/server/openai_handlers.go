// OpenAI compatibility handlers for the /v1/* surface.
//
// Wire format is dictated by the OpenAI API spec: success responses use the
// typed shapes in openai_types.go and errors use the OpenAI error envelope
// (utils.OpenAIError) via OpenAIInvalidRequest / OpenAINodeError.
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
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	obsgenai "github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/proxy"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stperic/zzrouter/pkg/utils"
)

const handshakeModelID = "zzrouter"

// OpenAIModelHandlers groups OpenAI model catalog handlers (/v1/models, /v1/model/info).
type OpenAIModelHandlers struct {
	modelService    *ModelService
	appsConfig      func() *pkgConfig.AppsConfig
	groups          *modelgroup.GroupStore
	adapter         ProtocolAdapter
	respondInternal func(*gin.Context, string) // openai responder Internal method
	// runtimeLookup returns the local-node runtime view for (model,endpoint)
	// or nil when the model isn't hot. Local-only by design — keeps
	// /v1/models cheap; cluster-wide rollup happens at the cluster layer.
	runtimeLookup func(model, endpoint string) *OpenAIModelRuntime
	localNode     func() string
}

// OpenAIInferenceHandlers groups OpenAI inference handlers (/v1/chat/completions, etc.).
type OpenAIInferenceHandlers struct {
	// Endpoint string is the per-request endpoint label ("chat",
	// "embeddings", "reranking"); flows down to launch keying so
	// per-endpoint instances coexist for one model.
	dispatchWithRecorder func(c *gin.Context, model string, body []byte, recorder *llm.InferenceRecorder, span trace.Span, endpoint string)
	dispatch             func(c *gin.Context, model string, body []byte, endpoint string)
	maxUploadBytes       func() int64
	forwardToDefault     func(*gin.Context, []byte)
}

// readRequestBody reads the whole request body, answering in the
// request's dialect when it cannot: 413 for a body over the engine-wide
// size cap, which a client must not retry unchanged, and 400 otherwise.
func readRequestBody(c *gin.Context) ([]byte, bool) {
	body, err := c.GetRawData()
	if err == nil {
		return body, true
	}
	r := httperr.FromContextOr(c, defaultCompatResponder)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		r.RequestTooLarge(c, fmt.Sprintf("request body exceeds the %d-byte limit", tooLarge.Limit))
		return nil, false
	}
	r.BadRequest(c, "failed to read request body", "")
	return nil, false
}

// readOpenAIBody reads the raw request body and unmarshals it into req. On
// failure it writes an error in the route's dialect and returns
// (nil, false). The raw bytes are returned so the caller can stash them in
// the request context for the downstream proxy.
func readOpenAIBody(c *gin.Context, req any) ([]byte, bool) {
	body, ok := readRequestBody(c)
	if !ok {
		return nil, false
	}
	if err := json.Unmarshal(body, req); err != nil {
		httperr.FromContextOr(c, defaultCompatResponder).BadRequest(c, fmt.Sprintf("Invalid JSON: %v", err), "")
		return nil, false
	}
	return body, true
}

// stashInferenceContext attaches the raw body, model name, and parsed
// per-request hints (e.g. keep_alive override) to the request context so
// downstream dispatchers can forward the body and consumers can apply
// hints at the instance layer. Hints are Ollama-protocol fields that
// zzRouter also honors on the OpenAI surface as a zzRouter extension.
//
// Also surfaces the request model on gin.Context so the proxy-metric
// middleware (zz.proxy.failed.requests.metric) carries it as a label
// when a downstream 4xx/5xx fires.
func stashInferenceContext(c *gin.Context, model string, body []byte) {
	ctx := context.WithValue(c.Request.Context(), CtxKeyOriginalBody, body)
	ctx = context.WithValue(ctx, CtxKeyModel, model)
	ctx = context.WithValue(ctx, CtxKeyRequestHints, keepalive.Parse(body))
	c.Request = c.Request.WithContext(ctx)
	c.Set(proxy.CtxKeyRequestModel, model)
}

// handleChatCompletions handles POST /v1/chat/completions.
func (h *OpenAIInferenceHandlers) handleChatCompletions(c *gin.Context) {
	ctx, span := llm.StartInferenceSpan(c.Request.Context(), "", llm.RequestTypeChat, false)
	defer span.End()
	c.Request = c.Request.WithContext(ctx)

	var req OpenAIChatCompletionRequest
	body, ok := readOpenAIBody(c, &req)
	if !ok {
		llm.SetSpanErrorWithType(span, "invalid_request_error", fmt.Errorf("bind failed"))
		return
	}

	if req.Model == "" {
		param := "model"
		OpenAIInvalidRequest(c, "Model field is required", &param)
		return
	}
	if len(req.Messages) == 0 {
		param := "messages"
		OpenAIInvalidRequest(c, "At least one message is required", &param)
		return
	}
	// Roles are not validated — the backend provider defines which roles it
	// supports (e.g. "tool", "developer", "function"). Empty role is
	// rejected as an obvious client error. Content follows the OpenAI spec:
	// required for user/system/tool/developer messages, nullable for
	// assistant messages (which may be tool-only turns with tool_calls
	// instead of content).
	for i, msg := range req.Messages {
		if msg.Role == "" {
			param := fmt.Sprintf("messages[%d].role", i)
			OpenAIInvalidRequest(c, fmt.Sprintf("Message role cannot be empty in message %d", i), &param)
			return
		}
		contentIsNull := len(msg.Content) == 0 || string(msg.Content) == "null"
		if contentIsNull && msg.Role != "assistant" {
			param := fmt.Sprintf("messages[%d].content", i)
			OpenAIInvalidRequest(c, fmt.Sprintf("Message content cannot be empty in message %d", i), &param)
			return
		}
	}

	span.SetAttributes(llm.ModelName(req.Model), llm.RequestStream(req.Stream))

	if req.Model == handshakeModelID {
		writeHandshakeResponse(c, req.Stream)
		llm.SetSpanOK(span)
		return
	}

	stashInferenceContext(c, req.Model, body)
	recorder := newChatRecorder(c, req.Model, body, req.Stream)
	h.dispatchWithRecorder(c, req.Model, body, recorder, span, "chat")
}

// handleCompletions handles POST /v1/completions (legacy).
func (h *OpenAIInferenceHandlers) handleCompletions(c *gin.Context) {
	ctx, span := llm.StartInferenceSpan(c.Request.Context(), "", llm.RequestTypeCompletion, false)
	defer span.End()
	c.Request = c.Request.WithContext(ctx)

	var req OpenAICompletionRequest
	body, ok := readOpenAIBody(c, &req)
	if !ok {
		llm.SetSpanErrorWithType(span, "invalid_request_error", fmt.Errorf("bind failed"))
		return
	}

	if req.Model == "" {
		param := "model"
		OpenAIInvalidRequest(c, "Model field is required", &param)
		return
	}
	if req.Prompt == "" {
		param := "prompt"
		OpenAIInvalidRequest(c, "Prompt field is required", &param)
		return
	}

	span.SetAttributes(llm.ModelName(req.Model))

	stashInferenceContext(c, req.Model, body)
	recorder := newCompletionRecorder(c, req.Model, body)
	h.dispatchWithRecorder(c, req.Model, body, recorder, span, "chat")
}

// handleEmbeddings handles POST /v1/embeddings.
func (h *OpenAIInferenceHandlers) handleEmbeddings(c *gin.Context) {
	ctx, span := llm.StartInferenceSpan(c.Request.Context(), "", llm.RequestTypeEmbedding, false)
	defer span.End()
	c.Request = c.Request.WithContext(ctx)

	var req OpenAIEmbeddingsRequest
	body, ok := readOpenAIBody(c, &req)
	if !ok {
		llm.SetSpanErrorWithType(span, "invalid_request_error", fmt.Errorf("bind failed"))
		return
	}
	if req.Model == "" {
		param := "model"
		OpenAIInvalidRequest(c, "Model field is required", &param)
		return
	}
	if req.Input == nil {
		param := "input"
		OpenAIInvalidRequest(c, "Input field is required", &param)
		return
	}

	span.SetAttributes(llm.ModelName(req.Model))

	stashInferenceContext(c, req.Model, body)
	recorder := newEmbeddingsRecorder(c, req.Model, body)
	h.dispatchWithRecorder(c, req.Model, body, recorder, span, "embeddings")
}

// handleImagesGenerations proxies /v1/images/generations to the backend.
func (h *OpenAIInferenceHandlers) handleImagesGenerations(c *gin.Context) {
	h.routeByModelField(c, "chat")
}

// handleModerations proxies /v1/moderations to the backend.
func (h *OpenAIInferenceHandlers) handleModerations(c *gin.Context) {
	h.routeByModelField(c, "chat")
}

// handleAudioSpeech proxies /v1/audio/speech (text-to-speech) to the backend.
func (h *OpenAIInferenceHandlers) handleAudioSpeech(c *gin.Context) {
	h.routeByModelField(c, "chat")
}

// multipartRouteByModel is the shared handler for multipart /v1/* endpoints
// (audio transcription/translation, image edits/variations). It buffers the
// request body once, extracts the `model` form field when present, and either
// routes by model (via resolveAndDispatchSimple) or forwards to the configured
// default backend when the field is absent.
//
// The body is stashed on the request context so the downstream proxyToBackend
// forwards the exact bytes read here — this is critical because multipart
// boundaries cannot be regenerated. The original Content-Type header, including
// the boundary parameter, survives through upstreamHeaders unchanged.
//
// Note: this routing path only yields usable results when the resolved
// provider is in service or cloud mode. On-demand provider instances
// (vLLM/llama.cpp/MLX local runs) hardcode Content-Type: application/json in
// their forwarder and would destroy the multipart payload. Operators should
// route audio and image endpoints to a cloud-mode provider that speaks the
// matching OpenAI surface.
func (h *OpenAIInferenceHandlers) multipartRouteByModel(c *gin.Context) {
	body, model, ok := readOpenAIMultipartBody(c, h.maxUploadBytes())
	if !ok {
		return
	}
	if model == "" {
		h.forwardToDefault(c, body)
		return
	}
	stashInferenceContext(c, model, body)
	h.dispatch(c, model, body, "chat")
}

// routeByModelField implements the shared "read body, extract model, dispatch"
// path for endpoints zzRouter doesn't parse fully — it only needs the model
// field to pick a backend, then forwards the original bytes.
func (h *OpenAIInferenceHandlers) routeByModelField(c *gin.Context, endpoint string) {
	var req OpenAIModelRoutingRequest
	body, ok := readOpenAIBody(c, &req)
	if !ok {
		return
	}
	if req.Model == "" {
		param := "model"
		OpenAIInvalidRequest(c, "Model field is required for routing", &param)
		return
	}
	stashInferenceContext(c, req.Model, body)
	h.dispatch(c, req.Model, body, endpoint)
}

// enrichRuntime populates the Endpoints + Runtime fields on a /v1/models
// entry produced by the adapter. Pure post-processing — kept separate
// from ListEntry so the adapter stays a zero-state translator.
func (h *OpenAIModelHandlers) enrichRuntime(entry any, m *cache.CachedModel) any {
	obj, ok := entry.(OpenAIModelObject)
	if !ok {
		return entry
	}
	if obj.Capabilities != nil {
		obj.Endpoints = endpointsForModel(*obj.Capabilities, m, h.appsConfig())
	}
	obj.ServedBy = deriveServedBy(m, obj.Endpoints)
	obj.Runtime = h.deriveRuntime(m, obj.Endpoints)
	return obj
}

// wireServedEndpoints are the catalog endpoints a model reaches through
// its provider's wire_endpoints rather than its own capabilities: served
// natively, or by the shim that translates to Chat Completions.
var wireServedEndpoints = []struct{ name, native, compat string }{
	{"responses", "responses", "responses_compat"},
	{"messages", "messages", "messages_compat"},
}

// endpointsForModel maps the closed capability bools onto the agent-facing
// endpoint slice so clients can branch without reading every flag, then
// adds each wire-served endpoint the model's provider declares.
func endpointsForModel(c OpenAIModelCapabilities, m *cache.CachedModel, cfg *pkgConfig.AppsConfig) []string {
	out := endpointsForCapabilities(c)
	// Both the passthrough and the shim speak chat shape, so an embedding-
	// or rerank-only model on such a provider must not advertise them.
	if c.Chat {
		for _, w := range wireServedEndpoints {
			if providerServesWire(m, cfg, w.native, w.compat) {
				out = append(out, w.name)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// endpointsForCapabilities maps the capability bools onto the endpoint
// vocabulary. /v1/responses is deliberately absent: reachability there
// depends on the serving provider's wire_endpoints, not on the model, so
// only callers that know the provider can add it.
func endpointsForCapabilities(c OpenAIModelCapabilities) []string {
	out := make([]string, 0, 5)
	if c.Chat {
		out = append(out, "chat")
	}
	if c.Completions {
		out = append(out, "completions")
	}
	if c.Embeddings {
		out = append(out, "embeddings")
	}
	if c.Rerank {
		out = append(out, "rerank")
	}
	return out
}

// providerServesWire reports whether a model's provider declares the
// native endpoint or its translation shim in wire_endpoints.
func providerServesWire(m *cache.CachedModel, cfg *pkgConfig.AppsConfig, native, compat string) bool {
	if m == nil || m.Provider == "" || cfg == nil {
		return false
	}
	svc, ok := cfg.LookupApp(m.Provider)
	if !ok {
		return false
	}
	return svc.Capabilities.SupportsWireEndpoint(native) || svc.Capabilities.SupportsWireEndpoint(compat)
}

// deriveRuntime returns the local-node hot-state for a model. Returns
// nil for cloud-backed models — the cloud upstream IS the runtime, so
// emitting a "running on macbook-pro" block would mislead agents into
// thinking there's a local instance lifecycle when there isn't (the
// served_by field carries the actual routing answer). For local
// providers, walks declared endpoints and returns the first running
// instance found (chat preferred via slice order).
func (h *OpenAIModelHandlers) deriveRuntime(m *cache.CachedModel, endpoints []string) *OpenAIModelRuntime {
	if m == nil || m.IsCloud {
		return nil
	}
	if h.runtimeLookup == nil {
		return nil
	}
	for _, ep := range endpoints {
		if rt := h.runtimeLookup(m.Name, ep); rt != nil {
			return rt
		}
	}
	return &OpenAIModelRuntime{Status: "cold", Node: m.Node}
}

// servedByCloud / servedByLocal are the closed vocabulary of the
// served_by field: whether a request for this entry is dispatched to a
// cloud provider (and may bill upstream) or to a local instance.
const (
	servedByCloud = "cloud"
	servedByLocal = "local"
)

// deriveServedBy returns "cloud" for cloud-backed models and "local"
// for everything else (on-demand providers, registry-fronted local
// models). Empty string for entries with no inference endpoints (e.g.
// deleted-but-cached aliases) so the field is omitted via omitempty.
func deriveServedBy(m *cache.CachedModel, endpoints []string) string {
	if m == nil || len(endpoints) == 0 {
		return ""
	}
	if m.IsCloud {
		return servedByCloud
	}
	return servedByLocal
}

// catalogByID indexes the catalog by the id /v1/models publishes, so a
// group can be described from the entries its replicas already produce.
// A lookup failure yields an empty index; describeGroup degrades to the
// bare entry rather than failing the request.
func (h *OpenAIModelHandlers) catalogByID(ctx context.Context) map[string]OpenAIModelObject {
	models, err := h.modelService.ListModels(ctx, &ListModelsRequest{})
	if err != nil {
		return nil
	}
	byID := make(map[string]OpenAIModelObject, len(models))
	for _, m := range models {
		obj, ok := h.enrichRuntime(h.adapter.ListEntry(m), m).(OpenAIModelObject)
		if !ok {
			continue
		}
		byID[obj.ID] = obj
		// The list also publishes a SourceID alias entry (the HF repo path
		// beside the file-stem name). A replica named by either form has to
		// resolve, or the group loses capabilities depending on which of two
		// equally-published ids its author happened to write down.
		if alias := qualifyID(m.SourceID, m); m.SourceID != "" && m.SourceID != m.Name {
			if _, taken := byID[alias]; !taken {
				byID[alias] = obj
			}
		}
	}
	return byID
}

// handleModels handles GET /v1/models. Translation to OpenAI's wire format
// is delegated to the ProtocolAdapter; this handler only orchestrates:
// canonical ModelService query → adapter translation → group-alias overlay
// → envelope serialization.
func (h *OpenAIModelHandlers) handleModels(c *gin.Context) {
	modelsResp, err := h.modelService.ListModels(c.Request.Context(), &ListModelsRequest{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, utils.NewOpenAIError(
			"api_error", "Failed to retrieve models", nil, nil,
		))
		return
	}

	// Node-qualified IDs (`name@node`, set in adapter.ListEntry) make
	// each per-node entry distinct. Dedupe on the qualified key so the
	// same model on coord and worker both surface; group-alias overlay
	// below stays keyed on bare names since groups are not node-scoped.
	entries := make([]any, 0, len(modelsResp))
	seen := make(map[string]bool, len(modelsResp))
	seenQualified := make(map[string]bool, len(modelsResp))
	for _, m := range modelsResp {
		entries = append(entries, h.enrichRuntime(h.adapter.ListEntry(m), m))
		seen[m.Name] = true
		seenQualified[qualifyID(m.Name, m)] = true

		// SourceID alias entry (e.g. "Qwen/Qwen2.5-0.5B-Instruct-GGUF"
		// alongside the file-stem name). Lets agents listing models
		// discover both forms; clients dedupe via alias_of.
		if m.SourceID != "" && m.SourceID != m.Name {
			aliasID := qualifyID(m.SourceID, m)
			if !seenQualified[aliasID] {
				alias := h.enrichRuntime(h.adapter.ListEntry(m), m)
				if obj, ok := alias.(OpenAIModelObject); ok {
					obj.ID = aliasID
					obj.AliasOf = qualifyID(m.Name, m)
					alias = obj
				}
				entries = append(entries, alias)
				seen[m.SourceID] = true
				seenQualified[aliasID] = true
			}
		}
	}

	// Group aliases aren't part of the canonical catalog — they're
	// resolver-only sugar. Overlay them here so /v1/models callers can
	// reach a group by its alias, described the same way a plain model
	// is: an agent that selects on capabilities.chat must not skip the
	// entries the router most wants it to prefer.
	if h.groups != nil {
		now := utils.Now().Unix()
		byID := make(map[string]OpenAIModelObject, len(entries))
		for _, e := range entries {
			if obj, ok := e.(OpenAIModelObject); ok {
				byID[obj.ID] = obj
			}
		}
		for name, group := range h.groups.List() {
			if seen[name] {
				continue
			}
			entries = append(entries, describeGroup(name, now, group.Replicas, byID, h.localNode()))
		}
	}

	c.JSON(http.StatusOK, h.adapter.ListEnvelope(entries))
}

// handleModelByID handles GET /v1/models/*model. A wildcard route is used so
// model names containing '/' (e.g. "deepseek/r1") resolve correctly. The
// list response decorates ids with "@node" when the same model exists on
// multiple nodes; accept that grammar as input so list→get round-trips
// cleanly.
func (h *OpenAIModelHandlers) handleModelByID(c *gin.Context) {
	rawID := strings.TrimPrefix(c.Param("model"), "/")
	modelID, nodeHint := splitOllamaAddr(rawID)

	if modelID == handshakeModelID {
		c.JSON(http.StatusOK, newOpenAIModel(handshakeModelID, utils.Now().Unix(), "zzrouter"))
		return
	}

	if h.groups != nil {
		if group := h.groups.Get(modelID); group != nil {
			// Same description the list emits — a list→get round-trip
			// that dropped capabilities would be its own trap.
			c.JSON(http.StatusOK, describeGroup(modelID, utils.Now().Unix(),
				group.Replicas, h.catalogByID(c.Request.Context()), h.localNode()))
			return
		}
	}

	matches, err := h.modelService.ListModels(c.Request.Context(), &ListModelsRequest{Model: modelID, Node: nodeHint})
	if err != nil || len(matches) == 0 {
		param := "model"
		code := "model_not_found"
		c.JSON(http.StatusNotFound, utils.NewOpenAIError(
			"invalid_request_error",
			"The model '"+rawID+"' does not exist",
			&param,
			&code,
		))
		return
	}

	// Route through the same adapter + enrichment pipeline as
	// handleModels so the single-model and list responses can never
	// drift on capabilities, endpoints, or runtime fields.
	m := matches[0]
	c.JSON(http.StatusOK, h.enrichRuntime(h.adapter.ListEntry(m), m))
}

// handleDeleteModelByID handles DELETE /v1/models/*model. Mirrors the OpenAI
// fine-tune cleanup flow: exact-match lookup, delete, return the spec's
// {id, object, deleted} envelope. Reserved routes (handshake model, model
// groups) cannot be deleted through this surface.
func (h *OpenAIModelHandlers) handleDeleteModelByID(c *gin.Context) {
	modelID := strings.TrimPrefix(c.Param("model"), "/")

	if modelID == "" {
		param := "model"
		OpenAIInvalidRequest(c, "Model ID is required", &param)
		return
	}

	if modelID == handshakeModelID || (h.groups != nil && h.groups.Get(modelID) != nil) {
		param := "model"
		code := "model_not_deletable"
		c.JSON(http.StatusBadRequest, utils.NewOpenAIError(
			"invalid_request_error",
			"The model '"+modelID+"' is a reserved route and cannot be deleted",
			&param,
			&code,
		))
		return
	}

	matches, err := h.modelService.ListModels(c.Request.Context(), &ListModelsRequest{Model: modelID})
	if err != nil || len(matches) == 0 {
		param := "model"
		code := "model_not_found"
		c.JSON(http.StatusNotFound, utils.NewOpenAIError(
			"invalid_request_error",
			"The model '"+modelID+"' does not exist",
			&param,
			&code,
		))
		return
	}

	resp, err := h.modelService.DeleteModels(c.Request.Context(), &DeleteModelsRequest{
		Pattern: matches[0].Name,
	})
	if err != nil || resp == nil || resp.Deleted == 0 {
		c.JSON(http.StatusInternalServerError, utils.NewOpenAIError(
			"api_error",
			"Failed to delete model '"+modelID+"'",
			nil,
			nil,
		))
		return
	}

	c.JSON(http.StatusOK, OpenAIModelDeleteResponse{
		ID:      matches[0].Name,
		Object:  "model",
		Deleted: true,
	})
}

func newOpenAIModel(id string, created int64, ownedBy string) OpenAIModelObject {
	return OpenAIModelObject{
		ID:      id,
		Object:  "model",
		Created: created,
		OwnedBy: ownedBy,
	}
}

// describeGroup renders a model group as a catalog entry carrying the
// capabilities a caller can rely on when dispatching to it.
//
// Capabilities are the intersection across replicas, and endpoints
// follow: a request may land on any replica, so advertising something
// only one of them can do would promise what the next request cannot
// keep. served_by reports "cloud" as soon as one replica is cloud-backed
// — a caller checking whether a call can bill upstream needs the
// pessimistic answer. Runtime is omitted: a group has no single
// instance, and hot/cold is a per-replica property.
//
// A group whose replicas are all absent from the catalog degrades to the
// bare entry it used to be rather than guessing.
func describeGroup(name string, created int64, replicas []modelgroup.Replica, byID map[string]OpenAIModelObject, localNode string) OpenAIModelObject {
	entry := newOpenAIModel(name, created, "zzrouter-route")

	var caps *OpenAIModelCapabilities
	var endpoints []string
	local := true
	for _, rep := range replicas {
		obj, ok := byID[replicaCatalogID(rep, localNode)]
		if !ok {
			// Cloud replicas keep their bare name (qualifyID skips them),
			// as does any model the catalog could not attribute to a node.
			obj, ok = byID[rep.Model]
		}
		if !ok || obj.Capabilities == nil {
			continue
		}
		if caps == nil {
			c := *obj.Capabilities
			caps, endpoints = &c, obj.Endpoints
		} else {
			intersectCapabilities(caps, *obj.Capabilities)
			endpoints = intersectStrings(endpoints, obj.Endpoints)
		}
		if obj.ServedBy == servedByCloud {
			local = false
		}
	}
	if caps == nil {
		return entry
	}

	entry.Capabilities = caps
	entry.Endpoints = endpoints
	entry.ServedBy = servedByCloud
	if local {
		entry.ServedBy = servedByLocal
	}
	return entry
}

// intersectStrings returns the members of a that also appear in b,
// preserving a's order. Returns nil rather than an empty slice so the
// omitempty field disappears instead of serializing as [].
func intersectStrings(a, b []string) []string {
	keep := make(map[string]bool, len(b))
	for _, s := range b {
		keep[s] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if keep[s] {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// replicaCatalogID is the id /v1/models publishes for a replica.
//
// Replica.Node documents "" as "local", but the catalog qualifies a
// local model as name@<hostname> — so an empty node has to be resolved
// before it can be looked up, or a hand-authored group that omits
// `node:` matches nothing and loses every capability it should have
// advertised.
func replicaCatalogID(rep modelgroup.Replica, localNode string) string {
	node := rep.Node
	if node == "" {
		node = localNode
	}
	if node == "" {
		return rep.Model
	}
	return rep.Model + "@" + node
}

// intersectCapabilities narrows dst to what dst and other both support.
// MaxContextTokens takes the smaller non-zero window for the same reason
// the bools intersect: the caller must be able to rely on it whichever
// replica serves.
func intersectCapabilities(dst *OpenAIModelCapabilities, other OpenAIModelCapabilities) {
	dst.Chat = dst.Chat && other.Chat
	dst.Completions = dst.Completions && other.Completions
	dst.Embeddings = dst.Embeddings && other.Embeddings
	dst.Rerank = dst.Rerank && other.Rerank
	dst.Tools = dst.Tools && other.Tools
	dst.Vision = dst.Vision && other.Vision
	dst.Stream = dst.Stream && other.Stream
	dst.JSONMode = dst.JSONMode && other.JSONMode
	if other.MaxContextTokens > 0 && (dst.MaxContextTokens == 0 || other.MaxContextTokens < dst.MaxContextTokens) {
		dst.MaxContextTokens = other.MaxContextTokens
	}
}

// qualifyID returns "name@node" for cluster-resident models, else
// "name". Cloud entries (m.IsCloud == true, even when m.Node is set
// to the brokering coordinator) keep the bare name — the broker is
// an implementation detail and qualifying would make the id unstable
// across coord renames.
func qualifyID(name string, m *cache.CachedModel) string {
	if m == nil || m.Node == "" || m.IsCloud {
		return name
	}
	return name + "@" + m.Node
}

// writeHandshakeResponse returns an instant OpenAI-compatible response for
// the "zzrouter" handshake model used by clients to validate connectivity
// without a real LLM backend.
func writeHandshakeResponse(c *gin.Context, stream bool) {
	const (
		id      = "chatcmpl-zzrouter"
		content = "zzRouter is connected and ready."
	)
	created := utils.Now().Unix()

	if !stream {
		c.JSON(http.StatusOK, OpenAIChatCompletionResponse{
			ID:      id,
			Object:  "chat.completion",
			Created: created,
			Model:   handshakeModelID,
			Choices: []OpenAIChatCompletionChoice{{
				Index:        0,
				Message:      OpenAIChatCompletionMessage{Role: "assistant", Content: content},
				FinishReason: "stop",
			}},
			Usage: OpenAIUsage{},
		})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	finishStop := "stop"
	writeSSEChunk(c, OpenAIChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   handshakeModelID,
		Choices: []OpenAIChatCompletionChunkChoice{{
			Index: 0,
			Delta: OpenAIChatCompletionDelta{Role: "assistant", Content: content},
		}},
	})
	writeSSEChunk(c, OpenAIChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   handshakeModelID,
		Choices: []OpenAIChatCompletionChunkChoice{{
			Index:        0,
			FinishReason: &finishStop,
		}},
	})
	_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
}

// writeSSEChunk marshals v as JSON and writes it as one SSE data frame.
func writeSSEChunk(c *gin.Context, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Info("Failed to marshal SSE chunk", "error", err)
		_, _ = c.Writer.WriteString("data: {\"error\": \"internal error\"}\n\n")
		c.Writer.Flush()
		return
	}
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", data)
	c.Writer.Flush()
}

// newChatRecorder builds and stashes an inference recorder for a chat request.
func newChatRecorder(c *gin.Context, model string, body []byte, stream bool) *llm.InferenceRecorder {
	recorder := llm.NewInferenceRecorder(c.Request.Context(), model, "")
	recorder.SetRequestType(llm.RequestTypeChat)
	recorder.SetStream(stream)
	recorder.SetRequestData(body)
	mirrorCallerIdentityToRecorder(c, recorder)
	ctx := context.WithValue(c.Request.Context(), CtxKeyInferenceRecorder, recorder)
	c.Request = c.Request.WithContext(ctx)
	return recorder
}

// newCompletionRecorder builds and stashes an inference recorder for a
// legacy completions request.
func newCompletionRecorder(c *gin.Context, model string, body []byte) *llm.InferenceRecorder {
	recorder := llm.NewInferenceRecorder(c.Request.Context(), model, "")
	recorder.SetRequestType(llm.RequestTypeCompletion)
	recorder.SetRequestData(body)
	mirrorCallerIdentityToRecorder(c, recorder)
	ctx := context.WithValue(c.Request.Context(), CtxKeyInferenceRecorder, recorder)
	c.Request = c.Request.WithContext(ctx)
	return recorder
}

// newEmbeddingsRecorder builds and stashes an inference recorder for an
// /v1/embeddings request. Sets RequestTypeEmbedding so the OTel GenAI
// emission lands on operation_name=embeddings instead of the previous
// silent skip (no recorder constructed → no metrics at all).
func newEmbeddingsRecorder(c *gin.Context, model string, body []byte) *llm.InferenceRecorder {
	recorder := llm.NewInferenceRecorder(c.Request.Context(), model, "")
	recorder.SetRequestType(llm.RequestTypeEmbedding)
	recorder.SetRequestData(body)
	mirrorCallerIdentityToRecorder(c, recorder)
	ctx := context.WithValue(c.Request.Context(), CtxKeyInferenceRecorder, recorder)
	c.Request = c.Request.WithContext(ctx)
	return recorder
}

// proxyToRemoteNode proxies a request to a worker over the cluster
// mTLS port. Workers expose /v1/* and /api/* only on their cluster
// listener — transport-level mTLS + OU=coordinator client cert IS
// the auth boundary; no application-layer header gate.
//
// Provider is passed via X-zzrouter-Provider so the worker doesn't
// re-resolve the model.
func (s *Server) proxyToRemoteNode(c *gin.Context, host, provider string, body []byte) {
	clusterURL := s.resolveNodeToClusterURL(host)
	if clusterURL == "" {
		slog.Error("Proxy target has no cluster URL", "node", host)
		writeError(c.Writer, c.Request, httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Message: "Worker cluster port not advertised", Code: "backend_unreachable"})
		return
	}
	mtlsClient := s.coordWorkerMTLSClient()
	if mtlsClient == nil {
		slog.Error("Coord mTLS client unavailable")
		writeError(c.Writer, c.Request, httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Message: "Coord mTLS dispatch unavailable", Code: "backend_unreachable"})
		return
	}
	s.proxyToWorkerURL(c, clusterURL, host, provider, body, mtlsClient)
}

// proxyToWorkerURL is the URL-resolved variant of proxyToRemoteNode —
// extracted so tests can drive the proxy half without standing up a
// full cluster registry. baseURL is the worker's cluster-port base
// (e.g. "https://192.0.2.10:9091") and client is the mTLS-equipped
// http.Client. host is the original symbolic node name used only for
// log/error context.
func (s *Server) proxyToWorkerURL(c *gin.Context, baseURL, host, provider string, body []byte, client *http.Client) {
	targetURL := baseURL + c.Request.URL.Path
	if c.Request.URL.RawQuery != "" {
		targetURL += "?" + c.Request.URL.RawQuery
	}

	utils.LogDebugf("🔀 Proxying to remote (mTLS): %s (provider=%s)", targetURL, provider)

	model, _ := c.Request.Context().Value(CtxKeyModel).(string)
	recorder := requestRecorder(c.Request.Context(), model, provider)
	recorder.SetUpstreamRequestData(body)
	var serverHost string
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		serverHost = u.Hostname()
		recorder.SetServer(serverHost, portFromURL(u))
	}

	inflightToken := proxy.IncInflight(c.Request.Context(),
		string(obsgenai.OperationName(c.Request.URL.Path)),
		provider, model, serverHost)
	defer proxy.DecInflight(c.Request.Context(), inflightToken)

	// Streaming context: cancelled if the client disconnects. The mTLS
	// client has no Timeout (DialClient sets none) so long streams run
	// to completion.
	streamCtx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	proxyReq, err := http.NewRequestWithContext(streamCtx, c.Request.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		writeError(c.Writer, c.Request, httperr.Error{Status: http.StatusInternalServerError, Type: "server_error", Message: "Failed to create proxy request", Code: "proxy_request_build_failed"})
		return
	}

	proxyReq.Header = upstreamHeaders(c.Request.Header, backend.Cluster())
	if proxyReq.Header.Get("Content-Type") == "" {
		proxyReq.Header.Set("Content-Type", "application/json")
	}
	if provider != "" {
		proxyReq.Header.Set(constants.HeaderServingProvider, provider)
	}

	resp, err := client.Do(proxyReq)
	if err != nil {
		slog.Error("Proxy error", "node", host, "error", err)
		writeError(c.Writer, c.Request, httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Message: "Failed to reach remote server", Code: "backend_unreachable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Backend errors (non-2xx) are rewritten into the request's dialect
	// before reaching the client. Errors come back as non-streaming JSON
	// regardless of whether the original request asked for SSE. The same
	// writeRewrittenError serves ProxyClient.ForwardToBackend and
	// proxyToInstance ModifyResponse, so the three sites can't drift.
	if resp.StatusCode >= 400 {
		mergeUpstreamHeaders(c.Writer.Header(), resp.Header)
		writeRewrittenError(c.Writer, c.Request, resp)
		return
	}

	// isStreamingResponse (wire.IsStreaming) is the canonical detector and
	// the one every other proxy site uses. The hand-rolled check this
	// replaces missed application/x-ndjson, which is exactly how Ollama
	// streams: an /api/chat stream was relayed down the non-streaming path,
	// so the per-chunk transforms never ran and every frame came back
	// stripped of the caller's @node spelling. It also logged those requests
	// as stream:false with the non-streaming metrics capture.
	isStreaming := isStreamingResponse(resp)

	meta := wire.RoutingMetadata{
		Provider:             provider,
		Node:                 host,
		InjectUsage:          s.config.Coordinator.Routing.InjectUsageMetadata,
		ClientModel:          clientModelFromContext(c.Request.Context()),
		SuppressBodyMetadata: wire.SuppressBodyFromVerbose(c.Query("verbose")),
		SuppressUsageFrame:   suppressUsageFrameFromContext(c.Request.Context()),
	}

	mergeUpstreamHeaders(c.Writer.Header(), resp.Header)
	if isStreaming {
		streamWithUsageInject(c.Writer, resp, s.inference.normalizers, provider, meta, recorder)
		return
	}
	captureNonStreamingMetrics(resp, recorder)
	writeNormalizedBody(c.Writer, resp, s.inference.normalizers, provider, meta, recorder)
}
