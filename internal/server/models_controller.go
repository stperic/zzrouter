package server

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/observability/logger"
)

// ModelsController handles HTTP requests for model-related operations.
// It splits its delegation between two services: ModelService for catalog
// CRUD (list/show/delete/stats) and ModelSearchService for the discovery
// surface (search across registries + per-model card fetch).
type ModelsController struct {
	service       *ModelService
	searchService *ModelSearchService
	nodes         NodeValidator
}

// NewModelsController creates a new models controller.
func NewModelsController(service *ModelService, searchService *ModelSearchService, nodes NodeValidator) *ModelsController {
	return &ModelsController{
		service:       service,
		searchService: searchService,
		nodes:         nodes,
	}
}

// RegisterPublicRoutes registers public model routes
func (ctrl *ModelsController) RegisterPublicRoutes(router *gin.RouterGroup) {
	router.GET("/models", ctrl.ListModels)
	router.GET("/models/show", ctrl.ShowModel)
	router.DELETE("/models", ctrl.DeleteModels)
	router.GET("/models/stats", ctrl.GetModelStats)
	router.POST("/models/scan", ctrl.RescanModels)
	router.POST("/models/defaults", ctrl.SaveModelDefaults)
	router.GET("/models/card/:provider/*id", ctrl.GetModelCard)
	router.GET("/models/instance/*model", ctrl.GetModelInstance)

	// Search and Cache (closely related to models)
	router.GET("/search", ctrl.Search)
	router.GET("/cache/stats", ctrl.GetCacheStats)
}

// ListModels handles GET /zzrouter/models
func (ctrl *ModelsController) ListModels(c *gin.Context) {
	req := &ListModelsRequest{
		Node:     QueryNode(c),
		Registry: QueryRegistry(c),
		App:      QueryProvider(c),
		Model:    QueryModel(c),
		Refresh:  c.Query("refresh") == "true",
	}

	// Reject unknown ?node= values with 404 instead of silently returning an
	// empty data array — which made agents think "no models" rather than
	// "wrong node".
	if !ctrl.nodes.IsKnownNode(req.Node) {
		NotFound(c, fmt.Sprintf("unknown node %q; known nodes: %v", req.Node, ctrl.nodes.KnownNodes()))
		return
	}

	// Parse pagination
	pagination, ok := ParsePagination(c)
	if !ok {
		return
	}

	logger.Debug("[ModelsController] List request", "node", req.Node, "model", req.Model)

	models, err := ctrl.service.ListModels(c.Request.Context(), req)
	if err != nil {
		logger.Error("[ModelsController] List failed", "error", err)
		RespondToError(c, err)
		return
	}

	// Report the normalised (post-default, post-cap) limit/offset in the
	// response metadata so clients can confirm what was actually applied —
	// raw query strings may have been clamped or defaulted.
	paginatedData, total, hasMore := ApplyPagination(models, pagination)
	respondListWithMetadata(c, paginatedData, total, hasMore, map[string]any{
		"limit":  pagination.Limit,
		"offset": pagination.Offset,
	})
}

// ShowModel handles GET /zzrouter/models/show
func (ctrl *ModelsController) ShowModel(c *gin.Context) {
	modelName := QueryModel(c)
	if modelName == "" {
		BadRequest(c, "model parameter is required")
		return
	}

	req := &ShowModelRequest{
		ModelName: modelName,
		Node:      QueryNode(c),
		App:       QueryProvider(c),
		Verbose:   c.Query("verbose") == "true",
	}

	logger.Debug("[ModelsController] Show request", "model", req.ModelName, "node", req.Node)

	resp, err := ctrl.service.ShowModel(c.Request.Context(), req)
	if err != nil {
		// Error handling with RFC 9457 format
		switch e := err.(type) {
		case cache.ErrModelNotFound:
			NotFound(c, err.Error())
		case ErrMultipleMatches:
			BadRequest(c, err.Error())
		case *RoutedError:
			e.Respond(c)
		default:
			logger.Error("[ModelsController] Show failed", "error", err)
			InternalNodeError(c, "Failed to show model")
		}
		return
	}

	respondSuccess(c, "Model retrieved", resp.Details)
}

// DeleteModels handles DELETE /zzrouter/models
func (ctrl *ModelsController) DeleteModels(c *gin.Context) {
	var req DeleteModelsRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	// Empty body produced an opaque 200 + success:true with the failure
	// hidden in data.errors[]. Agents can't distinguish that from a
	// successful no-op delete. Reject up front with a 400 + actionable
	// detail so retries are well-formed.
	if req.Pattern == "" && len(req.Models) == 0 && len(req.Paths) == 0 {
		BadRequest(c, "no deletion criteria provided; supply 'pattern', 'models', or 'paths' in the request body")
		return
	}

	logger.Debug("[ModelsController] Delete request", "pattern", req.Pattern, "models", len(req.Models))

	resp, err := ctrl.service.DeleteModels(c.Request.Context(), &req)
	if err != nil {
		logger.Error("[ModelsController] Delete failed", "error", err)
		InternalNodeError(c, "Failed to delete models")
		return
	}

	respondSuccess(c, "Models deleted", resp)
}

// GetModelStats handles GET /zzrouter/models/stats
func (ctrl *ModelsController) GetModelStats(c *gin.Context) {
	ctrl.service.handleModelStats(c)
}

// RescanModels handles POST /zzrouter/v1/models/scan
func (ctrl *ModelsController) RescanModels(c *gin.Context) {
	ctrl.service.handleRescanModels(c)
}

// SaveModelDefaults handles POST /zzrouter/models/defaults
func (ctrl *ModelsController) SaveModelDefaults(c *gin.Context) {
	ctrl.service.handleSaveModelDefaults(c)
}

// GetModelCard handles GET /zzrouter/models/card/:provider/*id
func (ctrl *ModelsController) GetModelCard(c *gin.Context) {
	ctrl.searchService.handleGetModelCard(c)
}

// GetModelInstance handles GET /zzrouter/models/instance/:model
func (ctrl *ModelsController) GetModelInstance(c *gin.Context) {
	ctrl.service.handleGetModelInstance(c)
}

// Search handles GET /zzrouter/search
func (ctrl *ModelsController) Search(c *gin.Context) {
	ctrl.searchService.HandleSearchPublic(c)
}

// GetCacheStats handles GET /zzrouter/cache/stats
func (ctrl *ModelsController) GetCacheStats(c *gin.Context) {
	ctrl.service.handleCacheStats(c)
}
