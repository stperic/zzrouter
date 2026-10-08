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
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/version"
)

// OllamaHandlers groups all Ollama compatibility HTTP handlers.
type OllamaHandlers struct {
	ollama                OllamaService
	ollamaDaemon          func() (*backend.Resolved, error)
	proxyToOllamaProvider func(*gin.Context, *backend.Resolved, string, []byte)
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

// ollamaRootProbeBody is the daemon's exact root response. The Ollama
// CLI matches on it, so it is a fixed wire string, not a message.
const ollamaRootProbeBody = "Ollama is running"

// handleOllamaRootProbe answers GET / and HEAD /.
//
// Plain text is the default, including for the "*/*" that both the
// Ollama CLI and curl send: the CLI v0.20+ uses this as a pre-flight
// connectivity check and aborts with a generic error if the body is not
// the daemon's own.
//
// A caller that explicitly asks for JSON gets a node identity document
// instead. The root is the first thing anything probes, and answering
// only "Ollama is running" names the wrong product and offers nowhere
// to go next.
func handleOllamaRootProbe(c *gin.Context) {
	if !clientAcceptsJSON(c) {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusOK, ollamaRootProbeBody)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"service":     "zzrouter",
		"version":     version.Current.String(),
		"api_version": "v1",
		"links": gin.H{
			"health":    "/health",
			"discovery": apiDiscoveryPrefix,
			"openapi":   "/openapi.json",
		},
	})
}

// clientAcceptsJSON reports whether the caller named application/json
// in Accept. A wildcard does not count: the Ollama CLI sends "*/*" and
// must keep getting the plain-text body.
func clientAcceptsJSON(c *gin.Context) bool {
	return strings.Contains(c.GetHeader("Accept"), "application/json")
}

// handleOllamaVersion proxies to the real Ollama daemon. The /api/*
// group is gated on Ollama being enabled in the cluster, so by the
// time a request arrives here at least one Ollama backend exists; if
// the local endpoint resolution still fails, that's an upstream-state
// problem (worker unreachable, endpoint not yet stamped) and a 503
// is the honest answer.
func (h *OllamaHandlers) handleOllamaVersion(c *gin.Context) {
	daemon, err := h.ollamaDaemon()
	if err != nil {
		OllamaError(c, http.StatusServiceUnavailable,
			"Ollama endpoint not resolved on this node; check /zzrouter/v1/providers/ollama")
		return
	}
	h.proxyToOllamaProvider(c, daemon, "/api/version", nil)
}

// handleOllamaMe proxies POST /api/me to the upstream Ollama daemon. The
// CLI uses this endpoint as an account-status handshake on every command;
// the daemon's natural response is 401 + {"error":"unauthorized","signin_url":...}
// when the host has no linked ollama.com account. With no upstream daemon
// configured we synthesize a 401 in the same shape (no signin_url since
// we have no per-host keypair to anchor it) — that's enough to satisfy
// the CLI's "endpoint exists" check without claiming an account state.
func (h *OllamaHandlers) handleOllamaMe(c *gin.Context) {
	daemon, err := h.ollamaDaemon()
	if err != nil {
		OllamaError(c, http.StatusUnauthorized, "unauthorized")
		return
	}
	body, _ := c.GetRawData()
	h.proxyToOllamaProvider(c, daemon, "/api/me", body)
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

	// Count replicas per name so single-replica entries stay bare and
	// multi-replica entries get the @node suffix. Same disambiguation
	// rule as /api/ps:HandleOllamaPsLocal.
	replicaCount := make(map[string]int, len(models))
	for _, m := range models {
		replicaCount[m.Name]++
	}

	entries := make([]any, 0, len(models))
	for _, m := range models {
		entry := h.adapter.ListEntry(m)
		if replicaCount[m.Name] >= 2 && m.Node != "" {
			if e, ok := entry.(OllamaTagEntry); ok {
				e.Name = e.Name + "@" + m.Node
				entry = e
			}
		}
		entries = append(entries, entry)
	}

	c.JSON(http.StatusOK, h.adapter.ListEnvelope(entries))
}

// handleOllamaShow answers POST /api/show by pulling the canonical details
// via ModelService.ShowModel (which routes to the node hosting the model
// and returns the raw provider response), then enriches with auto-route
// metadata if the model is part of a multi-replica group.
//
// The /api/show body accepts both `model` (upstream primary) and its
// `name` alias. /api/copy, /api/push, /api/delete, /api/create remain on the
// direct-proxy path via OllamaService.ManagementCommand; they move to
// ModelService in a later milestone once matching canonical methods exist.
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

	// Strip the @node routing hint before catalog lookup; node selection
	// happens in ShowModel via the App scope (cluster-aware) and the
	// upstream proxy fallback. Same parse the inference path uses.
	modelName, nodeHint := splitOllamaAddr(rawName)

	resp, err := h.modelService.ShowModel(c.Request.Context(), &ShowModelRequest{
		ModelName: modelName,
		Node:      nodeHint,
		App:       "ollama",
	})
	if err != nil {
		if _, notFound := err.(cache.ErrModelNotFound); notFound {
			// zzrouter's catalog doesn't know about this model yet — likely
			// pulled directly via /api/pull which doesn't sync the registry.
			// Fall back to upstream Ollama so /api/show stays consistent
			// with /api/chat + /api/generate (both work for Ollama-pulled
			// models that bypass the catalog). Use the bare name (no
			// @node suffix) since upstream Ollama doesn't speak that grammar.
			if h.proxyShowToUpstream(c, modelName) {
				return
			}
			OllamaError(c, http.StatusNotFound, "model '"+modelName+"' not found")
			return
		}
		if multi, ok := err.(ErrMultipleMatches); ok {
			OllamaError(c, http.StatusBadRequest,
				"ambiguous model '"+multi.ModelName+"': matched on multiple nodes "+
					fmt.Sprintf("%v", multi.Matches)+"; disambiguate with model@node (e.g. '"+multi.Matches[0]+"')")
			return
		}
		OllamaError(c, http.StatusInternalServerError, "failed to show model: "+err.Error())
		return
	}

	// Enrich with route metadata when the model is in an auto-route group
	// with ≥2 replicas. Single-deployment models omit the field entirely.
	if h.groups != nil {
		if group := h.groups.Get(modelName); group != nil && len(group.Replicas) >= 2 {
			nodes := make([]string, 0, len(group.Replicas))
			for _, rep := range group.Replicas {
				nodes = append(nodes, rep.Node)
			}
			strategy := string(group.Strategy)
			if strategy == "" {
				strategy = string(modelgroup.StrategyPriority)
			}
			resp.Route = &ModelRouteInfo{
				Strategy: strategy,
				Replicas: len(group.Replicas),
				Nodes:    nodes,
				Group:    modelName,
			}
		}
	}

	c.JSON(http.StatusOK, h.adapter.ShowResponse(resp))
}

// splitOllamaAddr separates an Ollama model identifier into its bare
// name and an optional @node routing hint. Used by /api/show + the
// management verbs so callers can target a specific cluster node
// (matches the model@node grammar /api/ps emits and the dispatch
// path consumes via parseInferenceModel). Empty hint = use existing
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
// exists. Coord's appsConfig reflects cluster-wide provider state via
// peer-sync, so a local check is sufficient — no fan-out per request.
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
	appCfg, ok := cfg.LookupApp("ollama")
	return ok && appCfg.IsEnabled()
}

// proxyShowToUpstream forwards POST /api/show to the local Ollama daemon
// when the zzrouter catalog has no entry for modelName. Returns true if a
// response was written (success OR upstream's own 404), false if no local
// Ollama endpoint is configured (caller falls back to the canonical 404).
func (h *OllamaHandlers) proxyShowToUpstream(c *gin.Context, modelName string) bool {
	daemon, err := h.ollamaDaemon()
	if err != nil {
		return false
	}
	body, err := json.Marshal(map[string]any{"model": modelName})
	if err != nil {
		return false
	}
	h.proxyToOllamaProvider(c, daemon, "/api/show", body)
	return true
}

func (h *OllamaHandlers) handleOllamaPs(c *gin.Context) {
	h.handleRoute(c, RoutingConfig{
		Endpoint:     "/zzrouter/v1/internal/ollama/ps",
		ArrayField:   "models",
		LocalHandler: h.HandleOllamaPsLocal,
	})
}

// HandleOllamaPsLocal handles GET /zzrouter/v1/internal/ollama/ps.
//
// Each returned entry's Name is suffixed with `@<node>` so that the
// cluster-aggregated response (produced by handleOllamaPs on the
// coordinator) has per-instance addressable rows — two nodes loading the
// same model become two distinct entries, each with its own live VRAM
// usage and expires_at. The Model field stays bare so clients that
// group-by-model can still do so.
//
// The `@<node>` suffix matches the canonical name grammar established by
// M1 (right-side qualifier: `llama3:8b@gpu-1`), consistent with
// HuggingFace revisions, Docker digests, email, and Go module versions.
//
// Live polling is preserved: each request hits the upstream Ollama
// daemon directly via protocol.ListRunningModels. Caching here would
// produce stale VRAM/TTL data and break Open WebUI's stop-model UI.
func (h *OllamaHandlers) HandleOllamaPsLocal(c *gin.Context) {
	ctx := c.Request.Context()
	entries := []OllamaPsEntry{}

	nodeSuffix := ""
	if h.nodeName != nil {
		if n := h.nodeName(); n != "" {
			nodeSuffix = "@" + n
		}
	}

	h.forEachLocalOllamaApp(func(name string, p protocol.ModelLister, t protocol.Target) {
		running, err := p.ListRunningModels(ctx, t)
		if err != nil {
			slog.Debug("ollama ps query failed", "app", name, "error", err)
			return
		}
		for _, m := range running {
			entries = append(entries, OllamaPsEntry{
				Name:          m.Name + nodeSuffix,
				Model:         m.Name,
				Size:          m.Size,
				Digest:        m.Digest,
				SizeVRAM:      m.SizeVRAM,
				ContextLength: m.ContextLength,
				ExpiresAt:     m.ExpiresAt,
				Details: OllamaModelDetails{
					Format:        m.Format,
					Family:        m.Family,
					ParameterSize: m.ParameterSize,
					QuantLevel:    m.QuantLevel,
				},
			})
		}
	})
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
	digest := c.Param("digest")
	if digest == "" {
		c.Status(http.StatusBadRequest)
		return
	}
	status, _ := h.ollama.CheckBlob(c.Request.Context(), digest)
	c.Status(status)
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

// forEachLocalOllamaApp invokes fn once per registered local Ollama app with
// a parsed, enabled endpoint. Apps without a usable endpoint are skipped.
func (h *OllamaHandlers) forEachLocalOllamaApp(fn func(name string, p protocol.ModelLister, t protocol.Target)) {
	cfg := h.appsConfig()
	if h.appMgr == nil || cfg == nil {
		return
	}
	resolver := backend.NewResolver(h.appsConfig)
	for key, app := range h.appMgr.Protocols().GetAll() {
		if !strings.EqualFold(app.Type(), "ollama") {
			continue
		}
		resolved, ok := resolver.Resolve(key)
		if !ok {
			continue
		}
		fn(key, app, protocol.TargetOf(resolved))
	}
}
