package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// ValidateFilesRequest represents a file validation request.
type ValidateFilesRequest struct {
	Files []instance.FileMount `json:"files" binding:"required"`
}

// RunsController handles HTTP requests for run-related operations.
type RunsController struct {
	service     *RunsService
	loadService *LoadService
	utility     *RunsUtilityService
	nodes       NodeValidator
	logs        RunLogHandler
	appsConfig  func() *pkgConfig.AppsConfig
	idem        *idempotencyStore
}

// NewRunsController creates a new runs controller.
func NewRunsController(
	service *RunsService,
	loadService *LoadService,
	utility *RunsUtilityService,
	nodes NodeValidator,
	logs RunLogHandler,
	appsConfig func() *pkgConfig.AppsConfig,
) *RunsController {
	return &RunsController{
		service:     service,
		loadService: loadService,
		utility:     utility,
		nodes:       nodes,
		logs:        logs,
		appsConfig:  appsConfig,
		idem:        newIdempotencyStore(),
	}
}

// RegisterPublicRoutes registers public run routes.
func (ctrl *RunsController) RegisterPublicRoutes(router *gin.RouterGroup) {
	// Broadcast
	router.GET("/runs", ctrl.ListRuns)

	// Action (?node=)
	router.GET("/runs/:id", ctrl.GetRun)
	router.GET("/runs/:id/logs", ctrl.GetRunLogs)
	router.GET("/runs/:id/health", ctrl.GetRunHealth)
	router.GET("/runs/:id/probes", ctrl.GetRunProbes)
	// Launch + ensure carry agent retry weight; gate on Idempotency-Key
	// so a retried timeout doesn't double-launch.
	router.POST("/runs", ctrl.idem.middleware(), ctrl.LaunchRun)
	router.POST("/runs/ensure", ctrl.idem.middleware(), ctrl.EnsureRun)
	router.DELETE("/runs/:id", ctrl.StopRun)
	router.POST("/runs/:id/restart", ctrl.RestartRun)

	// Load (Cache-aware)
	router.POST("/runs/load", ctrl.LoadModel)
	router.POST("/runs/preview", ctrl.PreviewRun)

	// Local configuration and utilities
	router.GET("/runs/capabilities", ctrl.ListCapabilities)
	router.GET("/runs/providers/:provider", ctrl.GetAppCapabilities)
	router.GET("/runs/providers/:provider/parameters", ctrl.GetAppParameters)
	router.POST("/runs/validate/files", ctrl.ValidateFiles)
	router.GET("/runs/examples/file-mounts", ctrl.GetFileMountExamples)
	router.GET("/runs/metrics", ctrl.GetMetrics)
	router.GET("/runs/config", ctrl.GetConfig)
	router.PATCH("/runs/config", ctrl.UpdateConfig)
	router.GET("/runs/ports", ctrl.ListAllocatedPorts)
	router.POST("/runs/batch", ctrl.BatchLaunch)
	router.POST("/runs/validate/config", ctrl.ValidateConfig)
}

// ============================================================================
// Routed endpoints (use service layer + cluster routing)
// ============================================================================

// ListRuns handles GET /zzrouter/runs.
func (ctrl *RunsController) ListRuns(c *gin.Context) {
	req := &ListRunsRequest{
		Node:   QueryNode(c),
		Status: c.Query("status"),
	}

	if !ctrl.nodes.IsKnownNode(req.Node) {
		NotFound(c, fmt.Sprintf("unknown node %q; known nodes: %v", req.Node, ctrl.nodes.KnownNodes()))
		return
	}

	resp, err := ctrl.service.ListRuns(c.Request.Context(), req)
	if err != nil {
		RespondToError(c, err)
		return
	}

	respondList(c, resp.Data, resp.Total, resp.HasMore)
}

// GetRun handles GET /zzrouter/runs/:id.
func (ctrl *RunsController) GetRun(c *gin.Context) {
	req := &GetRunRequest{
		RunID: c.Param("id"),
		Node:  QueryNode(c),
	}

	resp, err := ctrl.service.GetRun(c.Request.Context(), req)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			NotFound(c, "run not found: "+req.RunID)
			return
		}
		RespondToError(c, err)
		return
	}

	respondSuccess(c, "Run retrieved", resp)
}

// GetRunLogs handles GET /zzrouter/runs/:id/logs.
func (ctrl *RunsController) GetRunLogs(c *gin.Context) {
	ctrl.logs.HandleGetRunLogsPublic(c)
}

// GetRunProbes handles GET /zzrouter/runs/:id/probes.
func (ctrl *RunsController) GetRunProbes(c *gin.Context) {
	ctrl.logs.HandleGetRunProbesPublic(c)
}

// GetRunHealth handles GET /zzrouter/runs/:id/health.
func (ctrl *RunsController) GetRunHealth(c *gin.Context) {
	req := &GetRunHealthRequest{
		RunID: c.Param("id"),
		Node:  QueryNode(c),
	}

	resp, err := ctrl.service.GetRunHealth(c.Request.Context(), req)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			NotFound(c, "run not found: "+req.RunID)
			return
		}
		RespondToError(c, err)
		return
	}

	respondSuccess(c, "Run health retrieved", resp)
}

// LaunchRun handles POST /zzrouter/runs.
func (ctrl *RunsController) LaunchRun(c *gin.Context) {
	var req LaunchRunRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	resp, err := ctrl.service.LaunchRun(c.Request.Context(), &req)
	if err != nil {
		RespondToError(c, err)
		return
	}

	// Async contract: 202 + {accepted, id, job_id, ...}. Agents subscribe
	// via /zzrouter/v1/jobs/:job_id/stream?node=<node> for launch events.
	// Cancel in-flight launch with DELETE /jobs/:job_id; once the run is
	// Running the launch job is terminal — use DELETE /runs/:id to stop
	// the instance.
	respondAccepted(c, "Run launch accepted; subscribe to job_id for readiness", resp)
}

// EnsureRun handles POST /zzrouter/v1/runs/ensure. Idempotent
// warm-up: returns 200 with running-instance details when the model
// is already hot, 202 + job ids when a launch (and possibly a
// download) is needed. Singleflight at the manager layer dedupes
// concurrent ensures by construction.
func (ctrl *RunsController) EnsureRun(c *gin.Context) {
	var req EnsureRunRequest
	if !BindJSONStrict(c, &req) {
		return
	}
	resp, err := ctrl.service.EnsureRun(c.Request.Context(), &req)
	if err != nil {
		RespondToError(c, err)
		return
	}
	if resp.Status == "running" {
		respondSuccess(c, "Model already running", resp)
		return
	}
	respondAccepted(c, "Ensure accepted; subscribe to job_id (and deploy_job_id if set) for readiness", resp)
}

// StopRun handles DELETE /zzrouter/runs/:id.
func (ctrl *RunsController) StopRun(c *gin.Context) {
	req := &StopRunRequest{
		RunID: c.Param("id"),
		Node:  QueryNode(c),
	}

	resp, err := ctrl.service.StopRun(c.Request.Context(), req)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			NotFound(c, "run not found: "+req.RunID)
			return
		}
		RespondToError(c, err)
		return
	}

	respondSuccess(c, "Run stopped", resp)
}

// RestartRun handles POST /zzrouter/runs/:id/restart.
func (ctrl *RunsController) RestartRun(c *gin.Context) {
	req := &RestartRunRequest{
		RunID: c.Param("id"),
		Node:  QueryNode(c),
	}

	resp, err := ctrl.service.RestartRun(c.Request.Context(), req)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			NotFound(c, "run not found: "+req.RunID)
			return
		}
		RespondToError(c, err)
		return
	}

	respondAccepted(c, "Run restart accepted; subscribe to job_id of the new instance", resp)
}

// LoadModel handles POST /zzrouter/runs/load.
func (ctrl *RunsController) LoadModel(c *gin.Context) {
	var req LoadModelRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	resp, err := ctrl.loadService.LoadModel(c.Request.Context(), &req)
	if err != nil {
		RespondToError(c, err)
		return
	}

	// Endpoint providers (cloud/service that LoadModel forwards to
	// directly) complete synchronously and lack a job_id. Local on-demand
	// launches return 202 + job_id for async subscribe; the service layer
	// maps status codes, so mirror the upstream status here.
	if resp.StatusCode == http.StatusConflict {
		c.JSON(http.StatusConflict, SuccessResponse{
			Success: true,
			Message: resp.Message,
			Data:    resp,
		})
		return
	}
	if resp.JobID != "" {
		respondAccepted(c, "Model launch accepted; subscribe to job_id for readiness", resp)
		return
	}
	respondSuccess(c, resp.Message, resp)
}

// PreviewRun handles POST /zzrouter/runs/preview.
func (ctrl *RunsController) PreviewRun(c *gin.Context) {
	var req PreviewRunRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	// Fail fast on unknown or disabled providers so the response is a
	// structured 400, not a 200 + all-empty PreviewRunResponse from a
	// worker that can't resolve the provider either. The worker has its
	// own equivalent gate via isProviderEnabled in PreviewLocalRun for
	// the drift case (coord/worker appsConfig divergence under peer-sync).
	// Preview renders one provider's launch command, so an omitted
	// provider is a client error, not something to auto-detect. It used
	// to reach the executor and come back as an opaque 500.
	if req.Provider == "" {
		BadRequest(c, "provider is required: preview renders the launch command for one provider: "+
			"GET /zzrouter/v1/runs/capabilities lists the providers this cluster can run")
		return
	}
	svc, ok := ctrl.appsConfig().LookupApp(req.Provider)
	if !ok || !svc.IsEnabled() {
		BadRequest(c, fmt.Sprintf("provider %q is not configured or enabled", req.Provider))
		return
	}

	resp, err := ctrl.loadService.PreviewRun(c.Request.Context(), &req)
	if err != nil {
		RespondToError(c, err)
		return
	}

	respondSuccess(c, "Run preview", resp)
}

// ============================================================================
// Local utility endpoints (delegate to RunsUtilityService)
// ============================================================================

// ListCapabilities handles GET /zzrouter/runs/capabilities.
func (ctrl *RunsController) ListCapabilities(c *gin.Context) {
	data, err := ctrl.utility.ListCapabilities()
	if err != nil {
		ServiceUnavailable(c, err.Error())
		return
	}
	respondSuccess(c, "Runs capabilities retrieved", data)
}

// GetAppCapabilities handles GET /zzrouter/runs/providers/:provider.
func (ctrl *RunsController) GetAppCapabilities(c *gin.Context) {
	params := ValidateParams(c, []ParamRule{RequiredPathParam("provider")})
	if params == nil {
		return
	}

	data, err := ctrl.utility.GetAppCapabilities(params.GetString("provider"))
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	respondSuccess(c, "Provider capabilities retrieved", data)
}

// GetAppParameters handles GET /zzrouter/runs/providers/:provider/parameters.
func (ctrl *RunsController) GetAppParameters(c *gin.Context) {
	params := ValidateParams(c, []ParamRule{RequiredPathParam("provider")})
	if params == nil {
		return
	}

	resp, err := ctrl.utility.GetAppParameters(params.GetString("provider"))
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	respondSuccess(c, "Provider parameters retrieved", resp)
}

// ValidateFiles handles POST /zzrouter/runs/validate/files.
func (ctrl *RunsController) ValidateFiles(c *gin.Context) {
	var req ValidateFilesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, "Invalid request format: "+err.Error())
		return
	}
	respondSuccess(c, "File validation completed", ctrl.utility.ValidateFiles(req.Files))
}

// GetFileMountExamples handles GET /zzrouter/runs/examples/file-mounts.
func (ctrl *RunsController) GetFileMountExamples(c *gin.Context) {
	params := ValidateParams(c, []ParamRule{
		OptionalQueryParam("provider"),
		OptionalQueryParam("use_case"),
	})
	if params == nil {
		return
	}
	respondSuccess(c, "File mount examples retrieved",
		ctrl.utility.GetFileMountExamples(params.GetString("provider"), params.GetString("use_case")))
}

// GetMetrics handles GET /zzrouter/runs/metrics.
func (ctrl *RunsController) GetMetrics(c *gin.Context) {
	data, err := ctrl.utility.GetMetrics()
	if err != nil {
		ServiceUnavailable(c, err.Error())
		return
	}
	respondSuccess(c, "Runs metrics retrieved", data)
}

// GetConfig handles GET /zzrouter/runs/config.
func (ctrl *RunsController) GetConfig(c *gin.Context) {
	data, err := ctrl.utility.GetConfig()
	if err != nil {
		ServiceUnavailable(c, err.Error())
		return
	}
	respondSuccess(c, "Runs config retrieved", data)
}

// UpdateConfig handles PATCH /zzrouter/runs/config.
func (ctrl *RunsController) UpdateConfig(c *gin.Context) {
	NotImplemented(c, "runs config update is not yet implemented")
}

// ListAllocatedPorts handles GET /zzrouter/runs/ports.
func (ctrl *RunsController) ListAllocatedPorts(c *gin.Context) {
	data, err := ctrl.utility.ListAllocatedPorts()
	if err != nil {
		ServiceUnavailable(c, err.Error())
		return
	}
	respondSuccess(c, "Allocated ports retrieved", data)
}

// BatchLaunch handles POST /zzrouter/runs/batch.
func (ctrl *RunsController) BatchLaunch(c *gin.Context) {
	var req BatchLaunchRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	launched, failed, err := ctrl.utility.BatchLaunch(c.Request.Context(), req.Instances)
	if err != nil {
		ServiceUnavailable(c, err.Error())
		return
	}
	respondAccepted(c, "Batch launch completed", gin.H{"launched": launched, "failed": failed})
}

// ValidateConfig handles POST /zzrouter/runs/validate/config.
func (ctrl *RunsController) ValidateConfig(c *gin.Context) {
	var req struct {
		Provider   string `json:"provider" binding:"required"`
		LaunchMode string `json:"launch_mode" binding:"required,oneof=native"`
	}
	if !BindJSONStrict(c, &req) {
		return
	}

	data, err := ctrl.utility.ValidateConfig(req.Provider, req.LaunchMode)
	if err != nil {
		BadRequest(c, err.Error())
		return
	}
	respondSuccess(c, "Config validation completed", data)
}
