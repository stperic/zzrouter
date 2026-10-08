package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/utils"
	"golang.org/x/sync/singleflight"
)

// ============================================================================
// Runs Executor - Internal API for Direct Local Execution
// ============================================================================
//
// This executor handles direct interaction with the local provider app manager.
// Used by /zzrouter/internal/runs endpoints for cluster-internal communication.
//
// Responsibilities:
// - Query local provider app manager directly
// - No routing, no caching, no orchestration
// - Return raw local data only

// RunsExecutor handles direct local runs operations
type RunsExecutor struct {
	appMgr              *prov_apps.ProviderAppManager
	getNodename         func() string
	appsConfig          func() *pkgConfig.AppsConfig
	backend             *backend.Resolver
	routerAvailable     bool
	streamInstanceLogs  func(*gin.Context, string, *logFilter)
	getInstanceLogLines func(*gin.Context, string, int, *logFilter)
	// refreshResourceMetrics triggers an immediate re-probe of live
	// GPU memory/utilization after an unload completes. Mirrors
	// Ollama's waitForVRAMRecovery pattern: reconcile scheduler
	// state with actual free VRAM at unload events. Nullable.
	refreshResourceMetrics func()
	// modelInPool reports whether a model is already in the deployed
	// pool (Cache.LookupModel hit). Used by the auto_deploy=true path
	// to skip the deploy step when the model is already present.
	// Nullable — when nil, auto-deploy treats every model as missing.
	modelInPool func(name string) bool
	// deploy synthesizes a download via DeploymentsService. Called by
	// the auto_deploy=true path. Returns the per-node job_id when the
	// underlying tracker started a download. Nullable — when nil,
	// auto_deploy=true requests are refused.
	deploy func(ctx context.Context, req *DeployRequest) (*Deployment, error)
	// jobs is the registry the auto-deploy chain uses to mint a run job
	// that waits on the deploy job before invoking LaunchInstance.
	// Nullable — when nil, auto_deploy=true requests are refused.
	jobs *jobs.Registry
	// deployGroup coalesces concurrent auto_deploy on (provider, model, file)
	// onto one Deploy call; each caller still gets its own run_job_id.
	deployGroup singleflight.Group
}

// NewRunsExecutor creates a new runs executor. refreshResourceMetrics
// may be nil (tests, non-cluster deployments); production wiring
// passes a closure over ResourceTracker.CollectNow.
func NewRunsExecutor(
	appMgr *prov_apps.ProviderAppManager,
	getNodename func() string,
	appsConfig func() *pkgConfig.AppsConfig,
	routerAvailable bool,
	streamInstanceLogs func(*gin.Context, string, *logFilter),
	getInstanceLogLines func(*gin.Context, string, int, *logFilter),
	refreshResourceMetrics func(),
) *RunsExecutor {
	return &RunsExecutor{
		appMgr:                 appMgr,
		getNodename:            getNodename,
		appsConfig:             appsConfig,
		backend:                backend.NewResolver(appsConfig),
		routerAvailable:        routerAvailable,
		streamInstanceLogs:     streamInstanceLogs,
		getInstanceLogLines:    getInstanceLogLines,
		refreshResourceMetrics: refreshResourceMetrics,
	}
}

// WithAutoDeploy enables the auto_deploy=true code path on POST /runs.
// modelInPool is the cache hit-test; deploy is the download trigger;
// jobs is the registry the chained run job is opened on. All three must
// be non-nil for auto-deploy to be active. Returns the executor for
// fluent wiring.
func (e *RunsExecutor) WithAutoDeploy(
	modelInPool func(string) bool,
	deploy func(context.Context, *DeployRequest) (*Deployment, error),
	jobsReg *jobs.Registry,
) *RunsExecutor {
	e.modelInPool = modelInPool
	e.deploy = deploy
	e.jobs = jobsReg
	return e
}

// triggerResourceRefresh fires a ResourceTracker refresh in a
// goroutine so the unload response path doesn't block on
// nvidia-smi. Safe to call when the callback is nil.
func (e *RunsExecutor) triggerResourceRefresh() {
	if e.refreshResourceMetrics == nil {
		return
	}
	go e.refreshResourceMetrics()
}

// requireProviderAppMgr checks if the provider app manager is initialized and returns a 503 error if not.
func (e *RunsExecutor) requireProviderAppMgr(c *gin.Context) bool {
	if e.appMgr == nil {
		ServiceUnavailable(c, "provider manager not initialized")
		return false
	}
	return true
}

// GetLocalRuns retrieves all runs from the local provider app manager
// PLUS queries endpoint providers (like Ollama) for their running models.
// withStatus adds each launched run's ParametersStatus.
func (e *RunsExecutor) GetLocalRuns(withStatus bool) ([]instance.InstanceInfo, error) {
	if e.appMgr == nil {
		return nil, fmt.Errorf("provider app manager not initialized")
	}

	hostname := e.getNodename()
	var result []instance.InstanceInfo
	if withStatus {
		for _, inst := range e.appMgr.Instances().List() {
			result = append(result, e.appMgr.InstanceInfo(inst, hostname))
		}
	} else {
		result = e.appMgr.ListInstances(hostname)
	}
	launched := len(result)
	// A model an external daemon serves was not launched from config here,
	// so it has no parameters status to report.
	endpointInstances := e.queryEndpointProviders()
	for _, inst := range endpointInstances {
		result = append(result, inst.ToInfo(hostname))
	}

	utils.LogDebugf("[RunsExecutor] GetLocalRuns: returning %d instances (%d from registry + %d from endpoint providers) from host '%s'",
		len(result), launched, len(endpointInstances), hostname)
	return result, nil
}

// GetLocalRun retrieves a specific run from the local provider app manager
func (e *RunsExecutor) GetLocalRun(runID string) (*instance.InstanceInfo, error) {
	if e.appMgr == nil {
		return nil, fmt.Errorf("provider app manager not initialized")
	}

	inst, exists := e.appMgr.GetInstance(runID)
	if !exists {
		return nil, fmt.Errorf("instance not found: %s", runID)
	}

	info := e.appMgr.InstanceInfo(inst, e.getNodename())
	return &info, nil
}

// HandleInternalListRuns handles GET /zzrouter/internal/runs (internal API)
func (e *RunsExecutor) HandleInternalListRuns(c *gin.Context) {
	runs, err := e.GetLocalRuns(c.Query("include") == includeParametersStatus)
	if err != nil {
		slog.Error("Failed to get local runs", "runs", err)
		InternalNodeError(c, utils.SanitizeErrorMessage(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"instances": runs,
	})
}

// HandleInternalGetRun handles GET /zzrouter/internal/runs/:id (internal API)
func (e *RunsExecutor) HandleInternalGetRun(c *gin.Context) {
	runID := c.Param("id")
	if runID == "" {
		BadRequest(c, "run ID is required")
		return
	}

	run, err := e.GetLocalRun(runID)
	if err != nil {
		slog.Error("Failed to get local run", "run_id", runID, "error", err)
		NotFound(c, utils.SanitizeErrorMessage(err.Error()))
		return
	}

	c.JSON(http.StatusOK, run)
}

// HandleInternalStopRun handles DELETE /zzrouter/internal/runs/:id (internal API)
func (e *RunsExecutor) HandleInternalStopRun(c *gin.Context) {
	if !e.requireProviderAppMgr(c) {
		return
	}

	runID := c.Param("id")
	if runID == "" {
		BadRequest(c, "run ID is required")
		return
	}

	inst, exists := e.appMgr.GetInstance(runID)
	if exists {
		// On-demand instance found in provider app manager — stop it directly
		if err := e.appMgr.StopInstance(c.Request.Context(), runID); err != nil {
			InternalNodeError(c, "failed to stop instance")
			return
		}

		utils.LogDebugf("[RunsExecutor] Stopped instance '%s' (model: %s)", runID, inst.Model)
		e.triggerResourceRefresh()
		c.JSON(http.StatusOK, gin.H{
			"id":      inst.ID,
			"status":  "stopped",
			"message": "Instance stopped successfully",
		})
		return
	}

	// Not in provider app manager — check endpoint providers (e.g. Ollama)
	// The runID for endpoint models is the digest from the provider
	if e.tryUnloadEndpointModel(c, runID) {
		return
	}

	NotFound(c, fmt.Sprintf("instance '%s' not found", runID))
}

// tryUnloadEndpointModel attempts to unload a model from an endpoint provider (e.g. Ollama).
// Returns true if the model was found and handled (success or error response written).
func (e *RunsExecutor) tryUnloadEndpointModel(c *gin.Context, runID string) bool {
	cfg := e.appsConfig()
	if cfg == nil || e.appMgr == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), constants.ClusterQueryTimeout)
	defer cancel()

	var handled bool
	cfg.RangeApps(func(providerName string, providerCfg pkgConfig.ServiceConfig) bool {
		if !providerCfg.IsEnabled() || !providerCfg.HasEndpoint() {
			return true
		}
		resolved, ok := e.backend.Resolve(providerName)
		if !ok {
			return true
		}

		providerImpl, ok := e.appMgr.Protocol(providerName)
		if !ok {
			return true
		}
		target := protocol.TargetOf(resolved)

		// Query running models to find one matching this runID (digest)
		runningModels, err := providerImpl.ListRunningModels(ctx, target)
		if err != nil {
			return true
		}

		for _, m := range runningModels {
			if m.Digest != runID {
				continue
			}

			// Found it — unload
			if err := providerImpl.UnloadModel(ctx, target, m.Name); err != nil {
				slog.Error("[RunsExecutor] Failed to unload endpoint model", "provider", providerName, "model", m.Name, "error", err)
				InternalNodeError(c, fmt.Sprintf("failed to unload model '%s'", m.Name))
				handled = true
				return false
			}

			utils.LogDebugf("[RunsExecutor] Unloaded endpoint model '%s' from provider '%s'", m.Name, providerName)
			e.triggerResourceRefresh()
			c.JSON(http.StatusOK, gin.H{
				"id":      runID,
				"status":  "stopped",
				"message": fmt.Sprintf("Model '%s' unloaded from %s", m.Name, providerName),
			})
			handled = true
			return false
		}
		return true
	})

	return handled
}

// HandleInternalRestartRun handles POST /zzrouter/internal/runs/:id/restart (internal API).
// Restart = stop + relaunch with the same provider/model/parameters/env. The
// new instance gets a fresh port and a new ID. There is a brief window during
// which the model is not addressable.
func (e *RunsExecutor) HandleInternalRestartRun(c *gin.Context) {
	if !e.requireProviderAppMgr(c) {
		return
	}

	runID := c.Param("id")
	if runID == "" {
		BadRequest(c, "run ID is required")
		return
	}

	newInst, err := e.appMgr.RestartInstance(c.Request.Context(), runID)
	if err != nil {
		if errors.Is(err, prov_apps.ErrInstanceNotFound) {
			NotFound(c, fmt.Sprintf("instance '%s' not found", runID))
			return
		}
		InternalNodeError(c, "restart failed: "+err.Error())
		return
	}

	// Restart returns 202 + job_id of the NEW instance's launch handle.
	// The "stopping" phase of the old instance is deterministic and
	// short; the time-sink is the new-instance launch, which is what
	// the job tracks. Agents subscribe via /jobs/:id/stream?node=<node>.
	c.JSON(http.StatusAccepted, &RestartRunResponse{
		Accepted: true,
		OldID:    runID,
		ID:       newInst.ID,
		JobID:    newInst.StreamJobID,
		Provider: newInst.Provider,
		Model:    newInst.Model,
		Port:     newInst.Port,
		Status:   string(newInst.GetStatus()),
		Message:  fmt.Sprintf("Instance %s restart accepted as %s; subscribe to job_id", runID, newInst.ID),
		Node:     e.getNodename(),
	})
}

// queryEndpointProviders queries endpoint providers (like Ollama) for running models
// Returns pseudo-instances for models running in the provider
func (e *RunsExecutor) queryEndpointProviders() []*instance.Instance {
	var pseudoInstances []*instance.Instance

	// Check dependencies
	cfg := e.appsConfig()
	if !e.routerAvailable || cfg == nil || e.appMgr == nil {
		return pseudoInstances
	}

	// Use a timeout context to ensure provider queries don't hang indefinitely
	// This prevents resource leaks if a provider is slow or unresponsive
	ctx, cancel := context.WithTimeout(context.Background(), constants.ClusterQueryTimeout)
	defer cancel()

	// Iterate through configured providers
	cfg.RangeApps(func(providerName string, providerCfg pkgConfig.ServiceConfig) bool {
		// Only query enabled endpoint-based apps (service/external/cloud)
		if !providerCfg.IsEnabled() || !providerCfg.HasEndpoint() {
			return true
		}

		// Get the provider implementation from registry
		providerImpl, ok := e.appMgr.Protocol(providerName)
		if !ok {
			utils.LogDebugf("Provider '%s' not found in registry", providerName)
			return true
		}

		resolved, ok := e.backend.Resolve(providerName)
		if !ok {
			utils.LogDebugf("Provider '%s' has no endpoint configured, skipping", providerName)
			return true
		}

		// Query running models from the app's API
		runningModels, err := providerImpl.ListRunningModels(ctx, protocol.TargetOf(resolved))
		if err != nil {
			utils.LogDebugf("[RunsExecutor] Warning: Failed to query running models from %s: %v", providerName, err)
			return true
		}

		// Convert running models to Instance format
		for _, runningModel := range runningModels {
			// Create a pseudo-instance for endpoint provider models
			// Store the FULL digest internally (truncation happens in display layer)
			instanceID := runningModel.Digest
			if instanceID == "" {
				// Fallback: generate a 12-char hash if provider doesn't provide an ID
				source := fmt.Sprintf("%s-%s-%d", providerName, runningModel.Name, utils.Now().UnixNano())
				instanceID = instance.GenerateShortHash(source)
			}

			// Compute processor string (e.g. "100% GPU") from VRAM ratio
			var processor string
			if runningModel.Size > 0 && runningModel.SizeVRAM > 0 {
				pct := float64(runningModel.SizeVRAM) / float64(runningModel.Size) * 100
				if pct >= 100 {
					processor = "100% GPU"
				} else if pct > 0 {
					processor = fmt.Sprintf("%.0f%% GPU / %.0f%% CPU", pct, 100-pct)
				}
			} else if runningModel.SizeVRAM == 0 && runningModel.Size > 0 {
				processor = "100% CPU"
			}

			pseudoInstance := instance.NewInstance(instanceID, providerName, runningModel.Name, 0, time.Until(runningModel.ExpiresAt), 0)
			pseudoInstance.MarkRunning()
			pseudoInstance.SizeBytes = runningModel.Size
			pseudoInstance.ContextLength = runningModel.ContextLength
			pseudoInstance.Processor = processor
			pseudoInstance.SourceRepo = metadata.SourceOllama
			pseudoInstance.LaunchMode = instance.LaunchModeNative
			pseudoInstance.LastActivity = utils.Now()

			pseudoInstances = append(pseudoInstances, pseudoInstance)
		}

		if len(runningModels) > 0 {
			utils.LogDebugf("[RunsExecutor] Found %d running models in endpoint provider '%s'", len(runningModels), providerName)
		}
		return true
	})

	return pseudoInstances
}

// HandleInternalLaunchRun handles POST /zzrouter/internal/runs (internal API)
func (e *RunsExecutor) HandleInternalLaunchRun(c *gin.Context) {
	if !e.requireProviderAppMgr(c) {
		return
	}

	var req struct {
		Provider         string                 `json:"provider" binding:"required"`
		Runtime          string                 `json:"runtime,omitempty"`
		DisposablePlanID string                 `json:"disposable_plan_id,omitempty"`
		LaunchMode       string                 `json:"launch_mode" binding:"required,oneof=native"`
		Model            string                 `json:"model_name" binding:"required"` // matches public LaunchRunRequest
		Endpoint         string                 `json:"endpoint,omitempty" binding:"omitempty,oneof=chat embeddings reranking"`
		AutoDeploy       bool                   `json:"auto_deploy,omitempty"`
		Port             int                    `json:"port,omitempty"`
		Files            []instance.FileMount   `json:"files,omitempty"`
		Parameters       map[string]string      `json:"parameters,omitempty"`
		EnvVars          map[string]string      `json:"env_vars,omitempty"`
		NativeConfig     *instance.NativeConfig `json:"native_config,omitempty"`
	}

	// BindJSON emits a structured errors[] array so agents can read the
	// allowed launch_mode enum (param="native") from the validator.
	if !BindJSON(c, &req) {
		return
	}

	// Validate provider type
	if !e.appMgr.IsProviderSupported(req.Provider) {
		BadRequest(c, fmt.Sprintf("unsupported provider type: %s", req.Provider))
		return
	}

	// Cloud providers don't run — they're already running upstream and
	// "deploying" them just registers a model name (AddCloudModel). The
	// /runs surface is local-only by definition; previously this fell
	// through to LaunchInstance and 500'd because cloud providers have
	// no Execution config to spawn a process from.
	if cfg := e.appsConfig(); cfg != nil {
		if svc, ok := cfg.LookupApp(req.Provider); ok && svc.IsCloudProvider() {
			BadRequest(c, fmt.Sprintf(
				"provider %q is cloud-only and has no run lifecycle; use POST /zzrouter/v1/deployments to register cloud models", req.Provider))
			return
		}
	}

	// Convert launch mode
	_, err := parseLaunchMode(req.LaunchMode)
	if err != nil {
		BadRequest(c, "Invalid launch mode: "+err.Error())
		return
	}

	if err := e.appMgr.ValidateModel(c.Request.Context(), req.Model); err != nil {
		respondProviderErr(c, err)
		return
	}
	launchReq := prov_apps.LaunchRequest{
		Runtime: req.Runtime, DisposablePlanID: req.DisposablePlanID,
		Provider:     req.Provider,
		Model:        req.Model,
		Endpoint:     prov_apps.Endpoint(req.Endpoint),
		Port:         req.Port,
		Parameters:   req.Parameters,
		EnvVars:      req.EnvVars,
		Files:        req.Files,
		NativeConfig: req.NativeConfig,
	}

	// auto_deploy: when the model isn't in the pool, trigger a
	// download via DeploymentsService and chain the launch behind it.
	// The handler returns 202 immediately with both job IDs; the run
	// job's first phase waits on the deploy job. Cloud providers go
	// through POST /deployments instead and are refused here.
	if req.AutoDeploy && e.modelInPool != nil && !e.modelInPool(req.Model) {
		e.handleAutoDeployLaunch(c, req.Provider, req.Endpoint, launchReq)
		return
	}

	// Launch the instance. Concurrent POSTs for the same (provider,
	// model) are coalesced inside LaunchInstance via the manager's
	// launchGroup singleflight, so the direct /runs path and the
	// /runs/load path both get the same dedupe semantics — second
	// caller receives the first's instance instead of ErrModelAlreadyLoaded.
	inst, err := e.appMgr.LaunchInstance(c.Request.Context(), launchReq)
	if err != nil {
		// Most of what a launch can fail on is the caller's doing: a
		// model that is not on this node, a parameter the provider
		// rejects, a port outside the configured range. Reporting
		// those as 500 tells an agent to retry something that cannot
		// ever succeed, so classify by sentinel and keep 500 for the
		// failures that really are ours.
		respondProviderErr(c, fmt.Errorf("failed to launch instance: %w", err))
		return
	}

	hostname := e.getNodename()

	// Return 202 Accepted with instance details + job_id so agents can
	// subscribe to launch progress via /jobs/:id/stream?node=<node>.
	// Use req.Files/req.Parameters instead of instance.Config to avoid
	// a race with the launch goroutine that mutates instance.Config.
	c.JSON(http.StatusAccepted, gin.H{
		"accepted":           true,
		"id":                 inst.ID,
		"job_id":             inst.StreamJobID,
		"provider":           inst.Provider,
		"launch_mode":        string(inst.LaunchMode),
		"status":             string(inst.GetStatus()),
		"port":               inst.Port,
		"health_url":         inst.HealthURL,
		"started_at":         inst.StartedAt.Format(time.RFC3339),
		"message":            "Instance launch accepted; subscribe to job_id for readiness",
		"files_mounted":      len(req.Files),
		"parameters_applied": len(req.Parameters),
		"node":               hostname,
	})
}

// HandleInternalGetRunLogs handles GET /zzrouter/internal/runs/:id/logs (internal API)
// This endpoint is called by cluster routing to get logs from remote workers
func (e *RunsExecutor) HandleInternalGetRunLogs(c *gin.Context) {
	if !e.requireProviderAppMgr(c) {
		return
	}

	instanceID := c.Param("id")
	if instanceID == "" {
		BadRequest(c, "instance ID is required")
		return
	}

	// Validate numeric parameters
	linesParams := ValidateParams(c, []ParamRule{
		IntQueryParam("lines", 100, 1, 10000),
		BoolQueryParam("follow"),
		IntQueryParam("context", 0, 0, logFilterMaxContext),
		IntQueryParam("max_matches", logFilterDefaultMaxMatches, 1, logFilterAbsoluteMaxMatches),
	})
	if linesParams == nil {
		return
	}
	lines := linesParams.GetInt("lines")
	follow := linesParams.GetBool("follow")
	contextLines := linesParams.GetInt("context")
	maxMatches := linesParams.GetInt("max_matches")

	filter, err := compileLogFilter(c.Query("grep"), c.Query("regex"), contextLines, maxMatches)
	if err != nil {
		BadRequest(c, err.Error())
		return
	}

	// Wait for instance to have log file path set (async launch may not have completed yet)
	ctx, cancel := context.WithTimeout(c.Request.Context(), constants.ClusterActionTimeout)
	defer cancel()

	ticker := time.NewTicker(constants.StatusPollInterval)
	defer ticker.Stop()

	var inst *instance.Instance
	var logFilePath string

WaitForLogPath:
	for {
		select {
		case <-ctx.Done():
			RequestTimeout(c, "Timeout waiting for log file; instance is still starting, log file not ready yet")
			return
		case <-ticker.C:
			// Get fresh instance data
			i, exists := e.appMgr.GetInstance(instanceID)
			if !exists {
				NotFound(c, fmt.Sprintf("instance '%s' not found", instanceID))
				return
			}
			inst = i

			// Check if log file path is set
			if inst.LogFilePath != "" {
				logFilePath = inst.LogFilePath
				// Also check if file exists on filesystem
				if _, err := os.Stat(logFilePath); err == nil {
					break WaitForLogPath
				}
			}
		}
	}

	// Serve logs (streaming always starts from beginning of file)
	if follow {
		e.streamInstanceLogs(c, logFilePath, filter)
	} else {
		e.getInstanceLogLines(c, logFilePath, lines, filter)
	}
}

// HandleInternalGetRunHealth handles GET /zzrouter/internal/runs/:id/health (internal API)
func (e *RunsExecutor) HandleInternalGetRunHealth(c *gin.Context) {
	if !e.requireProviderAppMgr(c) {
		return
	}

	instanceID := c.Param("id")
	if instanceID == "" {
		BadRequest(c, "instance ID is required")
		return
	}

	inst, exists := e.appMgr.GetInstance(instanceID)
	if !exists {
		NotFound(c, fmt.Sprintf("instance '%s' not found", instanceID))
		return
	}

	// Calculate uptime
	uptime := time.Since(inst.StartedAt)

	// Determine health status
	healthStatus := "unknown"
	status := inst.GetStatus()
	switch status {
	case instance.StatusRunning:
		healthStatus = "healthy"
	case instance.StatusStarting:
		healthStatus = "starting"
	case instance.StatusFailed:
		healthStatus = "unhealthy"
	case instance.StatusStopped:
		healthStatus = "stopped"
	}

	response := gin.H{
		"instance_id": inst.ID,
		"status":      healthStatus,
		"health_url":  inst.HealthURL,
		"uptime":      uptime.String(),
	}

	if !inst.LastHealthCheck.IsZero() {
		response["last_check"] = inst.LastHealthCheck.Format(time.RFC3339)
	}

	c.JSON(http.StatusOK, response)
}

// handleAutoDeployLaunch is the auto_deploy=true chain. Resolves the
// provider's registry + variant, kicks off a deploy via the injected
// DeploymentsService, opens a run job that waits on the deploy job
// before invoking LaunchInstance, and returns 202 with both job IDs.
//
// Refuses cloud providers explicitly — those go through POST /deployments
// (AddCloudModel, no download). The structural difference between the
// two paths matters for failure modes and observability; transparent
// dispatch would mask it.
func (e *RunsExecutor) handleAutoDeployLaunch(c *gin.Context, providerName, endpoint string, launchReq prov_apps.LaunchRequest) {
	if e.deploy == nil || e.jobs == nil {
		ServiceUnavailable(c, "auto_deploy is not wired on this node")
		return
	}

	cfg := e.appsConfig()
	if cfg == nil {
		InternalNodeError(c, "apps config unavailable")
		return
	}
	svc, ok := cfg.LookupApp(providerName)
	if !ok {
		BadRequest(c, fmt.Sprintf("provider %q not in config", providerName))
		return
	}
	// Cloud providers were already refused at the top of the handler;
	// reaching this branch with a cloud provider would be a wiring bug.

	deployReq, err := autoDeployRequest(providerName, svc, launchReq.Model)
	if err != nil {
		BadRequest(c, err.Error())
		return
	}

	// Coalesce concurrent auto_deploy on (provider, weights, file): two
	// variants of one base share its download. Endpoint excluded since
	// downloads are endpoint-agnostic.
	deployReq.Nodes = []string{e.getNodename()}
	d := featureDownload(deployReq, svc, *deployReq.Features)
	dedupeKey := deployPlanKey(providerName, *deployReq.Features, &d)
	// Detach ctx: shared singleflight + request cancel would fail all coalesced callers.
	v, err, _ := e.deployGroup.Do(dedupeKey, func() (any, error) {
		return e.deploy(context.Background(), deployReq)
	})
	if err != nil {
		InternalNodeError(c, "deploy failed: "+utils.SanitizeErrorMessage(err.Error()))
		return
	}
	deployment, ok := v.(*Deployment)
	if !ok || deployment == nil {
		InternalNodeError(c, "deploy returned unexpected type")
		return
	}
	deployJobID := deploymentLocalJobID(deployment, e.getNodename())
	if deployJobID == "" {
		InternalNodeError(c, "deploy returned no job_id; cannot chain launch")
		return
	}

	// Carry the resolved variant into the launch via "#hint" so the launcher's
	// model→path resolver picks the right file inside the repo dir (llama-server
	// fails if MODEL_PATH is a directory). Skip when the model already encodes
	// a hint or no variant was selected.
	if deployReq.File != "" && !strings.Contains(launchReq.Model, "#") {
		launchReq.Model = launchReq.Model + "#" + deployReq.File
	}

	runHandle, err := e.jobs.StartDetached(jobs.KindRun, "", jobs.Meta{
		"model":         launchReq.Model,
		"provider":      providerName,
		"endpoint":      endpoint,
		"deploy_job_id": deployJobID,
		"phase":         "waiting_for_deploy",
		"chained_via":   "auto_deploy",
	})
	if err != nil {
		InternalNodeError(c, "open run job: "+err.Error())
		return
	}

	go e.waitDeployThenLaunch(runHandle, deployJobID, launchReq)

	c.JSON(http.StatusAccepted, gin.H{
		"accepted":      true,
		"id":            "",
		"job_id":        runHandle.ID(),
		"deploy_job_id": deployJobID,
		"provider":      providerName,
		"status":        "waiting_for_deploy",
		"node":          e.getNodename(),
		"message":       "deploy chain accepted; subscribe to deploy_job_id for download progress, then job_id for launch readiness",
	})
}

// waitDeployThenLaunch is the chained run-job goroutine. Subscribes to
// the deploy job's terminal event and either fails the run job or
// invokes LaunchInstance on success.
func (e *RunsExecutor) waitDeployThenLaunch(runHandle jobs.Handle, deployJobID string, launchReq prov_apps.LaunchRequest) {
	if err := waitJobCompletion(runHandle.Context(), func(context.Context) (jobs.Event, error) { return e.jobs.Get(deployJobID) }); err != nil {
		runHandle.Fail(fmt.Errorf("deploy job %s: %w", deployJobID, err))
		return
	}
	runHandle.Meta(jobs.Meta{"phase": "launching"})
	inst, err := e.appMgr.LaunchInstance(runHandle.Context(), launchReq)
	if err != nil {
		runHandle.Fail(fmt.Errorf("launch after deploy: %w", err))
		return
	}
	runHandle.Meta(jobs.Meta{"phase": "running", "instance_id": inst.ID, "port": inst.Port})
	runHandle.Done()
}

// deploymentLocalJobID extracts the per-node job_id for the local node
// from the multi-node Deployment response. Returns "" if the local node
// isn't in the deployment (defensive — auto-deploy always targets the
// requesting node).
func deploymentLocalJobID(d *Deployment, localNode string) string {
	if d == nil {
		return ""
	}
	for _, n := range d.Nodes {
		if n.Node == localNode {
			return n.JobID
		}
	}
	if len(d.Nodes) == 1 {
		return d.Nodes[0].JobID
	}
	return ""
}

// autoDeployRequest returns the download an auto-deploy of model needs
// on svc: the weights the model runs on (a variant's base) from the
// provider's registry. The file is the model's own #hint, else the
// provider's default quant unless the repo name already pins one.
func autoDeployRequest(provider string, svc pkgConfig.ServiceConfig, model string) (*DeployRequest, error) {
	registry := ""
	if svc.Search != nil {
		registry = svc.Search.Registry
	}
	if registry == "" {
		return nil, fmt.Errorf("provider %q has no search.registry configured; cannot infer download source", provider)
	}
	weights, file, _ := strings.Cut(svc.WeightsOf(model), "#")
	names, err := selectedFeatures(svc, nil)
	if err != nil {
		return nil, err
	}
	req := &DeployRequest{Model: weights, Registry: registry, File: file, Provider: provider, Features: &names, defaultFeatures: true}
	if req.File == "" && svc.Capabilities != nil && svc.Capabilities.DefaultVariant != "" && !modelNameEncodesVariant(weights) {
		req.File = svc.Capabilities.DefaultVariant
	}
	return req, nil
}

// modelNameEncodesVariant returns true when the model repo name pins a
// specific GGUF quant (e.g. "*-Q4_K_M", "*-FP16"). The bare "-GGUF" tag
// alone is a *family* marker — those repos hold many quants and STILL
// need default_variant filtering. This used to return true for any
// "-GGUF" substring, which made the download fetch every quant.
func modelNameEncodesVariant(model string) bool {
	upper := strings.ToUpper(model)
	for _, q := range []string{"-Q2_K", "-Q3_K", "-Q4_0", "-Q4_K", "-Q5_0", "-Q5_K", "-Q6_K", "-Q8_0", "-FP16"} {
		if strings.Contains(upper, q) {
			return true
		}
	}
	return false
}
