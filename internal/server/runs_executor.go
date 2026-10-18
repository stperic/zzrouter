package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
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
	publishCatalog      func() error
	appMgr              *prov_apps.ProviderAppManager
	getNodename         func() string
	appsConfig          func() *pkgConfig.AppsConfig
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
	// that waits on the deploy job before invoking EnsureRun.
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
	streamInstanceLogs func(*gin.Context, string, *logFilter),
	getInstanceLogLines func(*gin.Context, string, int, *logFilter),
	refreshResourceMetrics func(),
) *RunsExecutor {
	return &RunsExecutor{
		appMgr:                 appMgr,
		getNodename:            getNodename,
		appsConfig:             appsConfig,
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

// GetLocalRuns lists this node's runs, served models included; withStatus adds their ParametersStatus.
func (e *RunsExecutor) GetLocalRuns(ctx context.Context, withStatus bool) ([]instance.InstanceInfo, error) {
	if e.appMgr == nil {
		return nil, fmt.Errorf("provider app manager not initialized")
	}
	hostname := e.getNodename()
	result := e.appMgr.ListInstances(hostname)
	for i := range result {
		if inst, ok := e.appMgr.GetInstance(result[i].ID); withStatus && ok {
			result[i] = e.appMgr.InstanceInfo(ctx, inst, hostname)
		}
		classifyRunInfo(&result[i])
	}
	return result, nil
}

// GetLocalRun retrieves a specific run from the local provider app manager
func (e *RunsExecutor) GetLocalRun(ctx context.Context, runID string) (*instance.InstanceInfo, error) {
	if e.appMgr == nil {
		return nil, fmt.Errorf("provider app manager not initialized")
	}

	info, err := e.appMgr.RunInfo(ctx, runID, e.getNodename())
	classifyRunInfo(&info)
	return &info, err
}

func classifyRunInfo(info *instance.InstanceInfo) {
	if info.Refusal != nil {
		wire := failureWire(info.Refusal, http.StatusInternalServerError)
		info.LastRefusal = &httperr.Refusal{Code: wire.Code, Status: wire.Status, Message: wire.Message}
	}
}

// HandleInternalListRuns handles GET /zzrouter/internal/runs (internal API)
func (e *RunsExecutor) HandleInternalListRuns(c *gin.Context) {
	runs, err := e.GetLocalRuns(c.Request.Context(), c.Query("include") == includeParametersStatus)
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

	run, err := e.GetLocalRun(c.Request.Context(), runID)
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

	err := e.appMgr.StopInstance(c.Request.Context(), runID)
	if !publishOr503(c, e.publishCatalog) {
		return
	}
	if err != nil {
		RespondToError(c, fmt.Errorf("stop run %s: %w", runID, err))
		return
	}
	utils.LogDebugf("[RunsExecutor] Stopped instance '%s'", runID)
	e.triggerResourceRefresh()
	c.JSON(http.StatusOK, gin.H{
		"id":      runID,
		"status":  "stopped",
		"message": "Instance stopped successfully",
	})
}

// HandleInternalRestartRun replays a live run or retained pin.
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
	if !publishOr503(c, e.publishCatalog) {
		return
	}
	if err != nil {
		RespondToError(c, fmt.Errorf("restart run %s: %w", runID, err))
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
		Provider: newInst.Provider, BaseURL: newInst.BaseURL,
		Model:   newInst.Model,
		Port:    newInst.Port,
		Status:  string(newInst.GetStatus()),
		Message: fmt.Sprintf("Instance %s restart accepted as %s; subscribe to job_id", runID, newInst.ID),
		Node:    e.getNodename(),
	})
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
		Model            string                 `json:"model_name,omitempty"`
		Endpoint         string                 `json:"endpoint,omitempty" binding:"omitempty,oneof=chat embeddings reranking"`
		AutoDeploy       bool                   `json:"auto_deploy,omitempty"`
		Port             int                    `json:"port,omitempty"`
		Files            []instance.FileMount   `json:"files,omitempty"`
		Parameters       schema.ParameterValues `json:"parameters,omitempty"`
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

	// Convert launch mode
	_, err := parseLaunchMode(req.LaunchMode)
	if err != nil {
		BadRequest(c, "Invalid launch mode: "+err.Error())
		return
	}

	if err := validateRunTarget(e.appsConfig, req.Provider, req.Model, req.Endpoint); err != nil {
		RespondToError(c, err)
		return
	}
	// Before auto_deploy can start a download for a name admission refuses.
	if err := e.appMgr.ValidateModel(c.Request.Context(), req.Model); err != nil {
		RespondToError(c, err)
		return
	}
	launchReq := prov_apps.EnsureRequest{
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
	// job's first phase waits on the deploy job. A provider without runs has nothing to download.
	svc, _ := e.appsConfig().LookupApp(req.Provider)
	if req.Model != "" && req.AutoDeploy && e.modelInPool != nil && !e.modelInPool(req.Model) && prov_apps.Lifecycle(svc) != prov_apps.LifecycleNone {
		e.handleAutoDeployLaunch(c, req.Provider, req.Endpoint, launchReq)
		return
	}

	result, err := e.appMgr.EnsureRun(c.Request.Context(), launchReq)
	if err != nil {
		// Most of what a launch can fail on is the caller's doing: a
		// model that is not on this node, a parameter the provider
		// rejects, a port outside the configured range. Reporting
		// those as 500 tells an agent to retry something that cannot
		// ever succeed, so classify by sentinel and keep 500 for the
		// failures that really are ours.
		RespondToError(c, fmt.Errorf("failed to launch instance: %w", err))
		return
	}

	inst := result.Run
	if inst == nil {
		c.JSON(http.StatusOK, LaunchRunResponse{Accepted: true, Provider: req.Provider, LaunchMode: req.LaunchMode, Status: "running", Node: e.getNodename(), Message: "The provider serves this model without a run", ParametersApplied: len(req.Parameters), FilesMounted: len(req.Files)})
		return
	}

	hostname := e.getNodename()
	message := "Instance launch accepted; subscribe to job_id for readiness"
	if !result.Started {
		message = "Run already exists; reused"
	}

	cfg := inst.SnapshotConfig()
	c.JSON(http.StatusAccepted, gin.H{
		"accepted":           true,
		"id":                 inst.ID,
		"job_id":             inst.StreamJobID,
		"provider":           inst.Provider,
		"launch_mode":        string(inst.LaunchMode),
		"status":             string(inst.GetStatus()),
		"port":               inst.Port,
		"health_url":         inst.HealthURL,
		"base_url":           inst.BaseURL,
		"started_at":         inst.GetStartedAt().Format(time.RFC3339),
		"message":            message,
		"files_mounted":      len(req.Files),
		"parameters_applied": len(cfg.Parameters),
		"parameters":         cfg.Parameters,
		"pinned":             inst.IdleSnapshot().Pinned,
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
				// Stopped: its log is a finished file, read as a snapshot.
				if path := stoppedRunLog(e.appMgr, instanceID); path != "" {
					e.getInstanceLogLines(c, path, lines, filter)
					return
				}
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
	uptime := time.Since(inst.GetStartedAt())

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
// before invoking EnsureRun, and returns 202 with both job IDs.
//
// Refuses cloud providers explicitly — those go through POST /deployments
// (AddCloudModel, no download). The structural difference between the
// two paths matters for failure modes and observability; transparent
// dispatch would mask it.
func (e *RunsExecutor) handleAutoDeployLaunch(c *gin.Context, providerName, endpoint string, launchReq prov_apps.EnsureRequest) {
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
// invokes EnsureRun on success.
func (e *RunsExecutor) waitDeployThenLaunch(runHandle jobs.Handle, deployJobID string, launchReq prov_apps.EnsureRequest) {
	if err := (&jobAccess{registry: e.jobs}).Wait(runHandle.Context(), deployJobID, ""); err != nil {
		runHandle.Fail(fmt.Errorf("deploy job %s: %w", deployJobID, err))
		return
	}
	runHandle.Meta(jobs.Meta{"phase": "launching"})
	result, err := e.appMgr.EnsureRun(runHandle.Context(), launchReq)
	inst := result.Run
	if err == nil && inst != nil && inst.StreamJobID != "" {
		err = (&jobAccess{registry: e.jobs}).Wait(runHandle.Context(), inst.StreamJobID, "")
	}
	if err != nil {
		runHandle.Fail(fmt.Errorf("launch after deploy: %w", err))
		return
	}
	if inst != nil {
		runHandle.Meta(jobs.Meta{"phase": "running", "instance_id": inst.ID, "port": inst.Port})
	}
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
