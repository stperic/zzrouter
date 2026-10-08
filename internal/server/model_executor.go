package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stperic/zzrouter/pkg/utils"
)

// InternalExecutor handles direct execution against local providers
// Routing remains with the caller; local mutations invalidate catalog evidence.
type InternalExecutor struct {
	modelDetails       func(string, string, map[string]any) map[string]any
	inventoryStatus    func(context.Context, map[string]string) []cache.InventoryUnavailable
	invalidateCache    func()
	registry           *modelregistry.Registry
	getNodename        func() string
	ollamaDaemon       func() (*backend.Resolved, error)
	httpClient         *http.Client
	getHFConnector     func() *huggingface.Connector
	isCloudProvider    func(string) bool
	configStore        *pkgConfig.AppsConfigStore
	refreshCacheSync   func(context.Context) error
	getResourceMetrics func() any
}

// NewInternalExecutor creates a new internal API executor
func NewInternalExecutor(
	registry *modelregistry.Registry,
	getNodename func() string,
	ollamaDaemon func() (*backend.Resolved, error),
	httpClient *http.Client,
	getHFConnector func() *huggingface.Connector,
	isCloudProvider func(string) bool,
	configStore *pkgConfig.AppsConfigStore,
	refreshCacheSync func(context.Context) error,
	getResourceMetrics func() any,
) *InternalExecutor {
	return &InternalExecutor{
		registry:           registry,
		getNodename:        getNodename,
		ollamaDaemon:       ollamaDaemon,
		httpClient:         httpClient,
		getHFConnector:     getHFConnector,
		isCloudProvider:    isCloudProvider,
		configStore:        configStore,
		refreshCacheSync:   refreshCacheSync,
		getResourceMetrics: getResourceMetrics,
	}
}

// GetLocalModels retrieves models from local providers (Ollama, vLLM, HuggingFace, etc.)
// This is the V2 equivalent of getLocalModels but as a proper module
func (e *InternalExecutor) GetLocalModels(ctx context.Context) ([]map[string]any, error) {
	models, _, err := e.localModelSnapshot(ctx)
	return models, err
}

func (e *InternalExecutor) localModelSnapshot(ctx context.Context) ([]map[string]any, map[string]string, error) {
	utils.LogDebugf("[InternalAPI] GetLocalModels called")

	// Get models from registry
	registry := e.registry
	if registry == nil {
		return nil, nil, fmt.Errorf("model registry not initialized")
	}

	models, failures, err := registry.ModelSnapshot(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list models: %w", err)
	}

	hostname := e.getNodename()

	result := make([]map[string]any, 0, len(models))
	for _, m := range models {
		// Use ToOllamaFormat which handles ALL provider types (not just Ollama)
		// This converts ModelMetadata → map[string]interface{} with proper formatting
		modelMap := m.ToOllamaFormat(hostname)
		result = append(result, modelMap)
	}

	var cfg *pkgConfig.AppsConfig
	if e.configStore != nil {
		cfg = e.configStore.Config()
	}
	result = cache.EnrichLocalItems(result, cfg, e.modelDetails)
	utils.LogDebugf("[InternalAPI] Returning %d local models", len(result))
	return result, failures, nil
}

// ShowLocalModel retrieves detailed model information from the actual provider
// NO cache lookup, NO routing - just call the provider directly
func (e *InternalExecutor) ShowLocalModel(ctx context.Context, modelName string, verbose bool) (map[string]any, error) {
	utils.LogDebugf("[InternalAPI] ShowLocalModel: model=%s, verbose=%v", modelName, verbose)

	// 1. Find model in local registry
	registry := e.registry
	if registry == nil {
		return nil, fmt.Errorf("model registry not initialized")
	}

	models, err := registry.ListAllModels()
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}

	var targetModel *metadata.ModelMetadata
	for _, m := range models {
		if m.Name == modelName {
			targetModel = m
			break
		}
	}

	if targetModel == nil {
		return nil, fmt.Errorf("model '%s' not found in local registry", modelName)
	}

	utils.LogDebugf("[InternalAPI] Found model: %s (source: %s)", targetModel.Name, targetModel.SourceRepo)

	// 2. Call the appropriate provider based on source
	var details map[string]any
	var providerErr error

	switch targetModel.SourceRepo {
	case constants.RepoOllama:
		utils.LogDebugf("[InternalAPI] Calling Ollama API for: %s", targetModel.Name)
		details, providerErr = e.getOllamaModelDetails(ctx, targetModel.Name, verbose)
	case constants.RepoHuggingFace:
		utils.LogDebugf("[InternalAPI] Calling HuggingFace connector for: %s", targetModel.SourceID)
		details, providerErr = e.getHuggingFaceModelDetails(ctx, targetModel.SourceID)
	default:
		utils.LogDebugf("[InternalAPI] Error: show command not supported for %s models (model: %s)", targetModel.SourceRepo, modelName)
		return nil, fmt.Errorf("show command not supported for %s models", targetModel.SourceRepo)
	}

	if providerErr != nil {
		utils.LogDebugf("[InternalAPI] Error from provider for %s: %v", modelName, providerErr)
		return nil, fmt.Errorf("failed to get model details from provider: %w", providerErr)
	}

	// 3. Add metadata
	details["node"] = e.getNodename()
	details["source_repo"] = targetModel.SourceRepo
	details["name"] = targetModel.Name            // Ensure canonical name
	details["_provider"] = targetModel.SourceRepo // Carries provider identity for client formatting.

	utils.LogDebugf("[InternalAPI] Successfully retrieved %d fields from provider", len(details))
	return details, nil
}

// getOllamaModelDetails calls Ollama's /api/show endpoint
func (e *InternalExecutor) getOllamaModelDetails(ctx context.Context, modelName string, verbose bool) (map[string]any, error) {
	slog.Info("[InternalAPI/Ollama] Calling /api/show for", "for", modelName)

	// Get Ollama endpoint from config (required - no default)
	daemon, err := e.ollamaDaemon()
	if err != nil {
		return nil, fmt.Errorf("ollama not configured: %w", err)
	}
	reqBody := map[string]any{
		"name":    modelName,
		"verbose": verbose,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Use context-aware request to support cancellation and timeouts
	req, err := daemon.NewRequest(ctx, http.MethodPost, "/api/show", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode ollama response: %w", err)
	}

	slog.Info("[InternalAPI/Ollama] Success: fields returned", "Success", len(result))
	return result, nil
}

// getHuggingFaceModelDetails calls HuggingFace connector.
func (e *InternalExecutor) getHuggingFaceModelDetails(ctx context.Context, sourceID string) (map[string]any, error) {
	slog.Info("[InternalAPI/HuggingFace] Calling ShowModel for", "for", sourceID)

	hfConnector := e.getHFConnector()
	if hfConnector == nil {
		return nil, fmt.Errorf("huggingface connector not available")
	}

	details, err := hfConnector.ShowModel(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("huggingface request failed: %w", err)
	}

	slog.Info("[InternalAPI/HuggingFace] Success: fields returned", "Success", len(details))
	return details, nil
}

// HandleInternalDeleteModels is the INTERNAL endpoint for deleting models
// This is called by the router to delete models locally
// NO routing logic, NO cache - just execute deletion
//
// Endpoint: DELETE /zzrouter/internal/models
func (e *InternalExecutor) HandleInternalDeleteModels(c *gin.Context) {
	var req DeleteModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, "Invalid request")
		return
	}

	utils.LogDebugf("[InternalAPI] HandleInternalDeleteModels: pattern=%s, models=%d, paths=%d",
		req.Pattern, len(req.Models), len(req.Paths))

	deleted, errors := e.deleteLocal(c, &req)

	utils.LogDebugf("[InternalAPI] Deleted %d models, %d errors", deleted, len(errors))

	// Every node has admission evidence, including workers. A partial deletion
	// also changes that evidence.
	if deleted > 0 {
		if e.invalidateCache != nil {
			e.invalidateCache()
		} else {
			e.registry.InvalidateScanCache()
		}
	}

	// Use struct for consistency (matches DeleteModelsResponse)
	response := DeleteModelsResponse{
		Deleted: deleted,
		Errors:  errors,
	}
	c.JSON(http.StatusOK, response)
}

// deleteLocal deletes models from the local model registry
func (e *InternalExecutor) deleteLocal(c *gin.Context, req *DeleteModelsRequest) (int, []string) {
	if e.registry == nil {
		return 0, []string{"Model registry not initialized"}
	}

	registry := e.registry
	var modelsToDelete []string
	var errors []string

	// Handle specific models array (new format)
	// NOTE: ModelService.MultiRoute ensures only models for this host are sent here
	if len(req.Models) > 0 {
		// Separate cloud and local models to batch cloud deletions
		cloudModelsByProvider := map[string][]string{}
		var cloudModelNames []string

		for _, model := range req.Models {
			if e.isCloudProvider(model.Registry) {
				cloudModelsByProvider[model.Registry] = append(cloudModelsByProvider[model.Registry], model.Name)
				cloudModelNames = append(cloudModelNames, model.Name)
				continue
			}
			if err := registry.DeleteModelByNameAndSource(c.Request.Context(), model.Name, model.Registry); err != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", model.Name, err))
			} else {
				modelsToDelete = append(modelsToDelete, model.Name)
			}
		}

		// Batch-remove all cloud models in a single config write
		if len(cloudModelsByProvider) > 0 {
			if err := e.configStore.RemoveCloudModels(cloudModelsByProvider); err != nil {
				errors = append(errors, fmt.Sprintf("cloud model removal: %v", err))
			} else {
				modelsToDelete = append(modelsToDelete, cloudModelNames...)

				if err := e.refreshCacheSync(c.Request.Context()); err != nil {
					slog.Warn("Cache refresh after cloud model removal failed", "error", err)
				}
				slog.Info("Cloud models removed", "count", len(cloudModelNames))
			}
		}

		return len(modelsToDelete), errors
	}

	// Handle pattern-based deletion
	if req.Pattern != "" {
		models := registry.FindModelsByPattern(req.Pattern)
		for _, model := range models {
			if err := registry.DeleteModelByNameAndSource(c.Request.Context(), model.Name, model.SourceRepo); err != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", model.Name, err))
			} else {
				modelsToDelete = append(modelsToDelete, model.Name)
			}
		}
		return len(modelsToDelete), errors
	}

	// Handle path-based deletion
	if len(req.Paths) > 0 {
		deleted, errs := registry.DeleteModels(req.Paths)
		for _, err := range errs {
			errors = append(errors, err.Error())
		}
		return deleted, errors
	}

	return 0, []string{"No deletion criteria provided"}
}

// ============================================================================
// Internal HTTP Handlers
// ============================================================================

// HandleInternalListModels is the INTERNAL endpoint for listing models
// Called by the router to get actual model data - NO routing, NO cache
// Endpoint: GET /zzrouter/internal/models
func (e *InternalExecutor) HandleInternalListModels(c *gin.Context) {
	utils.LogDebugf("[InternalAPI] /zzrouter/internal/models called from %s", c.ClientIP())

	models, failures, err := e.localModelSnapshot(c.Request.Context())
	if err != nil {
		utils.LogDebugf("[InternalAPI] Error getting local models: %v", err)
		InternalNodeError(c, "Failed to get local models")
		return
	}

	utils.LogDebugf("[InternalAPI] Returning %d models", len(models))

	resp := gin.H{"models": models}
	if e.inventoryStatus != nil {
		resp["inventory_unavailable"] = e.inventoryStatus(c.Request.Context(), failures)
	}

	// Piggyback resource metrics so the coordinator doesn't need a separate broadcast
	if metrics := e.getResourceMetrics(); metrics != nil {
		resp["resources"] = metrics
	}

	c.JSON(http.StatusOK, resp)
}

// HandleInternalRescanModels is the INTERNAL endpoint that drops the
// node's registry scan cache so the next ListAllModels call re-queries
// every source (Ollama daemon /api/tags, filesystem GGUF scan, etc.).
//
// Endpoint: POST /zzrouter/v1/internal/models/rescan
//
// Called by the coord-side ModelService.handleRescanModels broadcast
// when an operator hits POST /zzrouter/v1/models/scan without (or with
// a non-local) ?node= filter. Without this handler the broadcast 404s
// and Ollama models pulled outside the manager's lifecycle stay
// invisible to the cluster catalog until the next pull-driven
// invalidation — which never comes for daemons started post-boot.
func (e *InternalExecutor) HandleInternalRescanModels(c *gin.Context) {
	utils.LogDebugf("[InternalAPI] /zzrouter/v1/internal/models/rescan called from %s", c.ClientIP())

	if e.registry == nil {
		InternalNodeError(c, "model registry not initialized")
		return
	}
	if e.invalidateCache != nil {
		e.invalidateCache()
	} else {
		e.registry.InvalidateScanCache()
	}

	models, err := e.GetLocalModels(c.Request.Context())
	if err != nil {
		// Invalidation succeeded but the post-invalidate enumerate
		// failed — surface as 5xx so the operator can distinguish
		// "this node truly has zero models" from "the enumerate path
		// errored after we dropped the cache".
		InternalNodeError(c, fmt.Sprintf("post-invalidate enumeration failed: %v", err))
		return
	}
	// Surface a per-source breakdown so an operator (or the verify
	// script) can spot when a downstream connector is dropping
	// entries — e.g. Ollama daemon reports N but the registry
	// returns N-1.
	bySource := make(map[string]int, 4)
	names := make([]string, 0, len(models))
	for _, m := range models {
		src, _ := m["source_repo"].(string)
		if src == "" {
			src = "unknown"
		}
		bySource[src]++
		if n, ok := m["name"].(string); ok {
			names = append(names, n)
		}
	}
	respondSuccess(c, "Rescan completed", gin.H{
		"models_found": len(models),
		"by_source":    bySource,
		"names":        names,
	})
}

// HandleInternalShowModel is the INTERNAL endpoint for showing model details
// Called by the router to get actual model data - NO routing, NO cache
// Endpoint: GET /zzrouter/internal/models/show?model=<name>&verbose=<bool>
func (e *InternalExecutor) HandleInternalShowModel(c *gin.Context) {
	modelName := QueryModel(c)
	verbose := c.Query("verbose") == "true"

	utils.LogDebugf("[InternalAPI] /zzrouter/internal/models/show: model=%s, verbose=%v", modelName, verbose)

	if modelName == "" {
		BadRequest(c, "model parameter is required")
		return
	}

	details, err := e.ShowLocalModel(c.Request.Context(), modelName, verbose)
	if err != nil {
		utils.LogDebugf("[InternalAPI] Error showing model: %v", err)
		NotFound(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, details)
}
