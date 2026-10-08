package server

import (
	"github.com/gin-gonic/gin"
)

// NodesController handles HTTP requests for node-related operations
type NodesController struct {
	service *NodesService
}

// NewNodesController creates a new nodes controller
func NewNodesController(service *NodesService) *NodesController {
	return &NodesController{
		service: service,
	}
}

// RegisterPublicRoutes registers public nodes routes
func (ctrl *NodesController) RegisterPublicRoutes(router *gin.RouterGroup) {
	router.GET("/nodes", ctrl.ListNodes)
	router.GET("/nodes/:name", ctrl.GetNode)
	router.GET("/nodes/compatible", ctrl.ListCompatibleNodes)
}

func (ctrl *NodesController) ListNodes(c *gin.Context) {
	req := &ListNodesRequest{
		Node:    QueryNode(c),
		Refresh: c.Query("refresh") == "true",
	}

	resp, err := ctrl.service.ListNodes(c.Request.Context(), req)
	if err != nil {
		InternalNodeError(c, err.Error())
		return
	}
	respondList(c, resp.Data, resp.Total, resp.HasMore)
}

func (ctrl *NodesController) GetNode(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		BadRequest(c, "node name is required")
		return
	}

	nodeInfo, err := ctrl.service.GetNode(c.Request.Context(), name)
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	respondSuccess(c, "Node retrieved", nodeInfo)
}

func (ctrl *NodesController) ListCompatibleNodes(c *gin.Context) {
	model := QueryModel(c)
	if model == "" {
		BadRequest(c, "model parameter is required")
		return
	}

	// Parse pagination
	pagination, ok := ParsePagination(c)
	if !ok {
		return
	}

	resp, err := ctrl.service.ListCompatibleNodes(c.Request.Context(), &ListCompatibleNodesRequest{
		Model:    model,
		Registry: QueryRegistry(c),
		Provider: QueryProvider(c),
	})
	if err != nil {
		RespondToError(c, err)
		return
	}

	// Apply pagination
	paginatedData, total, hasMore := ApplyPagination(resp.Data, pagination)
	respondList(c, paginatedData, total, hasMore)
}
