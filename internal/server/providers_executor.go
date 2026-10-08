package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ============================================================================
// Providers Executor - Internal API for Direct Local Execution
// ============================================================================
//
// Unified executor handling both runtime status and lifecycle operations.
// Used by /zzrouter/internal/providers endpoints for cluster-internal communication.
//
// Responsibilities:
// - List local providers (runtime status from Server + lifecycle from ProviderAppManager)
// - Get/update specific provider details
// - Install/upgrade/uninstall providers locally
// - Return install plans and verification results
// - No routing, no orchestration — local execution only

// ProvidersExecutor handles direct local provider operations.
type ProvidersExecutor struct {
	mgr              *prov_apps.ProviderAppManager
	nodeName         string
	getLocalAppsInfo func() []prov_apps.LocalProviderInfo
	appsConfig       func() *pkgConfig.AppsConfig
	configStore      *pkgConfig.AppsConfigStore
	// finalizeOn is the terminal-success hook for any onboarding session
	// on this node (install, execute-step final pass, admin PATCH
	// enabled=true). Backed by *Server.FinalizeOnboarding.
	finalizeOn func(name string) error
	// finalizeOff is the terminal-success hook for teardown sessions on
	// this node (uninstall, admin PATCH enabled=false). Backed by
	// *Server.FinalizeOffboarding.
	finalizeOff func(name string) error
	// republishState pushes this node's snapshot into the cluster view.
	// Backed by *Server.publishSelfSnapshot.
	republishState func()
}

// NewProvidersExecutor creates a new providers executor.
// mgr may be nil when providerAppMgr is not available.
//
// finalizeOn and finalizeOff MUST be non-nil. Every session-terminal
// handler routes through them; a nil hook would turn a wiring bug into
// "session succeeded, provider disabled, HTTP 200". Panics at wire time.
func NewProvidersExecutor(
	mgr *prov_apps.ProviderAppManager,
	nodeName string,
	getLocalAppsInfo func() []prov_apps.LocalProviderInfo,
	appsConfig func() *pkgConfig.AppsConfig,
	configStore *pkgConfig.AppsConfigStore,
	finalizeOn func(name string) error,
	finalizeOff func(name string) error,
	republishState func(),
) *ProvidersExecutor {
	if finalizeOn == nil || finalizeOff == nil {
		panic("ProvidersExecutor: finalizeOn and finalizeOff are required")
	}
	return &ProvidersExecutor{
		mgr: mgr, nodeName: nodeName,
		getLocalAppsInfo: getLocalAppsInfo, appsConfig: appsConfig,
		configStore:    configStore,
		finalizeOn:     finalizeOn,
		finalizeOff:    finalizeOff,
		republishState: republishState,
	}
}

// afterLifecycle wraps a session's terminal hook so the node's published
// snapshot is refreshed before the job reports done.
//
// Install and uninstall used to refresh by accident: they flip config, and
// a config mutation republishes. Upgrade changes no config, so the cluster
// view kept serving the pre-upgrade version indefinitely — a job reported
// done while every read still described the version it replaced.
//
// The republish runs even when the hook fails: the bytes on disk changed
// either way, and the reported state should describe what is actually
// there rather than what was there before the attempt.
func (e *ProvidersExecutor) afterLifecycle(hook func() error) func() error {
	return func() error {
		var err error
		if hook != nil {
			err = hook()
		}
		if e.republishState != nil {
			e.republishState()
		}
		return err
	}
}

// ============================================================================
// Runtime Status Handlers (absorbed from AppsExecutor)
// ============================================================================

// HandleInternalListProviders handles GET /zzrouter/internal/providers
func (e *ProvidersExecutor) HandleInternalListProviders(c *gin.Context) {
	appFilter := c.Query("provider")
	nameFilter := c.Query("name")
	keyFilter := c.Query("key")
	runningFilter := c.Query("running")
	formatFilter := c.Query("format")

	slog.Info("[ProvidersExecutor] ListProviders", "provider", appFilter, "name", nameFilter, "key", keyFilter, "running", runningFilter, "format", formatFilter)

	apps := e.listAppsLocal(appFilter, nameFilter, keyFilter, runningFilter, formatFilter)

	slog.Info("[ProvidersExecutor] Returning providers from host", "count", len(apps), "value", e.nodeName)
	c.JSON(http.StatusOK, gin.H{
		"providers": apps,
	})
}

// listAppsLocal retrieves and filters local apps.
func (e *ProvidersExecutor) listAppsLocal(providerFilter, nameFilter, keyFilter, runningFilter, _ string) []prov_apps.LocalProviderInfo {
	allApps := e.getLocalAppsInfo()

	if providerFilter == "" && nameFilter == "" && keyFilter == "" && runningFilter == "" {
		return allApps
	}

	filtered := make([]prov_apps.LocalProviderInfo, 0, len(allApps))
	for _, app := range allApps {
		if providerFilter != "" && !matchesFilterPattern(app.Type, providerFilter) {
			continue
		}
		if nameFilter != "" && !matchesFilterPattern(app.Name, nameFilter) {
			continue
		}
		if keyFilter != "" && !matchesFilterPattern(app.Key, keyFilter) {
			continue
		}
		if runningFilter != "" && runningFilter != "true" {
			continue
		}
		filtered = append(filtered, app)
	}
	return filtered
}

// HandleInternalGetProvider handles GET /zzrouter/internal/providers/:name.
// Returns the same LocalProviderInfo shape as HandleInternalListProviders
// so State drives enabled/installed via one MarshalJSON, not two.
func (e *ProvidersExecutor) HandleInternalGetProvider(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		BadRequest(c, "provider name is required")
		return
	}

	slog.Info("[ProvidersExecutor] GetProvider", "name", name)

	for _, app := range e.getLocalAppsInfo() {
		if app.Key == name || app.Name == name || app.Type == name {
			slog.Info("[ProvidersExecutor] Returning provider", "provider", name)
			c.JSON(http.StatusOK, app)
			return
		}
	}

	NotFound(c, fmt.Sprintf("provider '%s' not found", name))
}

// HandleInternalUpdateProvider handles PATCH /zzrouter/internal/providers/:name
func (e *ProvidersExecutor) HandleInternalUpdateProvider(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		BadRequest(c, "provider name is required")
		return
	}

	var req struct {
		Enabled       *bool   `json:"enabled"`
		PinnedVersion *string `json:"pinned_version"`
	}
	if !BindJSONOptional(c, &req) {
		return
	}

	if req.Enabled == nil && req.PinnedVersion == nil {
		BadRequest(c, "at least one of enabled or pinned_version is required")
		return
	}

	slog.Info("[ProvidersExecutor] UpdateProvider", "name", name,
		"enabled", req.Enabled, "pinned_version", req.PinnedVersion)

	var cfg *pkgConfig.AppsConfig
	if e.configStore != nil {
		cfg = e.configStore.Config()
	}
	svc := cfg.Find(name)
	if svc == nil {
		NotFound(c, fmt.Sprintf("provider %q not found", name))
		return
	}

	var actions []string

	if req.PinnedVersion != nil {
		if e.configStore == nil {
			respondFinalizeErr(c, "pin failed", fmt.Errorf("providers config store not ready"))
			return
		}
		if err := e.configStore.SetProviderPinnedVersion(name, *req.PinnedVersion); err != nil {
			respondFinalizeErr(c, "pin failed", err)
			return
		}
		if *req.PinnedVersion == "" {
			actions = append(actions, "version pin cleared")
		} else {
			actions = append(actions, fmt.Sprintf("pinned to %s", *req.PinnedVersion))
		}
	}

	// Enabled is applied after the pin so a single call that both pins a
	// version and enables the provider onboards against the new pin.
	enabled := svc.IsEnabled()
	if req.Enabled != nil {
		enabled = *req.Enabled
		if enabled {
			if err := e.finalizeOn(name); err != nil {
				respondFinalizeErr(c, "enable failed", err)
				return
			}
			actions = append(actions, "enabled")
		} else {
			if err := e.finalizeOff(name); err != nil {
				respondFinalizeErr(c, "disable failed", err)
				return
			}
			actions = append(actions, "disabled")
		}
	}

	// Response keys must match the public-side UpdateAppResponse struct
	// (pkg/.../apps_service.go) so routing layer deserialization populates
	// the Name field.
	resp := gin.H{
		"name":    name,
		"enabled": enabled,
		"message": fmt.Sprintf("Provider '%s' %s", name, strings.Join(actions, ", ")),
	}
	if req.PinnedVersion != nil {
		resp["pinned_version"] = *req.PinnedVersion
	}
	c.JSON(http.StatusOK, resp)
}

// ============================================================================
// Lifecycle Handlers
// ============================================================================

// HandleInternalGetInstallStatus handles GET /zzrouter/internal/providers/:name/install/status
// Returns the local install-progress snapshot for a provider. Mirrors the
// public GetStepStatus but is LOCAL-ONLY (no routing) so a coordinator can
// query the worker that's actually running the install.
func (e *ProvidersExecutor) HandleInternalGetInstallStatus(c *gin.Context) {
	name := c.Param("name")
	progress := e.mgr.Install().Progress(name)
	if progress == nil {
		c.JSON(http.StatusOK, nil)
		return
	}
	c.JSON(http.StatusOK, progress.Snapshot())
}

// HandleInternalGetProviderStatus handles GET /zzrouter/internal/providers/:name/status
func (e *ProvidersExecutor) HandleInternalGetProviderStatus(c *gin.Context) {
	name := c.Param("name")
	slog.Info("[ProvidersExecutor] GetProviderStatus", "name", name)

	status, err := e.mgr.ProviderStatus(name)
	if err != nil {
		respondProviderErr(c, err)
		return
	}

	c.JSON(http.StatusOK, status)
}

// HandleInternalInstallProvider handles POST /zzrouter/internal/providers/:name/install
func (e *ProvidersExecutor) HandleInternalInstallProvider(c *gin.Context) {
	name := c.Param("name")
	var req installRequest
	if !BindJSONStrictOptional(c, &req) {
		return
	}

	if e.hasRecipe(name) {
		req.Action = "install"
		plan, err := e.mgr.Install().PrepareInstallPlan(c.Request.Context(), name, req.Version, req.PlanOptions)
		if err != nil {
			respondRecipeErr(c, err)
			return
		}
		jobID, err := e.mgr.Install().ExecuteResolvedAsync(c.Request.Context(), name, plan, req.PlanOptions, -1, e.afterLifecycle(func() error { return e.finalizeOn(name) }))
		if err != nil {
			respondRecipeErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, acceptedJob(jobID, name, e.nodeName, gin.H{"plan_id": plan.PlanID, "runtime": plan.Provider}))
		return
	}
	if req.Smoke != nil {
		BadRequest(c, "smoke requires a typed runtime recipe")
		return
	}
	slog.Info("[ProvidersExecutor] InstallProvider", "provider", name, "version", req.Version)

	// Wrap the request context with a GPU inventory cache so preflight
	// and the downstream install plan share a single hardware probe
	// instead of shelling out to nvidia-smi / rocm-smi / ghw twice.
	ctx := gpu.WithCachedInventory(c.Request.Context())

	// Preflight: check system prerequisites before install
	if inst, err := e.mgr.Install().Installer(name); err == nil {
		reqs := e.mgr.ProviderRequirements(name)
		report := inst.Preflight(ctx, reqs)
		if !report.AllOK {
			respondPreflightFailed(c, report)
			return
		}
	}

	// Async: return 202 + job_id immediately. Actual install runs on a
	// detached goroutine owned by the coordinator; finalizeOn fires
	// before the terminal Done event so "job done" means "config flipped".
	finalize := e.afterLifecycle(func() error { return e.finalizeOn(name) })
	jobID, err := e.mgr.Install().InstallAsync(ctx, prov_apps.InstallRequest{
		Provider: name,
		Version:  req.Version,
		Force:    req.Force,
	}, finalize)
	if err != nil {
		respondProviderErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, acceptedJob(jobID, name, e.nodeName, nil))
}

// HandleInternalUpgradeProvider handles POST /zzrouter/internal/providers/:name/upgrade
func (e *ProvidersExecutor) HandleInternalUpgradeProvider(c *gin.Context) {
	name := c.Param("name")

	var req installRequest
	if !BindJSONStrictOptional(c, &req) {
		return
	}
	if e.hasRecipe(name) {
		req.Action = "upgrade"
		plan, err := e.mgr.Install().PrepareInstallPlan(c.Request.Context(), name, req.Version, req.PlanOptions)
		if err != nil {
			respondRecipeErr(c, err)
			return
		}
		jobID, err := e.mgr.Install().ExecuteResolvedAsync(c.Request.Context(), name, plan, req.PlanOptions, -1, e.afterLifecycle(func() error { return e.finalizeOn(name) }))
		if err != nil {
			respondRecipeErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, acceptedJob(jobID, name, e.nodeName, gin.H{"plan_id": plan.PlanID, "runtime": plan.Provider}))
		return
	}
	if req.Smoke != nil {
		BadRequest(c, "smoke requires a typed runtime recipe")
		return
	}
	slog.Info("[ProvidersExecutor] UpgradeProvider", "name", name, "version", req.Version)

	jobID, err := e.mgr.Install().UpgradeAsync(c.Request.Context(), name, req.Version, e.afterLifecycle(nil))
	if err != nil {
		respondProviderErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, acceptedJob(jobID, name, e.nodeName, nil))
}

// HandleInternalUninstallProvider handles DELETE /zzrouter/internal/providers/:name
func (e *ProvidersExecutor) HandleInternalUninstallProvider(c *gin.Context) {
	name := c.Param("name")
	slog.Info("[ProvidersExecutor] UninstallProvider", "name", name)

	finalize := e.afterLifecycle(func() error { return e.finalizeOff(name) })
	jobID, err := e.mgr.Install().UninstallAsync(c.Request.Context(), name, finalize)
	if err != nil {
		respondProviderErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, acceptedJob(jobID, name, e.nodeName, nil))
}

// HandleInternalPreflightInstall handles POST /zzrouter/internal/providers/:name/install/preflight
func (e *ProvidersExecutor) HandleInternalPreflightInstall(c *gin.Context) {
	name := c.Param("name")

	inst, err := e.mgr.Install().Installer(name)
	if err != nil {
		NotFound(c, err.Error())
		return
	}

	// Get requirements from provider config
	reqs := e.mgr.ProviderRequirements(name)

	report := inst.Preflight(c.Request.Context(), reqs)
	c.JSON(http.StatusOK, report)
}

// HandleInternalGetInstallPlan handles POST /zzrouter/internal/providers/:name/install/plan
func (e *ProvidersExecutor) HandleInternalGetInstallPlan(c *gin.Context) {
	name := c.Param("name")
	var req installRequest
	if !BindJSONStrictOptional(c, &req) {
		return
	}

	plan, err := e.mgr.Install().PrepareInstallPlan(c.Request.Context(), name, req.Version, req.PlanOptions)
	if err != nil {
		respondRecipeErr(c, err)
		return
	}

	// Pre-probe step state so the client can tell which steps are already
	// installed. Framed as CurrentState (not verification) — `installed:false`
	// on a fresh machine is the expected state, not a failure.
	plan.CurrentState = plan.ProbeStateContext(c.Request.Context())

	c.JSON(http.StatusOK, plan)
}

// HandleInternalVerifyInstall handles POST /zzrouter/internal/providers/:name/install/verify
func (e *ProvidersExecutor) HandleInternalVerifyInstall(c *gin.Context) {
	name := c.Param("name")
	var req verifyRequest
	if !BindJSONStrictOptional(c, &req) {
		return
	}

	runtime := name
	if req.Runtime != "" {
		runtime = req.Runtime
	}
	if e.hasRecipe(name) {
		cfg, _ := e.appsConfig().LookupApp(name)
		if _, ok := cfg.Install.Runtimes[runtime]; !ok {
			BadRequest(c, "runtime not declared by provider")
			return
		}
	}
	inst, err := e.mgr.Install().Installer(runtime)
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	if e.hasRecipe(name) {
		result, err := e.verifyInstalledRuntime(c.Request.Context(), name, runtime, inst)
		if err != nil {
			respondRecipeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
		return
	}

	version := e.verifyVersion(runtime, req.Version)
	plan, err := inst.InstallPlan(c.Request.Context(), version)
	if err != nil {
		InternalNodeError(c, err.Error())
		return
	}

	result := plan.VerifyAllContext(c.Request.Context())
	c.JSON(http.StatusOK, result)
}

// HandleInternalExecuteStep handles POST /zzrouter/internal/providers/:name/install/execute-step.
//
// Async: delegates to InstallCoordinator.ExecuteStepAsync and returns
// 202 + {accepted, job_id, provider, node}. The step runs on a
// coordinator goroutine with progress mirrored onto the jobs stream;
// finalizeOn fires inside the goroutine before the terminal Done when
// VerifyAll reports all steps pass. Subscribe to job_id for the
// terminal StepResult (carried in the final event's Meta).
func (e *ProvidersExecutor) HandleInternalExecuteStep(c *gin.Context) {
	name := c.Param("name")
	var req verifyStepRequest
	if !BindJSONStrict(c, &req) {
		return
	}
	if req.Smoke != nil {
		BadRequest(c, "smoke requires automatic installation")
		return
	}
	if req.Step < 0 {
		BadRequest(c, "step must be >= 0")
		return
	}

	if e.hasRecipe(name) {
		if req.ExpectedPlanID == "" {
			BadRequest(c, "expected_plan_id is required for guided execution")
			return
		}
		plan, err := e.mgr.Install().PrepareInstallPlan(c.Request.Context(), name, req.Version, req.PlanOptions)
		if err != nil {
			respondRecipeErr(c, err)
			return
		}
		jobID, err := e.mgr.Install().ExecuteResolvedAsync(c.Request.Context(), name, plan, req.PlanOptions, req.Step, e.afterLifecycle(func() error { return e.finalizeOn(name) }))
		if err != nil {
			respondRecipeErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, acceptedJob(jobID, name, e.nodeName, gin.H{"plan_id": plan.PlanID, "runtime": plan.Provider, "step": req.Step}))
		return
	}
	slog.Info("[ProvidersExecutor] ExecuteStep", "provider", name, "step", req.Step)

	// finalize mirrors the Run All contract: only flip onboarding config
	// when VerifyAll passes. ExecuteStepAsync invokes it after a
	// successful step — finalize itself re-checks plan state and no-ops
	// when earlier steps still haven't run.
	finalize := func() error {
		inst, err := e.mgr.Install().Installer(name)
		if err != nil {
			slog.Warn("[ProvidersExecutor] finalize skipped: installer unavailable", "provider", name, "error", err)
			return nil //nolint:nilerr // the step itself succeeded; failing it here would misreport the step
		}
		version := e.mgr.ResolveVersion(name, req.Version)
		plan, err := inst.InstallPlan(c.Request.Context(), version)
		if err != nil {
			slog.Warn("[ProvidersExecutor] finalize skipped: plan unavailable", "provider", name, "version", version, "error", err)
			return nil //nolint:nilerr // the step itself succeeded; failing it here would misreport the step
		}
		if !plan.VerifyAllContext(c.Request.Context()).AllOK {
			return nil
		}
		return e.finalizeOn(name)
	}

	version := e.mgr.ResolveVersion(name, req.Version)
	jobID, err := e.mgr.Install().ExecuteStepAsync(c.Request.Context(), name, version, req.Step, e.afterLifecycle(finalize))
	if err != nil {
		respondProviderErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, acceptedJob(jobID, name, e.nodeName, gin.H{"step": req.Step}))
}

// HandleInternalVerifyInstallStep handles POST /zzrouter/internal/providers/:name/install/verify-step
func (e *ProvidersExecutor) HandleInternalVerifyInstallStep(c *gin.Context) {
	name := c.Param("name")
	var req verifyStepRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	if req.Step < 0 {
		BadRequest(c, "step must be >= 0")
		return
	}

	var plan *install.Plan
	var err error
	if e.hasRecipe(name) {
		plan, err = e.mgr.Install().PrepareInstallPlan(c.Request.Context(), name, req.Version, req.PlanOptions)
	} else {
		inst, lookupErr := e.mgr.Install().Installer(name)
		if lookupErr != nil {
			NotFound(c, lookupErr.Error())
			return
		}
		plan, err = inst.InstallPlan(c.Request.Context(), e.mgr.ResolveVersion(name, req.Version))
	}
	if err != nil {
		respondRecipeErr(c, err)
		return
	}

	for _, step := range plan.Steps {
		if step.Number == req.Step {
			result := install.VerifyStepContext(c.Request.Context(), step)
			c.JSON(http.StatusOK, result)
			return
		}
	}

	NotFound(c, "step not found")
}

// acceptedJob is the 202 body every provider-lifecycle endpoint returns.
//
// The URLs are spelled out rather than left as a template the caller has to
// assemble, because the primary consumer of this API is an agent that has
// only the response in front of it. "subscribe to job_id" in a message string
// tells it what to do but not where; these tell it where.
//
// Phase reaches "done" or "failed" and nothing else is terminal.
func acceptedJob(jobID, provider, node string, extra gin.H) gin.H {
	out := gin.H{
		"accepted":   true,
		"job_id":     jobID,
		"provider":   provider,
		"node":       node,
		"job_url":    "/zzrouter/v1/jobs/" + jobID,
		"stream_url": "/zzrouter/v1/jobs/" + jobID + "/stream",
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// respondPreflightFailed rejects an install whose prerequisites are not met,
// carrying the failed checks as structured errors.
//
// The report already knows which check failed and, for most of them, the
// command that fixes it. Flattening that into one prose sentence threw the
// hints away and left a caller parsing English to find out it needed curl —
// so the failures ride in the Errors extension, which exists for exactly
// this, and Detail keeps the human summary.
func respondPreflightFailed(c *gin.Context, report *preflight.Report) {
	var failed []utils.ParamError
	var summary []string
	for _, r := range report.Results {
		if r.Passed {
			continue
		}
		summary = append(summary, r.Message)
		failed = append(failed, utils.ParamError{
			Key:     r.Check,
			Code:    string(httperr.CodePreflightFailed),
			Message: r.Message,
			Hint:    r.Hint,
		})
	}

	problem := utils.NewProblemDetails(http.StatusBadRequest, "Bad Request",
		"prerequisites not met: "+strings.Join(summary, "; "), c.Request.URL.Path)
	problem.Code = string(httperr.CodePreflightFailed)
	problem.Errors = failed
	if reqID, exists := c.Get("request_id"); exists {
		if id, ok := reqID.(string); ok {
			problem.RequestID = id
		}
	}
	c.Header("Content-Type", "application/problem+json")
	c.JSON(http.StatusBadRequest, problem)
}

// verifyVersion picks which plan POST /install/verify runs against.
//
// Deliberately not used by verify-step: that endpoint drives a guided
// install step by step, where the target is the version being installed,
// not the one already on disk.
//
// Verifying an install means verifying THE install, so what this node
// actually has wins over the config pin. Defaulting to the pin checked a
// plan for a release that was never installed: after any upgrade past the
// pin, every artifact path in it named the wrong build, and the caller was
// told a healthy install had failed. The pin is only the answer when
// nothing is installed to ask about.
func (e *ProvidersExecutor) verifyVersion(name, requested string) string {
	if requested != "" {
		return requested
	}
	if v := e.mgr.ProviderVersion(name); v != "" && v != "unknown" {
		return v
	}
	return e.mgr.ResolveVersion(name, "")
}

func (e *ProvidersExecutor) hasRecipe(provider string) bool {
	if e.appsConfig == nil || e.appsConfig() == nil {
		return false
	}
	cfg, ok := e.appsConfig().LookupApp(provider)
	return ok && cfg.Install != nil
}

func respondRecipeErr(c *gin.Context, err error) {
	if errors.Is(err, install.ErrStalePlan) {
		Conflict(c, err.Error())
		return
	}
	if errors.Is(err, install.ErrInstallPolicy) {
		RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", []utils.ParamError{{Key: "install", Code: string(httperr.CodePreflightFailed), Message: err.Error()}})
		return
	}
	if errors.Is(err, prov_apps.ErrProviderNotFound) || errors.Is(err, prov_apps.ErrShutdown) || errors.Is(err, prov_apps.ErrInstancesRunning) || errors.Is(err, prov_apps.ErrProviderAlreadyInstalled) || errors.Is(err, install.ErrUnsupportedPlatform) {
		respondProviderErr(c, err)
		return
	}
	RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", []utils.ParamError{{Key: "install", Code: string(httperr.CodePreflightFailed), Message: err.Error()}})
}
