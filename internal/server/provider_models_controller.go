package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
)

// ProviderModelsController routes provider files through the coordinator.
type ProviderModelsController struct {
	service *ProvidersService
	nodes   NodeValidator
}

// RegisterPublicRoutes mounts provider model operations independently of runs.
func (ctrl *ProviderModelsController) RegisterPublicRoutes(router *gin.RouterGroup, idempotent idempotentRoute) {
	router.GET("/providers/:name/models", ctrl.List)
	idempotent(router, http.MethodPost, "/providers/:name/models", nil, ctrl.Download)
	router.DELETE("/providers/:name/models", ctrl.Delete)
	idempotent(router.Group("", requestSizeLimitMiddleware(providerWorkflowMaxBytes)), http.MethodPost, "/providers/:name/models/resolve", nil, ctrl.Resolve)
}

// Resolve handles POST /providers/:name/models/resolve.
func (ctrl *ProviderModelsController) Resolve(c *gin.Context) {
	var req providerModelResolveRequest
	if !BindJSONStrict(c, &req) || !ctrl.validateNode(c, req.Node) {
		return
	}
	result, err := ctrl.service.ResolveProviderModels(c.Request.Context(), c.Param("name"), req)
	if err != nil {
		RespondToError(c, err)
		return
	}
	status := http.StatusOK
	for _, item := range result.Requirements {
		if item.JobID != "" {
			status = http.StatusAccepted
			break
		}
	}
	c.JSON(status, gin.H{"data": result})
}

func (ctrl *ProviderModelsController) validateNode(c *gin.Context, node string) bool {
	id := constants.ParseNodeIdentifier(node)
	if id.Nodename == "" || id.IsWildcard() {
		RespondToError(c, httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeRequired), Message: "one named node is required"})
		return false
	}
	if !ctrl.nodes.IsKnownNode(node) {
		RespondToError(c, httperr.Error{Status: http.StatusNotFound, Code: string(httperr.CodeUnknownNode), Message: "unknown node"})
		return false
	}
	return true
}

// List handles GET /providers/:name/models.
func (ctrl *ProviderModelsController) List(c *gin.Context) {
	node := c.Query("node")
	if !ctrl.validateNode(c, node) {
		return
	}
	result, err := ctrl.service.ListProviderModels(c.Request.Context(), c.Param("name"), node, c.Query("category"))
	if err != nil {
		RespondToError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

// Download handles POST /providers/:name/models.
func (ctrl *ProviderModelsController) Download(c *gin.Context) {
	var req providerModelDownloadRequest
	if !BindJSONStrict(c, &req) || !ctrl.validateNode(c, req.Node) {
		return
	}
	result, err := ctrl.service.DownloadProviderModel(c.Request.Context(), c.Param("name"), req)
	if err != nil {
		RespondToError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"data": result})
}

// Delete handles DELETE /providers/:name/models.
func (ctrl *ProviderModelsController) Delete(c *gin.Context) {
	node := c.Query("node")
	if !ctrl.validateNode(c, node) {
		return
	}
	result, err := ctrl.service.DeleteProviderModel(c.Request.Context(), c.Param("name"), node, c.Query("category"), c.Query("name"))
	if err != nil {
		RespondToError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}
