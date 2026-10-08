package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/model/cache"
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
	onModelDelete    func(modelName string)       // Optional callback after model deletion (for auto-route sync)
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

// SetOnModelDelete sets an optional callback invoked after model deletion.
func (s *ModelService) SetOnModelDelete(fn func(modelName string)) {
	s.onModelDelete = fn
}

// ListModels handles listing models with caching and routing
func (s *ModelService) ListModels(ctx context.Context, req *ListModelsRequest) ([]*cache.CachedModel, error) {
	utils.LogDebugf("[PublicAPI] ListModels: node=%s, repo=%s, app=%s, model=%s, refresh=%v",
		req.Node, req.Registry, req.App, req.Model, req.Refresh)

	ctx, cancel := ensureTimeout(ctx, DefaultServiceTimeout)
	defer cancel()

	// If refresh requested, invalidate cache (next request will rebuild)
	if req.Refresh {
		utils.LogDebugf("[PublicAPI] Refresh requested - invalidating cache")
		s.cache.Invalidate()
	}

	// Use centralized cache method (handles cache expiration, refresh, filtering)
	models, err := s.cache.ListModels(ctx, req.Node, req.Registry, req.App, req.Model)
	if err != nil {
		return nil, fmt.Errorf("failed to get cached models: %w", err)
	}

	utils.LogDebugf("[PublicAPI] Returning %d models from cache", len(models))
	return models, nil
}

// ShowModel handles showing model details with routing
func (s *ModelService) ShowModel(ctx context.Context, req *ShowModelRequest) (*ShowModelResponse, error) {
	utils.LogDebugf("[PublicAPI] ShowModel: model=%s, node=%s, app=%s, verbose=%v",
		req.ModelName, req.Node, req.App, req.Verbose)

	ctx, cancel := ensureTimeout(ctx, DefaultServiceTimeout)
	defer cancel()

	// Step 1: Find the model in cache to determine target host
	matches, err := s.cache.ListModels(ctx, req.Node, "", req.App, req.ModelName)
	if err != nil {
		return nil, fmt.Errorf("failed to get cached models: %w", err)
	}

	// Step 2: Handle match scenarios
	switch len(matches) {
	case 0:
		return nil, cache.ErrModelNotFound{ModelName: req.ModelName}
	case 1:
		// Cloud models have no /api/show-equivalent backend; the show envelope
		// is a local-runtime detail surface. Direct callers to /v1/models/<id>.
		if matches[0].IsCloud {
			return nil, newProblemError(http.StatusNotImplemented, "Not Implemented",
				"show is not supported for cloud-served models; use GET /v1/models/"+matches[0].Name+" for cloud catalog details")
		}
		return s.getModelDetails(ctx, matches[0], req.Verbose)
	default:
		// Multiple matches
		matchStrings := make([]string, len(matches))
		for i, m := range matches {
			matchStrings[i] = fmt.Sprintf("%s@%s", m.Name, m.Node)
		}
		return nil, ErrMultipleMatches{
			ModelName: req.ModelName,
			Matches:   matchStrings,
		}
	}
}

// getModelDetails routes the show request to the appropriate host
func (s *ModelService) getModelDetails(ctx context.Context, model *cache.CachedModel, verbose bool) (*ShowModelResponse, error) {
	utils.LogDebugf("[PublicAPI] Getting details for model=%s from node=%s", model.Name, model.Node)

	// Build routing request to internal API
	path := fmt.Sprintf("/zzrouter/v1/internal/models/show?model=%s&verbose=%v", url.QueryEscape(model.Name), verbose)
	routingReq := &routing.Request{
		Path:   path,
		Node:   model.Node, // Route to specific host
		Method: "GET",
	}

	// Route the request
	resp, err := s.router.Route(ctx, routingReq)
	if err != nil {
		return nil, fmt.Errorf("routing failed: %w", err)
	}

	if resp.StatusCode != 200 {
		// Preserve upstream status + Problem Details so RespondToError forwards 4xx verbatim instead of 500ing.
		return nil, parseRoutedError(resp.StatusCode, resp.Body)
	}

	// Parse response (raw provider response)
	var details map[string]any
	if err := json.Unmarshal(resp.Body, &details); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Convert to ShowModelResponse
	return s.mapToShowResponse(details), nil
}

// mapToShowResponse converts raw provider response to ShowModelResponse
func (s *ModelService) mapToShowResponse(m map[string]any) *ShowModelResponse {
	resp := &ShowModelResponse{
		Details: m, // Store ALL provider fields
	}

	// Extract common fields for convenience
	if v, ok := m["name"].(string); ok {
		resp.Name = v
	}
	if v, ok := m["size"].(float64); ok {
		resp.Size = int64(v)
	}
	if v, ok := m["node"].(string); ok {
		resp.Node = v
	}
	if v, ok := m["source_repo"].(string); ok {
		resp.SourceRepo = v
	}
	if v, ok := m["format"].(string); ok {
		resp.Format = v
	}
	if v, ok := m["modified_at"].(string); ok {
		resp.ModifiedAt = v
	}
	if v, ok := m["assigned_app"].(string); ok {
		resp.AssignedApp = v
	}
	if v, ok := m["digest"].(string); ok {
		resp.Digest = v
	}

	return resp
}

// DeleteModels handles deleting models with routing
func (s *ModelService) DeleteModels(ctx context.Context, req *DeleteModelsRequest) (*DeleteModelsResponse, error) {
	utils.LogDebugf("[PublicAPI] DeleteModels: pattern=%s, paths=%d, models=%d",
		req.Pattern, len(req.Paths), len(req.Models))

	ctx, cancel := ensureTimeout(ctx, 60*time.Second)
	defer cancel()

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

	// Parse response
	var deleteResp DeleteModelsResponse
	if err := json.Unmarshal(resp.Body, &deleteResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Invalidate cache after deletion
	if deleteResp.Deleted > 0 {
		s.cache.Invalidate()
		utils.LogDebugf("[PublicAPI] Cache invalidated after %d models deleted", deleteResp.Deleted)

		// Auto-route sync: update routes for deleted models
		if s.onModelDelete != nil {
			for _, m := range req.Models {
				s.onModelDelete(m.Name)
			}
			if req.Pattern != "" {
				s.onModelDelete(req.Pattern)
			}
		}
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
		result, err := s.registry.RescanLocal()
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
		if resp.StatusCode < http.StatusBadRequest {
			s.cache.Invalidate()
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
	if resp.StatusCode < http.StatusBadRequest {
		s.cache.Invalidate()
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

// deleteModelsMultiNode handles deletion where each model has its own target host
// Groups models by host, routes to each host, and aggregates results
func (s *ModelService) deleteModelsMultiNode(ctx context.Context, models []ModelToDelete) (*routing.Response, error) {
	hostname := s.host.GetNodename()

	// Group models by host
	modelsByNode := make(map[string][]ModelToDelete)

	for _, model := range models {
		host := model.Node
		// Normalize: empty host or matching hostname means local
		// Use the server's actual hostname so router's nodeName check matches
		if host == "" || host == hostname {
			host = hostname
		}
		modelsByNode[host] = append(modelsByNode[host], model)
	}

	utils.LogDebugf("[PublicAPI] Grouped %d models into %d hosts", len(models), len(modelsByNode))

	// Build request body for each host
	hostBodies := make(map[string][]byte, len(modelsByNode))
	for host, hostModels := range modelsByNode {
		hostReq := &DeleteModelsRequest{Models: hostModels}
		body, err := json.Marshal(hostReq)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request for host %s: %w", host, err)
		}
		hostBodies[host] = body
	}

	// Route to all hosts using MultiRoute
	multiResp, err := s.router.MultiRoute(ctx, "/zzrouter/v1/internal/models", "DELETE", hostBodies)
	if err != nil {
		return nil, fmt.Errorf("multi-route failed: %w", err)
	}

	// Aggregate responses from all hosts
	return s.aggregateDeleteResponses(multiResp)
}

// aggregateDeleteResponses aggregates DELETE responses from multiple hosts
// This is operation-specific aggregation logic in the service layer
func (s *ModelService) aggregateDeleteResponses(multiResp *routing.Response) (*routing.Response, error) {
	// Parse multi-route response (array of host responses)
	var hostResponses []map[string]any
	if err := json.Unmarshal(multiResp.Body, &hostResponses); err != nil {
		return nil, fmt.Errorf("failed to parse multi-route response: %w", err)
	}

	// Aggregate deletion results
	totalDeleted := 0
	var allErrors []string

	for _, hostResp := range hostResponses {
		host := "(unknown)"
		if h, ok := hostResp["node"].(string); ok {
			host = h
		}

		// Check for routing errors
		if errMsg, ok := hostResp["error"].(string); ok {
			allErrors = append(allErrors, fmt.Sprintf("%s: %s", host, errMsg))
			continue
		}

		// Parse the body from this host
		body, ok := hostResp["body"].(map[string]any)
		if !ok {
			allErrors = append(allErrors, fmt.Sprintf("%s: invalid response format", host))
			continue
		}

		// Extract deleted count (JSON numbers are float64)
		if deleted, ok := body["deleted"].(float64); ok {
			totalDeleted += int(deleted)
		} else {
			// Log warning but continue - might be zero
			utils.LogDebugf("[PublicAPI] Warning: No 'deleted' field in response from %s", host)
		}

		// Extract errors array if present
		if errors, ok := body["errors"].([]any); ok {
			for _, e := range errors {
				if errStr, ok := e.(string); ok {
					allErrors = append(allErrors, errStr)
				}
			}
		}
	}

	// Build aggregated response
	aggregated := DeleteModelsResponse{
		Deleted: totalDeleted,
	}
	if len(allErrors) > 0 {
		aggregated.Errors = allErrors
	}

	body, err := json.Marshal(aggregated)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal aggregated response: %w", err)
	}

	return &routing.Response{
		StatusCode: 200,
		Body:       body,
	}, nil
}

// deleteModelsStandard handles pattern/path-based deletion
// Uses standard Route for single host or broadcast
func (s *ModelService) deleteModelsStandard(ctx context.Context, req *DeleteModelsRequest) (*routing.Response, error) {
	requestBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
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

	inst, found := s.instanceProvider.GetInstanceByModel(modelName)
	if !found {
		NotFound(c, fmt.Sprintf("model '%s' not loaded", modelName))
		return
	}

	respondSuccess(c, "Model instance retrieved", gin.H{
		"model":    modelName,
		"instance": inst.ID,
		"port":     inst.Port,
		"status":   string(inst.GetStatus()),
		"provider": inst.Provider,
	})
}
