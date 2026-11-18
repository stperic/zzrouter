// Ollama compatibility handlers for the /api/* surface.
//
// Wire format is dictated by the Ollama protocol: success responses use
// Ollama's own JSON shapes (see ollama_types.go) and errors use the
// {"error": "..."} envelope via RespondToErrorOllama. Service-layer code
// returns *RoutedError values; the translation to Ollama wire format happens
// only at the handler boundary.
//
// Scope: /api/* routes to Ollama-provider models only. A request for a model
// served by a non-Ollama provider returns 404 pointing at /v1/chat/completions.
package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/access/control"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// OllamaHandlers groups all Ollama compatibility HTTP handlers.
type OllamaHandlers struct {
	access                catalogAccess
	ollama                OllamaService
	ollamaDaemon          func() (*backend.Resolved, error)
	proxyToOllamaProvider func(*gin.Context, *backend.Resolved, string, []byte, func() error)
	handleRoute           func(*gin.Context, RoutingConfig)
	appMgr                *prov_apps.ProviderAppManager
	appsConfig            func() *pkgConfig.AppsConfig

	// modelService + adapter form the M2 protocol-adapter pipeline:
	// /api/tags and /api/show pull from the canonical cluster-aware
	// catalog and translate via the adapter to Ollama wire format.
	modelService *ModelService
	adapter      ProtocolAdapter
	groups       *modelgroup.GroupStore

	// nodeName returns this node's own hostname. Used by /api/ps to
	// suffix entries as `<model>@<node>` so the cluster-aggregated
	// response is unambiguous — each running instance is its own row,
	// addressable for operations like targeted unload. The `@<node>`
	// suffix matches the canonical name grammar (right-side qualifier,
	// consistent with email / HuggingFace / Docker / Go modules).
	nodeName func() string
}

// ollamaInferenceHandler returns a handler that routes an Ollama inference
// request of the given kind ("generate", "chat", "embeddings").
func (h *OllamaHandlers) ollamaInferenceHandler(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := h.ollama.RouteInference(c, kind); err != nil && !c.Writer.Written() {
			RespondToErrorOllama(c, err)
		}
	}
}

// ollamaManagementHandler returns a handler that dispatches an Ollama
// management command. modelLookup toggles the remote-cache lookup path used
// by commands that name a specific model (delete/copy/show/push).
func (h *OllamaHandlers) ollamaManagementHandler(endpoint string, modelLookup bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := h.ollama.ManagementCommand(c, endpoint, modelLookup); err != nil && !c.Writer.Written() {
			RespondToErrorOllama(c, err)
		}
	}
}

// handleOllamaVersion proxies to the real Ollama daemon. The /api/*
// group is gated on Ollama being enabled in the cluster, so by the
// time a request arrives here at least one Ollama backend exists; if
// the local endpoint resolution still fails, that's an upstream-state
// problem (worker unreachable, endpoint not yet stamped) and a 503
// is the honest answer.
func (h *OllamaHandlers) handleOllamaVersion(c *gin.Context) {
	if err := ollamaProviderAccessError(c); err != nil {
		RespondToErrorOllama(c, err)
		return
	}
	daemon, err := h.ollamaDaemon()
	if err != nil {
		OllamaError(c, http.StatusServiceUnavailable,
			"Ollama endpoint not resolved on this node; check /zzrouter/v1/providers/ollama")
		return
	}
	h.proxyToOllamaProvider(c, daemon, "/api/version", nil, nil)
}

// handleOllamaMe proxies POST /api/me to the upstream Ollama daemon. The
// CLI uses this endpoint as an account-status handshake on every command;
// the daemon's natural response is 401 + {"error":"unauthorized","signin_url":...}
// when the host has no linked ollama.com account. With no upstream daemon
// configured we synthesize a 401 in the same shape (no signin_url since
// we have no per-host keypair to anchor it) — that's enough to satisfy
// the CLI's "endpoint exists" check without claiming an account state.
func (h *OllamaHandlers) handleOllamaMe(c *gin.Context) {
	if control.RoutesRestricted(GetAccessContext(c)) && control.ProviderAllowed(GetAccessContext(c), constants.AppOllama) {
		OllamaError(c, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := ollamaAccessError(c); err != nil {
		RespondToErrorOllama(c, err)
		return
	}
	daemon, err := h.ollamaDaemon()
	if err != nil {
		OllamaError(c, http.StatusUnauthorized, "unauthorized")
		return
	}
	body, _ := c.GetRawData()
	h.proxyToOllamaProvider(c, daemon, "/api/me", body, nil)
}

// handleOllamaTags lists Ollama-backed models only. Queries ModelService
// with App:"ollama" so non-Ollama formats (MLX, HF, cloud) are filtered
// out — agents using /api/tags expect "what Ollama can serve", not the
// universal catalog. Multi-node disambiguation via name@node suffix
// only when ≥2 nodes carry the same model (matches /api/ps convention).
func (h *OllamaHandlers) handleOllamaTags(c *gin.Context) {
	models, err := h.modelService.ListModels(c.Request.Context(), &ListModelsRequest{App: "ollama"})
	if err != nil {
		OllamaError(c, http.StatusInternalServerError, "failed to list models")
		return
	}

	models = slices.DeleteFunc(slices.Clone(models), func(m *cache.CachedModel) bool { return !h.access.permits(c, m.Name, m.Provider) })
	// Count replicas per name so single-replica entries stay bare and
	// multi-replica entries get the @node suffix. Same disambiguation
	// rule as /api/ps:HandleOllamaPsLocal.
	replicaCount := make(map[string]int, len(models))
	for _, m := range models {
		replicaCount[m.Name]++
	}

	entries := make([]any, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		entry := h.adapter.ListEntry(m)
		if replicaCount[m.Name] >= 2 && m.Node != "" {
			if e, ok := entry.(OllamaTagEntry); ok {
				e.Name = e.Name + "@" + m.Node
				entry = e
			}
		}
		entries = append(entries, entry)
		if e, ok := entry.(OllamaTagEntry); ok {
			seen[e.Name] = true
		}
	}

	if h.groups != nil {
		for name, group := range h.groups.List() {
			if seen[name] || !group.Available() {
				continue
			}
			for _, replica := range h.access.replicas(c, name, group.Replicas) {
				if replica.App == constants.AppOllama {
					entries = append(entries, OllamaTagEntry{Name: name, Model: name})
					break
				}
			}
		}
	}
	c.JSON(http.StatusOK, h.adapter.ListEnvelope(entries))
}

// handleOllamaShow returns authorized topology for a group alias, or delegates
// physical-model metadata to ModelService. Both model and name are accepted.
func (h *OllamaHandlers) handleOllamaShow(c *gin.Context) {
	var req OllamaManagementRequest
	if !BindJSON(c, &req) {
		return
	}
	rawName := req.ModelName()
	if rawName == "" {
		OllamaError(c, http.StatusBadRequest, "model field is required")
		return
	}

	// A route has no single weight file or template. Return its authorized
	// topology without arbitrarily querying one physical replica for metadata.
	if h.groups != nil && c.GetHeader("X-Node") == "" {
		if group := h.groups.Get(rawName); group != nil && group.Available() {
			replicas := h.access.replicas(c, rawName, group.Replicas)
			nodes := make([]string, 0, len(replicas))
			for _, rep := range replicas {
				if rep.App == constants.AppOllama {
					nodes = append(nodes, rep.Node)
				}
			}
			if len(nodes) > 0 {
				c.JSON(http.StatusOK, h.adapter.ShowResponse(&ShowModelResponse{Name: rawName, Route: &ModelRouteInfo{
					Group: rawName, Strategy: strategyName(group.Strategy), Replicas: len(nodes), Nodes: nodes,
				}}))
				return
			}
		}
	}

	// The show owner resolves physical models, including uncataloged requests.
	modelName, nodeHint := splitOllamaAddr(rawName)
	resp, err := h.modelService.ShowModel(c.Request.Context(), &ShowModelRequest{
		ModelName: modelName, Node: nodeHint, Verbose: req.Verbose, AllowUncataloged: true,
		Admit: func(model, provider string) bool { return h.access.permits(c, model, provider) },
	})
	if err != nil {
		RespondToErrorOllama(c, err)
		return
	}

	c.JSON(http.StatusOK, h.adapter.ShowResponse(resp))
}

// splitOllamaAddr separates an Ollama model identifier into its bare
// name and an optional @node routing hint. Used by /api/show + the
// management verbs so callers can target a specific cluster node
// (matches the model@node grammar /api/ps emits and the dispatch
// path consumes via the model resolver). Empty hint = use existing
// resolution logic.
func splitOllamaAddr(raw string) (name, node string) {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw, ""
	}
	return raw[:at], raw[at+1:]
}

// requireOllamaPresence is a middleware that gates the /api/* surface on
// at least one node in the cluster having the ollama provider enabled.
// Returns 503 + Ollama-shape error with an install hint when no backend
// exists. The provider tree includes per-node enable choices, so this
// check needs no network fan-out.
func (h *OllamaHandlers) requireOllamaPresence() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.ollamaEnabled() {
			c.Next()
			return
		}
		OllamaError(c, http.StatusServiceUnavailable,
			"no Ollama backend available; install via POST /zzrouter/v1/providers/ollama/install?node=<n>")
		c.Abort()
	}
}

// ollamaEnabled reports whether the ollama provider is enabled on any
// node visible from this node's view of the cluster. The provider sync
// mechanism propagates enable state across coord/worker, so the local
// AppsConfig is the canonical signal.
func (h *OllamaHandlers) ollamaEnabled() bool {
	cfg := h.appsConfig()
	if cfg == nil {
		return false
	}
	enabled := false
	cfg.RangeApps(func(name string, provider pkgConfig.ServiceConfig) bool {
		if name != constants.AppOllama && provider.Protocol != pkgConfig.ProtocolOllama {
			return true
		}
		if provider.IsEnabled() {
			enabled = true
			return false
		}
		for node := range provider.Nodes {
			if choice := provider.EnabledOn(node); choice != nil && *choice {
				enabled = true
				return false
			}
		}
		return true
	})
	return enabled
}

func (h *OllamaHandlers) handleOllamaPs(c *gin.Context) {
	h.handleRoute(c, RoutingConfig{
		Endpoint:     "/zzrouter/v1/internal/ollama/ps",
		ArrayField:   "models",
		LocalHandler: h.HandleOllamaPsLocal,
		Transform: func(body []byte) ([]byte, error) {
			var response OllamaPsResponse
			if err := json.Unmarshal(body, &response); err != nil {
				return nil, err
			}
			response.Models = slices.DeleteFunc(response.Models, func(entry OllamaPsEntry) bool { return !h.access.permits(c, entry.Model, entry.Provider) })
			for i := range response.Models {
				response.Models[i].Provider = ""
			}
			return json.Marshal(response)
		},
	})
}

// HandleOllamaPsLocal handles GET /zzrouter/v1/internal/ollama/ps from the run registry, which the reconcile keeps true.
//
// Each returned entry's Name is suffixed with `@<node>` so that the
// cluster-aggregated response (produced by handleOllamaPs on the
// coordinator) has per-instance addressable rows — two nodes loading the
// same model become two distinct entries. The Model field stays bare so
// clients that group-by-model can still do so.
//
// The `@<node>` suffix matches the canonical name grammar established by
// M1 (right-side qualifier: `llama3:8b@gpu-1`), consistent with
// HuggingFace revisions, Docker digests, email, and Go module versions.
func (h *OllamaHandlers) HandleOllamaPsLocal(c *gin.Context) {
	entries := []OllamaPsEntry{}
	nodeSuffix := ""
	if h.nodeName != nil {
		if n := h.nodeName(); n != "" {
			nodeSuffix = "@" + n
		}
	}
	if h.appMgr != nil {
		for _, run := range h.appMgr.Instances().List() {
			svc, ok := h.appMgr.ProviderConfig(run.Provider)
			if !ok || prov_apps.Lifecycle(svc) != prov_apps.LifecycleServed || run.GetStatus() != instance.StatusRunning {
				continue
			}
			loaded := run.Loaded()
			entries = append(entries, OllamaPsEntry{
				Name: run.Model + nodeSuffix, Model: run.Model, Provider: run.Provider,
				Size: loaded.Size, SizeVRAM: loaded.VRAM, ContextLength: loaded.ContextLength, ExpiresAt: loaded.ExpiresAt, Digest: loaded.Digest,
				Details: OllamaModelDetails{Format: loaded.Format, Family: loaded.Family, ParameterSize: loaded.ParameterSize, QuantLevel: loaded.QuantLevel},
			})
		}
	}
	c.JSON(http.StatusOK, OllamaPsResponse{Models: entries})
}

// handleOllamaPull handles POST /api/pull.
func (h *OllamaHandlers) handleOllamaPull(c *gin.Context) {
	var req OllamaPullRequest
	if !BindJSON(c, &req) {
		return
	}
	if req.Model == "" {
		OllamaError(c, http.StatusBadRequest, "model field is required")
		return
	}
	if err := h.ollama.Deploy(c, req.Model, req.Insecure, req.Stream); err != nil && !c.Writer.Written() {
		RespondToErrorOllama(c, err)
	}
}

func (h *OllamaHandlers) handleOllamaBlobsHead(c *gin.Context) {
	if err := ollamaAccessError(c); err != nil {
		RespondToErrorOllama(c, err)
		return
	}
	digest := c.Param("digest")
	if digest == "" {
		c.Status(http.StatusBadRequest)
		return
	}
	if err := h.ollama.CheckBlob(c, digest); err != nil && !c.Writer.Written() {
		RespondToErrorOllama(c, err)
	}
}

func (h *OllamaHandlers) handleOllamaBlobsPost(c *gin.Context) {
	params := ValidateParams(c, []ParamRule{RequiredPathParam("digest")})
	if params == nil {
		return
	}
	if err := h.ollama.CreateBlob(c, params.GetString("digest")); err != nil && !c.Writer.Written() {
		RespondToErrorOllama(c, err)
	}
}

// handleOllamaDelete delegates physical ownership to the shared exact deletion service.
func (h *OllamaHandlers) handleOllamaDelete(c *gin.Context) {
	if err := ollamaMutationAccessError(c); err != nil {
		RespondToErrorOllama(c, err)
		return
	}
	var req OllamaManagementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		OllamaError(c, 400, "invalid model deletion request")
		return
	}
	_, err := h.modelService.DeleteModels(c.Request.Context(), &DeleteModelsRequest{Model: req.ModelName(), Node: c.GetHeader("X-Node"), DefaultProvider: constants.AppOllama, Admit: func(model, provider string) bool { return h.access.permits(c, model, provider) }})
	if err != nil {
		RespondToErrorOllama(c, err)
		return
	}
	c.JSON(200, gin.H{})
}
