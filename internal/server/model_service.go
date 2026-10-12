package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Timeout constants using centralized values from pkg/constants
const (
	DefaultServiceTimeout      = constants.HTTPDefaultTimeout
	DefaultCacheRefreshTimeout = constants.HTTPDefaultTimeout
)

// ModelService handles business logic for model-related public API
// Responsibilities:
//   - Cache management
//   - Model filtering
//   - Orchestrating routing decisions
type ModelService struct {
	cache            ModelCacheProvider
	registry         ModelRegistryProvider
	host             NodeInfoProvider
	instanceProvider InstanceProvider // Instance management dependency
	router           routing.Router
	appsConfig       func() *pkgConfig.AppsConfig // Always returns current provider config
}

// NewModelService creates a new model service
func NewModelService(cache ModelCacheProvider, registry ModelRegistryProvider, host NodeInfoProvider, instanceProvider InstanceProvider, router routing.Router, appsConfig func() *pkgConfig.AppsConfig) *ModelService {
	return &ModelService{
		cache:            cache,
		registry:         registry,
		host:             host,
		instanceProvider: instanceProvider,
		appsConfig:       appsConfig,
		router:           router,
	}
}

// ListModels handles listing models with caching and routing
func (s *ModelService) ListModels(ctx context.Context, req *ListModelsRequest) ([]*cache.CachedModel, error) {
	utils.LogDebugf("[PublicAPI] ListModels: node=%s, repo=%s, app=%s, model=%s, refresh=%v",
		req.Node, req.Registry, req.App, req.Model, req.Refresh)

	ctx, cancel := ensureTimeout(ctx, DefaultServiceTimeout)
	defer cancel()

	if req.Refresh {
		if _, err := s.registry.RescanLocal(ctx); err != nil {
			return nil, err
		}
	}

	// Use centralized cache method (handles cache expiration, refresh, filtering)
	models, err := s.cache.ListModels(ctx, req.Node, req.Registry, req.App, req.Model)
	if err != nil {
		return nil, fmt.Errorf("failed to get cached models: %w", err)
	}

	utils.LogDebugf("[PublicAPI] Returning %d models from cache", len(models))
	return models, nil
}

// ShowModel uses the inference resolver and the serving node's metadata reader.
func (s *ModelService) ShowModel(ctx context.Context, req *ShowModelRequest) (*ShowModelResponse, error) {
	ctx, cancel := ensureTimeout(ctx, DefaultServiceTimeout)
	defer cancel()
	// Detail needs published identity even for pins; inference may route a catalog miss directly.
	if _, err := s.cache.ListModels(ctx, req.Node, req.Registry, req.App, req.ModelName); err != nil {
		return nil, err
	}
	ref := resolver.Reference{Model: req.ModelName, Provider: req.App, Node: req.Node, Registry: req.Registry}
	if req.AllowUncataloged {
		ref.DefaultProvider = constants.AppOllama
	}
	resolved, err := s.cache.ResolveModel(ctx, ref)
	if err != nil {
		return nil, err
	}
	if resolved.Node == "*" {
		return nil, newProblemError(http.StatusBadRequest, "Bad Request", "model show requires one node, never a broadcast")
	}
	if resolved.GroupName != "" || (resolved.Replica != nil && resolved.Replica.IsCloud) {
		return nil, newRouteProblemError(http.StatusNotImplemented, "Not Implemented", "model detail is unavailable for a group or cloud model; inspect GET /v1/models or select an exact catalog model with provider and node", httperr.CodeModelDetailUnsupported, resolved.Node)
	}
	model := resolved.RequestModel
	if model == "" {
		model = resolved.ModelName
	}
	query := url.Values{"model": {model}, "provider": {resolved.Provider}, "registry": {req.Registry}, "verbose": {strconv.FormatBool(req.Verbose)}, "compact": {strconv.FormatBool(req.Compact)}, "allow_uncataloged": {strconv.FormatBool(req.AllowUncataloged)}}
	if resolved.Replica != nil {
		query.Set("registry", resolved.Replica.SourceRepo)
		query.Set("source_id", resolved.Replica.SourceID)
	}
	details, err := routeAndParse[map[string]any](ctx, s.router, http.MethodGet, "/zzrouter/v1/internal/models/show?"+query.Encode(), localIfEmpty(resolved.Node), nil)
	if err != nil {
		return nil, err
	}
	if *details == nil {
		return nil, newRouteProblemError(http.StatusBadGateway, "Bad Gateway", "model detail must be a JSON object", httperr.CodeUpstreamShape, resolved.Node)
	}
	return s.mapToShowResponse(*details), nil
}

// mapToShowResponse preserves the complete payload alongside the common fields.
func (s *ModelService) mapToShowResponse(m map[string]any) *ShowModelResponse {
	size, _ := m["size"].(float64)
	return &ShowModelResponse{
		Details:     m,
		Name:        stringFromMaps("name", m),
		Size:        int64(size),
		Node:        stringFromMaps("node", m),
		SourceRepo:  stringFromMaps("source_repo", m),
		Format:      stringFromMaps("format", m),
		ModifiedAt:  stringFromMaps("modified_at", m),
		AssignedApp: stringFromMaps("assigned_app", m),
		Digest:      stringFromMaps("digest", m),
	}
}

// DeleteModels handles deleting models with routing
func (s *ModelService) DeleteModels(ctx context.Context, req *DeleteModelsRequest) (*DeleteModelsResponse, error) {
	utils.LogDebugf("[PublicAPI] DeleteModels: pattern=%s, paths=%d, models=%d",
		req.Pattern, len(req.Paths), len(req.Models))

	ctx, cancel := ensureTimeout(ctx, 60*time.Second)
	defer cancel()

	if err := req.Validate(); err != nil {
		return nil, newProblemError(400, "Bad Request", err.Error())
	}
	if req.Model != "" {
		if _, err := s.cache.ListModels(ctx, req.Node, req.Registry, req.Provider, req.Model); err != nil {
			return nil, err
		}
		resolved, err := s.cache.ResolveModel(ctx, resolver.Reference{Model: req.Model, Node: req.Node, Provider: req.Provider, Registry: req.Registry, Exact: true, SourceID: req.SourceID, DefaultProvider: req.DefaultProvider})
		if err != nil {
			return nil, err
		}
		if resolved.GroupName != "" || (resolved.Replica != nil && (resolved.Replica.VariantOf != "" || resolved.Replica.IsCloud)) {
			return nil, httperr.Error{Status: 409, Code: string(httperr.CodeModelNotDeletable), Message: "logical variants, groups and cloud models are configuration; use their supported management API instead of physical deletion"}
		}
		if resolved.Node == "*" {
			return nil, newProblemError(400, "Bad Request", "exact deletion requires one node")
		}
		logical := false
		s.appsConfig().RangeApps(func(provider string, cfg pkgConfig.ServiceConfig) bool {
			if resolved.Provider != "" && !strings.EqualFold(provider, resolved.Provider) {
				return true
			}
			_, _, logical = cfg.Variant(resolved.ModelName)
			return !logical
		})
		if logical {
			return nil, httperr.Error{Status: 409, Code: string(httperr.CodeModelNotDeletable), Message: "logical variants are configuration; remove the models entry through the parameters API"}
		}
		if resolved.Replica == nil {
			return nil, cache.ErrModelNotFound{ModelName: req.Model}
		}
		selected := *req
		selected.Model, selected.Provider, selected.Registry, selected.SourceID = resolved.Replica.Name, resolved.Replica.Provider, resolved.Replica.SourceRepo, resolved.Replica.SourceID
		selected.Node = localIfEmpty(resolved.Node)
		req = &selected
	}
	var resp *routing.Response
	var err error

	// Two routing patterns:
	// 1. Models array with per-model host targeting → MultiRoute
	// 2. Pattern/paths targeting single host or broadcast → Standard Route
	if len(req.Models) > 0 {
		resp, err = s.deleteModelsMultiNode(ctx, req.Models)
	} else {
		resp, err = s.deleteModelsStandard(ctx, req)
	}

	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, parseRoutedError(resp.StatusCode, resp.Body, resp.Headers)
	}

	// Parse response
	var deleteResp DeleteModelsResponse
	if err := json.Unmarshal(resp.Body, &deleteResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	utils.LogDebugf("[PublicAPI] Deleted %d models", deleteResp.Deleted)
	return &deleteResp, nil
}

// handleModelStats delegates to Server (implemented in model_utils.go)
// We will eventually move the business logic here
func (s *ModelService) handleModelStats(c *gin.Context) {
	// For now, cast to access Server method if we have to,
	// or better, implement the logic here using the interfaces.

	// Implementation from model_utils.go handleModelStats
	host := QueryNode(c)

	// Local query: explicit local host or no cluster configured
	if s.cache.IsLocalNode(host) {
		respondSuccess(c, "Model stats retrieved", s.host.GetLocalStats())
		return
	}

	// Cluster broadcast logic (simplified for now using router)
	resp, err := s.router.Broadcast(c.Request.Context(), "/zzrouter/v1/internal/models/stats", "GET", nil)
	if err != nil {
		BadGateway(c, "Cluster stats broadcast failed")
		return
	}

	// Broadcast passthrough: forward the internal response verbatim so whatever
	// envelope the internal handler produces is preserved without double-wrapping.
	c.Data(resp.StatusCode, "application/json", resp.Body)
}

// handleRescanModels triggers a registry-scan-cache invalidation on
// the requested node. With ?node= set, routes to that specific peer
// (the worker re-runs its Ollama daemon /api/tags probe + filesystem
// scan); without, falls back to broadcast for backward compat.
//
// Use case: an operator started Ollama on a worker AFTER zzrouter-node
// was running, or someone ran `ollama pull` directly against the
// worker's daemon (bypassing the manager). Without an explicit rescan,
// the worker's scan cache stays stale until something else invalidates
// it (a managed pull/delete, an apps-config reload). Hitting this
// endpoint surfaces the new state to the cluster catalog immediately.
func (s *ModelService) handleRescanModels(c *gin.Context) {
	host := QueryNode(c)

	// Local query: explicit local host or no cluster configured.
	if s.cache.IsLocalNode(host) {
		result, err := s.registry.RescanLocal(c.Request.Context())
		if err != nil {
			InternalNodeError(c, "Failed to rescan models")
			return
		}
		respondSuccess(c, "Rescan completed", result)
		return
	}

	// Targeted: route to the specific worker's internal rescan endpoint
	// so the response reflects exactly that node's post-invalidation
	// model count, not an aggregated-broadcast value that can't
	// disambiguate which node actually re-scanned.
	if host != "" {
		routingReq := &routing.Request{
			Path:   "/zzrouter/v1/internal/models/rescan",
			Method: "POST",
			Node:   host,
		}
		resp, err := s.router.Route(c.Request.Context(), routingReq)
		if err != nil {
			BadGateway(c, fmt.Sprintf("Rescan routing to %q failed", host))
			return
		}

		c.Data(resp.StatusCode, "application/json", resp.Body)
		return
	}

	// No target: broadcast to every peer.
	resp, err := s.router.Broadcast(c.Request.Context(), "/zzrouter/v1/internal/models/rescan", "POST", nil)
	if err != nil {
		BadGateway(c, "Cluster rescan broadcast failed")
		return
	}

	c.Data(resp.StatusCode, "application/json", resp.Body)
}

// handleCacheStats returns cache performance metrics. Role-agnostic:
// every node runs its own Cache (local-only on workers, cluster-wide
// on coord), so stats are always reportable.
func (s *ModelService) handleCacheStats(c *gin.Context) {
	stats := gin.H{
		"status": "enabled",
	}
	if provider, ok := s.cache.(CacheStatsProvider); ok {
		stats["cache"] = provider.GetCacheStats()
	}
	respondSuccess(c, "Cache stats retrieved", stats)
}

// CacheStatsProvider is implemented by cache providers that can report metrics.
type CacheStatsProvider interface {
	GetCacheStats() map[string]any
}

// deleteModelsMultiNode routes legacy per-node batches through the existing router.
func (s *ModelService) deleteModelsMultiNode(ctx context.Context, models []ModelToDelete) (*routing.Response, error) {
	grouped := map[string][]ModelToDelete{}
	for _, model := range models {
		node := model.Node
		if node == "" || node == s.host.GetNodename() {
			node = s.host.GetNodename()
		}
		grouped[node] = append(grouped[node], model)
	}
	bodies := make(map[string][]byte, len(grouped))
	for node, items := range grouped {
		body, err := json.Marshal(DeleteModelsRequest{Models: items})
		if err != nil {
			return nil, err
		}
		bodies[node] = body
	}
	response, err := s.router.MultiRoute(ctx, "/zzrouter/v1/internal/models", http.MethodDelete, bodies)
	if err != nil {
		return nil, err
	}
	return s.aggregateDeleteResponses(response)
}

// aggregateDeleteResponses preserves completed effects from every responding node.
func (s *ModelService) aggregateDeleteResponses(response *routing.Response) (*routing.Response, error) {
	var nodes []struct {
		Node   string          `json:"node"`
		Error  string          `json:"error"`
		Status int             `json:"status_code"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(response.Body, &nodes); err != nil {
		return nil, fmt.Errorf("decode deletion responses: %w", err)
	}
	aggregate := DeleteModelsResponse{}
	var failure *RoutedError
	applied := false
	for _, node := range nodes {
		if node.Error != "" {
			aggregate.Errors = append(aggregate.Errors, node.Node+": "+node.Error)
			continue
		}
		if node.Status >= http.StatusBadRequest {
			current := parseRoutedError(node.Status, node.Body, nil)
			if failure == nil {
				failure = current
			}
			if pd := current.ProblemDetails; pd != nil {
				if pd.Deleted != nil {
					aggregate.Deleted += *pd.Deleted
				}
				applied = applied || pd.MutationApplied
			}
			continue
		}
		var local DeleteModelsResponse
		if err := json.Unmarshal(node.Body, &local); err != nil || string(node.Body) == "null" {
			aggregate.Errors = append(aggregate.Errors, node.Node+": invalid deletion response")
			continue
		}
		aggregate.Deleted += local.Deleted
		aggregate.Errors = append(aggregate.Errors, local.Errors...)
	}
	if failure != nil {
		if failure.ProblemDetails == nil {
			failure.ProblemDetails = utils.NewProblemDetails(failure.StatusCode, http.StatusText(failure.StatusCode), failure.Error(), "")
		}
		failure.ProblemDetails.Deleted = &aggregate.Deleted
		failure.ProblemDetails.MutationApplied = applied || aggregate.Deleted > 0
		return nil, failure
	}
	body, err := json.Marshal(aggregate)
	if err != nil {
		return nil, err
	}
	return &routing.Response{StatusCode: http.StatusOK, Body: body}, nil
}

// deleteModelsStandard handles pattern/path-based deletion
// Uses standard Route for single host or broadcast
func (s *ModelService) deleteModelsStandard(ctx context.Context, req *DeleteModelsRequest) (*routing.Response, error) {
	requestBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	if req.Node == "" || req.Node == "*" {
		response, err := s.router.MultiRoute(ctx, "/zzrouter/v1/internal/models", http.MethodDelete, map[string][]byte{"*": requestBody})
		if err != nil {
			return nil, err
		}
		return s.aggregateDeleteResponses(response)
	}

	routingReq := &routing.Request{
		Path:   "/zzrouter/v1/internal/models",
		Method: "DELETE",
		Node:   req.Node,
		Body:   requestBody,
	}

	return s.router.Route(ctx, routingReq)
}

// handleSaveModelDefaults handles POST /zzrouter/models/defaults.
// Stub: persistence isn't yet wired through ModelService; returning a
// 501 in the canonical envelope is more honest than the previous
// silent 200 with empty body, which made the endpoint look like it
// succeeded when nothing was stored.
func (s *ModelService) handleSaveModelDefaults(c *gin.Context) {
	RespondToError(c, newProblemError(http.StatusNotImplemented, "Not Implemented",
		"saving model defaults is not yet implemented; track via /zzrouter/v1 API discovery for availability"))
}

// handleGetModelInstance returns instance info for a loaded model.
// The route uses a `*model` wildcard so HuggingFace-style names containing
// slashes (e.g. "mlx-community/SmolLM-135M-Instruct-4bit") match correctly.
// Gin captures the wildcard with a leading slash, which we strip here.
func (s *ModelService) handleGetModelInstance(c *gin.Context) {
	modelName := strings.TrimPrefix(c.Param("model"), "/")

	resolved, err := s.cache.ResolveModel(c.Request.Context(), resolver.Reference{Model: modelName, Node: c.GetHeader("X-Node")})
	if err != nil {
		RespondToError(c, err)
		return
	}
	var runs struct {
		Instances []instance.InstanceInfo `json:"instances"`
	}
	if resolved.IsRemote() {
		ctx, cancel := context.WithTimeout(c.Request.Context(), constants.ClusterQueryTimeout)
		defer cancel()
		resp, err := s.router.Unicast(ctx, resolved.Node, "/zzrouter/v1/internal/runs", http.MethodGet, nil)
		if err != nil {
			RespondToError(c, newRoutedTransportError(err, resolved.Node))
			return
		}
		if resp.StatusCode != http.StatusOK {
			RespondToError(c, parseRoutedError(resp.StatusCode, resp.Body, resp.Headers))
			return
		}
		if err := json.Unmarshal(resp.Body, &runs); err != nil {
			BadGateway(c, "node returned an invalid run inventory")
			return
		}
	} else if inst, found := s.instanceProvider.GetInstanceByModel(resolved.Provider, resolved.ModelName); found {
		runs.Instances = []instance.InstanceInfo{inst.ToInfo("")}
	}
	for _, run := range runs.Instances {
		if strings.EqualFold(run.Model, resolved.ModelName) && (resolved.Provider == "" || strings.EqualFold(run.Provider, resolved.Provider)) && (!resolved.IsRemote() || !run.Status.IsTerminal()) {
			respondSuccess(c, "Model instance retrieved", gin.H{"model": modelName, "instance": run.ID, "port": run.Port, "status": string(run.Status), "provider": run.Provider})
			return
		}
	}
	NotFound(c, fmt.Sprintf("model %q not loaded on the selected node; inspect GET /zzrouter/v1/runs", modelName))
}
