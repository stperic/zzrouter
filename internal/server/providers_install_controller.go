package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
)

// ============================================================================
// Lifecycle Handlers (delegated to ProvidersService)
// ============================================================================

// GetProviderStatus handles GET /zzrouter/v1/providers/:name/status
func (ctrl *ProvidersController) GetProviderStatus(c *gin.Context) {
	name := c.Param("name")
	node := QueryNode(c)

	status, err := ctrl.providersService.GetProviderStatus(c.Request.Context(), name, node)
	if err != nil {
		RespondToLookupError(c, err)
		return
	}

	respondSuccess(c, "Provider status retrieved", status)
}

// InstallProvider handles POST /zzrouter/v1/providers/:name/install
func (ctrl *ProvidersController) InstallProvider(c *gin.Context) {
	name := c.Param("name")
	var req installRequest
	if !BindJSONStrictOptional(c, &req) {
		return
	}
	node := QueryNode(c)

	result, err := ctrl.providersService.InstallProvider(c.Request.Context(), name, req.Version, node, req.Force, req.PlanOptions)
	if err != nil {
		RespondToError(c, err)
		return
	}

	// Do NOT call FinalizeOnboarding here. The target node's internal
	// install handler is the owner of the session's terminal-success
	// hook and fires finalize inside the async goroutine before the
	// terminal Done event. Forging enabled=true on the coordinator
	// here would make it believe the provider is installed the moment
	// the async job was accepted — not when it actually completed.

	// Async contract: upstream returned 202 + {accepted, job_id,
	// provider, node}. Status 202 so agents see the consistent
	// "accepted → subscribe to job_id" flow across /deployments,
	// /update/apply, /install, /upgrade, DELETE.
	respondAccepted(c, "Install accepted; subscribe to job_id for progress", result)
}

// GetStepStatus handles GET /zzrouter/v1/providers/:name/install/status
// Returns the current install progress on the target node. Routes to the
// worker via providersService so a coordinator can surface progress for
// installs running on remote nodes. Designed for TUI polling (~500ms).
func (ctrl *ProvidersController) GetStepStatus(c *gin.Context) {
	name := c.Param("name")
	node := QueryNode(c)

	progress, err := ctrl.providersService.GetInstallStatus(c.Request.Context(), name, node)
	if err != nil {
		RespondToError(c, err)
		return
	}
	if progress == nil {
		respondSuccess(c, "No install in progress", nil)
		return
	}
	respondSuccess(c, "Install progress", progress)
}

// PreflightInstall handles POST /zzrouter/v1/providers/:name/install/preflight
// Checks system prerequisites (python, GPU, curl, etc.) on the target node.
func (ctrl *ProvidersController) PreflightInstall(c *gin.Context) {
	name := c.Param("name")
	node := QueryNode(c)

	report, err := ctrl.providersService.PreflightInstall(c.Request.Context(), name, node)
	if err != nil {
		RespondToError(c, err)
		return
	}

	respondSuccess(c, "Preflight completed", report)
}

// GetInstallPlan handles POST /zzrouter/v1/providers/:name/install/plan
func (ctrl *ProvidersController) GetInstallPlan(c *gin.Context) {
	name := c.Param("name")
	var req installRequest
	if !BindJSONStrictOptional(c, &req) {
		return
	}
	node := QueryNode(c)

	plan, err := ctrl.providersService.GetInstallPlan(c.Request.Context(), name, req.Version, node, req.PlanOptions)
	if err != nil {
		RespondToError(c, err)
		return
	}

	respondSuccess(c, "Install plan generated", plan)
}

// VerifyInstallStep handles POST /zzrouter/v1/providers/:name/install/verify-step
func (ctrl *ProvidersController) VerifyInstallStep(c *gin.Context) {
	name := c.Param("name")
	var req verifyStepRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	if req.Step < 0 {
		BadRequest(c, "step must be >= 0")
		return
	}
	node := QueryNode(c)

	result, err := ctrl.providersService.VerifyInstallStep(c.Request.Context(), name, req.Version, req.Step, node, req.PlanOptions)
	if err != nil {
		RespondToError(c, err)
		return
	}

	respondSuccess(c, "Install step verified", result)
}

// ExecuteInstallStep handles POST /zzrouter/v1/providers/:name/install/execute-step.
// Async: upstream returns 202 + job_id. Subscribe to /jobs/:id/stream
// for progress and the terminal StepResult.
func (ctrl *ProvidersController) ExecuteInstallStep(c *gin.Context) {
	name := c.Param("name")
	var req verifyStepRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	if req.Step < 0 {
		BadRequest(c, "step must be >= 0")
		return
	}
	node := QueryNode(c)

	result, err := ctrl.providersService.ExecuteInstallStep(c.Request.Context(), name, req.Version, req.Step, node, req.PlanOptions)
	if err != nil {
		RespondToError(c, err)
		return
	}

	respondAccepted(c, "Install step accepted; subscribe to job_id for progress", result)
}

// VerifyInstall handles POST /zzrouter/v1/providers/:name/install/verify
func (ctrl *ProvidersController) VerifyInstall(c *gin.Context) {
	name := c.Param("name")
	var req verifyRequest
	if !BindJSONStrictOptional(c, &req) {
		return
	}
	node := QueryNode(c)

	result, err := ctrl.providersService.VerifyInstall(c.Request.Context(), name, req.Version, node, install.PlanOptions{Runtime: req.Runtime})
	if err != nil {
		RespondToError(c, err)
		return
	}

	respondSuccess(c, "Install verification completed", result)
}

// UpgradeProvider handles POST /zzrouter/v1/providers/:name/upgrade
func (ctrl *ProvidersController) UpgradeProvider(c *gin.Context) {
	name := c.Param("name")
	var req installRequest
	if !BindJSONStrictOptional(c, &req) {
		return
	}
	node := QueryNode(c)

	result, err := ctrl.providersService.UpgradeProvider(c.Request.Context(), name, req.Version, node, req.PlanOptions)
	if err != nil {
		RespondToError(c, err)
		return
	}

	respondAccepted(c, "Upgrade accepted; subscribe to job_id for progress", result)
}

// UninstallProvider handles DELETE /zzrouter/v1/providers/:name
func (ctrl *ProvidersController) UninstallProvider(c *gin.Context) {
	name := c.Param("name")
	node := QueryNode(c)

	// Local providers need lifecycle uninstall; cloud providers skip this step
	isCloud := false
	if cfg := ctrl.appsConfig(); cfg != nil {
		if svc, exists := cfg.LookupApp(name); exists {
			isCloud = svc.IsCloudProvider()
		}
	}

	if !isCloud {
		result, err := ctrl.providersService.UninstallProvider(c.Request.Context(), name, node)
		if err != nil {
			RespondToError(c, err)
			return
		}
		// Do NOT call FinalizeOffboarding here. The target node's internal
		// uninstall handler owns the session's terminal hook and fires
		// finalize inside the async goroutine before Done.
		respondAccepted(c, "Uninstall accepted; subscribe to job_id for progress", result)
		return
	}

	// Cloud provider — no routed service call, no binary to remove;
	// the coordinator IS the node and finalizes synchronously. Not a
	// job-producing path, so returns the legacy sync envelope.
	if err := ctrl.registrar.FinalizeOffboarding(name); err != nil {
		respondFinalizeErr(c, "disable failed", err)
		return
	}
	respondSuccess(c, "Provider uninstalled", gin.H{"status": "uninstalled", "provider": name})
}

// AddProviderInstance handles POST /zzrouter/v1/providers/instances.
// Creates a runtime provider entry from a type-specific factory, persists
// it to the providers directory, and fires the OnChange listener chain so
// the model registry scans the new backend on its next refresh.
//
// Today the only supported type is "ollama-connect" which produces an
// ExternalProvider with protocol=ollama. The TUI and CLI can grow new
// types (vllm-remote, openai-compatible, …) by adding factories without
// changing this handler's shape.
func (ctrl *ProvidersController) AddProviderInstance(c *gin.Context) {
	var req struct {
		Type     string `json:"type" binding:"required"`
		Name     string `json:"name" binding:"required"`
		Endpoint string `json:"endpoint" binding:"required"`
		Token    string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, "Invalid request body: "+err.Error())
		return
	}
	if msg := validateProviderInstanceName(req.Name); msg != "" {
		BadRequest(c, msg)
		return
	}
	if u, err := url.Parse(req.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		BadRequest(c, "endpoint must be a full URL including scheme and host (e.g. http://host:11434)")
		return
	}

	var provider pkgConfig.Provider
	switch req.Type {
	case "ollama-connect":
		provider = pkgConfig.NewOllamaConnectProvider(req.Name, req.Endpoint, req.Token)
	default:
		BadRequest(c, fmt.Sprintf("unknown provider instance type %q", req.Type))
		return
	}

	if err := ctrl.configStore.AddProviderInstance(req.Name, provider); err != nil {
		if errors.Is(err, pkgConfig.ErrProviderExists) {
			Conflict(c, err.Error())
			return
		}
		BadRequest(c, err.Error())
		return
	}

	// Deliberately do NOT call ctrl.registrar.FinalizeOnboarding here.
	// AddProviderInstance is itself the terminal hook of the
	// external-connect session: the factory constructs the Provider with
	// Enabled=true, the store persists it, and the OnChange listener
	// chain reconciles everything. Calling FinalizeOnboarding on top
	// would be a redundant second mutation of the same flag.

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"name":     req.Name,
			"type":     req.Type,
			"kind":     constants.AppModeExternal,
			"protocol": "ollama",
			"endpoint": req.Endpoint,
			"enabled":  true,
		},
	})
}
