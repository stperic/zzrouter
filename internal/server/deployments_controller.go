package server

import (
	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/constants"
)

// DeploymentsController handles HTTP requests for /zzrouter/v1/deployments/* endpoints.
//
// The public surface is a single unified endpoint family: POST /deployments
// returns a Deployment entity, GET/DELETE /deployments operate on tracked
// deployments. Per-node primitives live on the /zzrouter/v1/internal/deployments
// surface.
type DeploymentsController struct {
	service *DeploymentsService
}

// NewDeploymentsController creates a DeploymentsController.
func NewDeploymentsController(service *DeploymentsService) *DeploymentsController {
	return &DeploymentsController{service: service}
}

// RegisterPublicRoutes registers public deployment routes.
func (ctrl *DeploymentsController) RegisterPublicRoutes(router *gin.RouterGroup) {
	router.POST("/deployments", ctrl.Deploy)
	router.GET("/deployments", ctrl.ListDeployments)
	router.DELETE("/deployments", ctrl.StopAllDeployments)
	router.GET("/deployments/:id", ctrl.GetDeployment)
	router.DELETE("/deployments/:id", ctrl.CancelDeployment)
	router.DELETE("/deployments/:id/nodes/:node", ctrl.CancelDeploymentNode)
}

// Deploy handles POST /deployments — always returns a Deployment-shaped 202.
func (ctrl *DeploymentsController) Deploy(c *gin.Context) {
	var req DeployRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	deployment, err := ctrl.service.Deploy(c.Request.Context(), &req)
	if err != nil {
		RespondToError(c, err)
		return
	}

	respondAccepted(c, deployment.Message, deployment)
}

// ListDeployments handles GET /deployments — lists tracked deployments with
// live progress merged. Supports ?active=true to filter terminal entries and
// ?node=X to limit results to deployments that target the named node.
func (ctrl *DeploymentsController) ListDeployments(c *gin.Context) {
	pagination, ok := ParsePagination(c)
	if !ok {
		return
	}
	activeOnly := c.Query("active") == "true"
	nodeFilter := QueryNode(c)

	deployments := ctrl.service.ListDeployments(c.Request.Context(), activeOnly)

	if nodeFilter != "" {
		filtered := make([]*Deployment, 0, len(deployments))
		for _, d := range deployments {
			for _, n := range d.Nodes {
				if n.Node == nodeFilter {
					filtered = append(filtered, d)
					break
				}
			}
		}
		deployments = filtered
	}

	paginated, total, hasMore := ApplyPagination(deployments, pagination)
	respondList(c, paginated, total, hasMore)
}

// GetDeployment handles GET /deployments/:id with live progress merge.
func (ctrl *DeploymentsController) GetDeployment(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		BadRequest(c, "id is required")
		return
	}

	d := ctrl.service.GetDeployment(c.Request.Context(), id)
	if d == nil {
		NotFound(c, "Deployment not found: "+id)
		return
	}

	respondSuccess(c, "Deployment retrieved successfully", d)
}

// CancelDeployment handles DELETE /deployments/:id — cancels a deployment and
// stops every per-node download it spawned.
func (ctrl *DeploymentsController) CancelDeployment(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		BadRequest(c, "id is required")
		return
	}

	if err := ctrl.service.CancelDeployment(c.Request.Context(), id); err != nil {
		NotFound(c, "Deployment not found: "+id)
		return
	}

	respondSuccess(c, "Deployment cancelled successfully", gin.H{
		"id":     id,
		"status": constants.StatusCancelled,
	})
}

// CancelDeploymentNode handles DELETE /deployments/:id/nodes/:node — cancels a
// single node within a deployment. Returns the updated deployment (with live
// progress merged) so callers can see the new aggregate state in one round-trip.
func (ctrl *DeploymentsController) CancelDeploymentNode(c *gin.Context) {
	id := c.Param("id")
	node := c.Param("node")
	if id == "" {
		BadRequest(c, "id is required")
		return
	}
	if node == "" {
		BadRequest(c, "node is required")
		return
	}

	if err := ctrl.service.CancelDeploymentNode(c.Request.Context(), id, node); err != nil {
		RespondToError(c, err)
		return
	}

	d := ctrl.service.GetDeployment(c.Request.Context(), id)
	if d == nil {
		NotFound(c, "Deployment not found: "+id)
		return
	}

	respondSuccess(c, "Deployment node cancelled successfully", d)
}

// StopAllDeployments handles DELETE /deployments — without ?node= it cancels
// every active deployment cluster-wide; with ?node=X it cancels only the slices
// targeting node X across every active deployment.
func (ctrl *DeploymentsController) StopAllDeployments(c *gin.Context) {
	nodeFilter := QueryNode(c)

	if nodeFilter != "" {
		cancelled := ctrl.service.CancelNodeAcrossDeployments(c.Request.Context(), nodeFilter)
		respondSuccess(c, "Node cancelled across deployments", gin.H{
			"node":      nodeFilter,
			"cancelled": cancelled,
		})
		return
	}

	cancelled := ctrl.service.CancelAllDeployments(c.Request.Context())
	respondSuccess(c, "All active deployments cancelled", gin.H{
		"cancelled": cancelled,
	})
}
