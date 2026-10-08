package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/utils"
	"golang.org/x/sync/singleflight"
)

// ============================================================================
// Load Executor - Internal API for Direct Local Execution
// ============================================================================
//
// This executor handles direct interaction with the local runs manager.
// Used by /zzrouter/internal/runs/load and /zzrouter/internal/runs/preview endpoints
// for cluster-internal communication.
//
// Responsibilities:
// - Load models on local runs manager directly
// - Generate run previews
// - No routing, no caching, no orchestration
// - Return raw local data only

// modelLister is the one question the load path asks the model
// registry: what is on this node's disk.
type modelLister interface {
	ListAllModels() ([]*metadata.ModelMetadata, error)
}

// LoadExecutor handles direct local model loading operations
type LoadExecutor struct {
	appMgr            *prov_apps.ProviderAppManager
	isProviderEnabled func(string) bool
	registry          modelLister
	resolveBackend    func(string) (*backend.Resolved, bool)
	mergeAndResolve   func(modelName, appType, endpoint string, parameters, environment map[string]string) (*mergeResolveResult, error)
	launchOnDemand    func(ctx context.Context, modelName, providerType, endpoint string, parameters, environment map[string]string) (*instance.Instance, error)
	invalidateCache   func()
	getNodename       func() string
	// refreshResourceMetrics triggers an immediate re-probe of live
	// GPU memory/utilization after a load completes so the next
	// routing decision sees the new VRAM state. Mirrors Ollama's
	// pattern: probe at load/unload events rather than on a
	// continuous timer. Nullable — when nil (tests, non-cluster
	// deployments), the load path simply skips the refresh.
	refreshResourceMetrics func()
	loadGroup              singleflight.Group // Prevents thundering herd on concurrent model loads
}

// Note: findMatchingModels is defined in model_load_handlers.go and used here

// canonicalModelName resolves a caller-supplied model name through the
// provider manager, which owns the mapping. A nil manager leaves the
// name alone; the load fails on it a moment later either way.
func (e *LoadExecutor) canonicalModelName(model string) string {
	if e.appMgr == nil {
		return model
	}
	return e.appMgr.CanonicalModelName(model)
}

// normalizeAndValidateProvider normalizes and validates a provider (DRY helper)
func (e *LoadExecutor) normalizeAndValidateProvider(provider string) (string, error) {
	if provider == "" {
		return "", nil
	}

	normalized := utils.NormalizeAppType(provider)

	// Validate provider exists
	if _, ok := e.appMgr.Protocol(normalized); !ok {
		return "", fmt.Errorf("unknown provider: %s", provider)
	}

	// Check provider enabled in config
	if !e.isProviderEnabled(normalized) {
		return "", fmt.Errorf("provider not enabled: %s", provider)
	}

	return normalized, nil
}

// errAutoDetectModelNotFound and errAutoDetectMultipleProviders are sentinels
// returned by autoDetectProvider so callers can branch with errors.Is rather
// than substring-matching the message. Unexported because they're only consumed
// inside this package.
var (
	errAutoDetectModelNotFound     = errors.New("model not found in registry")
	errAutoDetectMultipleProviders = errors.New("multiple apps available for model")
)

// autoDetectProvider auto-detects provider from model name (DRY helper)
func (e *LoadExecutor) autoDetectProvider(modelName string) (string, error) {
	models, err := e.registry.ListAllModels()
	if err != nil {
		return "", fmt.Errorf("failed to list models: %w", err)
	}

	// Use fuzzy matching to find model
	matches := findMatchingModels(modelName, models)

	if len(matches) == 0 {
		return "", fmt.Errorf("model '%s': %w", modelName, errAutoDetectModelNotFound)
	}

	if len(matches) > 1 {
		return "", fmt.Errorf("model '%s' (%w) - please specify --provider flag", modelName, errAutoDetectMultipleProviders)
	}

	// Single match - use SourceRepo as provider and normalize it
	provider := utils.NormalizeAppType(matches[0].SourceRepo)
	utils.LogDebugf("[LoadExecutor] Auto-detected provider %s for model %s", provider, modelName)

	return provider, nil
}

// triggerResourceRefresh fires a ResourceTracker refresh in a
// goroutine so the load/unload response path doesn't block on
// nvidia-smi. Safe to call when the callback is nil — a no-op in
// tests and non-cluster deployments. Mirrors Ollama's event-driven
// discipline: refresh live memory at model-lifecycle events, not
// on a continuous polling loop.
func (e *LoadExecutor) triggerResourceRefresh() {
	if e.refreshResourceMetrics == nil {
		return
	}
	go e.refreshResourceMetrics()
}

// NewLoadExecutor creates a new load executor. refreshResourceMetrics
// may be nil (tests, non-cluster deployments); production wiring
// passes a closure over ResourceTracker.CollectNow.
func NewLoadExecutor(
	appMgr *prov_apps.ProviderAppManager,
	isProviderEnabled func(string) bool,
	registry modelLister,
	resolveBackend func(string) (*backend.Resolved, bool),
	mergeAndResolve func(modelName, appType, endpoint string, parameters, environment map[string]string) (*mergeResolveResult, error),
	launchOnDemand func(ctx context.Context, modelName, providerType, endpoint string, parameters, environment map[string]string) (*instance.Instance, error),
	invalidateCache func(),
	getNodename func() string,
	refreshResourceMetrics func(),
) *LoadExecutor {
	return &LoadExecutor{
		appMgr:                 appMgr,
		isProviderEnabled:      isProviderEnabled,
		registry:               registry,
		resolveBackend:         resolveBackend,
		mergeAndResolve:        mergeAndResolve,
		launchOnDemand:         launchOnDemand,
		invalidateCache:        invalidateCache,
		getNodename:            getNodename,
		refreshResourceMetrics: refreshResourceMetrics,
	}
}

// LoadLocalModel loads a model on the local runs manager
// Uses singleflight to prevent thundering herd when multiple clients request the same model
func (e *LoadExecutor) LoadLocalModel(ctx context.Context, req *LoadModelRequest) (*LoadModelResponse, error) {
	if e.appMgr != nil {
		if err := e.appMgr.ValidateModel(ctx, req.ModelName); err != nil {
			return nil, err
		}
	}
	// Resolve BEFORE the coalescing key and the existing-instance checks
	// are computed from it: a launch registers the run under the resolved
	// name, so an alias reaching those checks raw reads as "not running".
	// On a copy -- the request is the caller's and it still reads it.
	resolved := *req
	resolved.requestedModel = req.ModelName
	resolved.ModelName = e.canonicalModelName(req.ModelName)

	// Generate key for singleflight coalescing
	// Include provider to differentiate same model on different providers
	key := fmt.Sprintf("%s:%s", resolved.ModelName, resolved.Provider)

	// Use singleflight to coalesce concurrent requests for the same model
	result, err, shared := e.loadGroup.Do(key, func() (any, error) {
		if resolved.Force && e.appMgr != nil {
			admitted, err := e.appMgr.AdmitLocalModel(ctx, resolved.requestedModel)
			if err != nil {
				return nil, err
			}
			ctx = admitted
		}
		return e.doLoadLocalModel(ctx, &resolved)
	})

	if shared {
		slog.Info("[LoadExecutor] Request for coalesced with concurrent request", "model_name", resolved.ModelName)
	}

	if err != nil {
		return nil, err
	}
	return result.(*LoadModelResponse), nil //nolint:errcheck // singleflight Do returns the exact type its fn returned
}

// doLoadLocalModel performs the actual model loading (called by singleflight)
func (e *LoadExecutor) doLoadLocalModel(ctx context.Context, req *LoadModelRequest) (*LoadModelResponse, error) { //nolint:gocyclo,cyclop // Existing-instance reuse, provider API loads and local launches share one load flow.
	// Start model load span
	ctx, span := llm.StartModelLoadSpan(ctx, req.ModelName, req.Provider)
	defer span.End()
	startTime := utils.Now()

	utils.LogDebugf("[LoadExecutor] doLoadLocalModel: model=%s, provider=%s, force=%v",
		req.ModelName, req.Provider, req.Force)

	// Check if provider app manager is available
	if e.appMgr == nil {
		err := fmt.Errorf("provider app manager not initialized")
		llm.SetSpanError(span, err)
		return nil, err
	}

	// Normalize and validate provider
	normalizedProvider, err := e.normalizeAndValidateProvider(req.Provider)
	if err != nil {
		return &LoadModelResponse{
			StatusCode: http.StatusBadRequest,
			Error:      err.Error(),
		}, nil
	}

	// Check if model is already starting/running
	if existingInstance, found := e.appMgr.Instances().GetByModelEndpoint(req.ModelName, req.Endpoint); found {
		status := existingInstance.GetStatus()
		if !req.Force && (status == instance.StatusStarting || status == instance.StatusRunning) {
			detail := fmt.Sprintf("Instance %s is %s. Use --force to restart.", existingInstance.ID, status)
			if status == instance.StatusRunning {
				detail = fmt.Sprintf("Instance %s is running on port %d. Use --force to restart.", existingInstance.ID, existingInstance.Port)
			}
			// Return existing instance info with 409 status (allows reuse of running instance).
			// JobID echoes the existing launch's handle so idempotent retries subscribe to
			// the same stream (may be empty for instances that predate jobs wiring).
			return &LoadModelResponse{
				InstanceID: existingInstance.ID,
				JobID:      existingInstance.StreamJobID,
				Model:      existingInstance.Model,
				Provider:   existingInstance.Provider,
				Port:       existingInstance.Port,
				Status:     string(status),
				Node:       "", // Empty - routing layer sets the actual host
				Message:    fmt.Sprintf("Model '%s' is already %s", req.ModelName, status),
				StatusCode: http.StatusConflict,
				Error:      fmt.Sprintf("Model '%s' is already %s", req.ModelName, status),
				Details:    detail,
			}, nil
		}
		// Force restart - stop existing instance
		if err := e.appMgr.StopInstance(ctx, existingInstance.ID); err != nil && !errors.Is(err, prov_apps.ErrInstanceNotFound) {
			return nil, err
		}
	}

	// A variant's weights are its base's: that is what has to be on disk,
	// and the provider is the one that defines the variant.
	weights := req.ModelName
	if owner, base, ok := e.appMgr.Variant(normalizedProvider, req.ModelName); ok {
		weights, normalizedProvider = base, owner
	}

	// Validate model exists in registry (unless force)
	if !req.Force {
		models, err := e.registry.ListAllModels()
		if err != nil {
			return nil, fmt.Errorf("failed to query model registry: %w", err)
		}
		if matches := findMatchingModels(weights, models); len(matches) == 0 {
			return &LoadModelResponse{
				StatusCode: http.StatusNotFound,
				Error:      "Model file not found",
				Details:    fmt.Sprintf("Model '%s' has not been downloaded", weights),
			}, nil
		}
	}

	// Auto-detect provider if not specified
	if normalizedProvider == "" {
		detectedProvider, err := e.autoDetectProvider(req.ModelName)
		if err != nil {
			if errors.Is(err, errAutoDetectModelNotFound) {
				return &LoadModelResponse{
					StatusCode: http.StatusNotFound,
					Error:      err.Error(),
				}, nil
			}
			if errors.Is(err, errAutoDetectMultipleProviders) {
				return &LoadModelResponse{
					StatusCode: http.StatusConflict,
					Error:      "Multiple apps available",
					Details:    err.Error(),
				}, nil
			}
			return nil, err
		}
		normalizedProvider = detectedProvider
	}

	// Check if provider has its own endpoint (service/external/cloud) and load via API
	resolved, hasEndpoint := e.resolveBackend(normalizedProvider)
	backendURL := ""
	if hasEndpoint {
		backendURL = resolved.Endpoint
	}
	utils.LogDebugf("[LoadExecutor] Provider '%s' endpoint check: hasEndpoint=%v, backendURL=%s", normalizedProvider, hasEndpoint, backendURL)

	if hasEndpoint {
		endpoint, _ := url.Parse(backendURL)
		providerImpl, _ := e.appMgr.Protocol(normalizedProvider)
		ctx, cancel := context.WithTimeout(ctx, constants.ClusterActionTimeout)
		defer cancel()

		utils.LogDebugf("[LoadExecutor] Loading model '%s' via provider API at %s", req.ModelName, backendURL)
		if err := providerImpl.LoadModel(ctx, protocol.TargetOf(resolved), req.ModelName, req.Parameters); err != nil {
			return nil, fmt.Errorf("failed to load model via provider API: %w", err)
		}

		// Extract port from endpoint URL (e.g., "http://localhost:11434" → 11434)
		port := 0
		if endpoint != nil && endpoint.Port() != "" {
			if p, err := strconv.Atoi(endpoint.Port()); err == nil {
				port = p
			}
		}

		loadDuration := time.Since(startTime).Milliseconds()
		utils.LogDebugf("[LoadExecutor] Model loaded successfully via endpoint provider: %s (port %d)", req.ModelName, port)

		// Record metrics and set span attributes
		llm.AddModelLoadResult(span, float64(loadDuration), "", port)
		span.SetAttributes(llm.ModelProvider(normalizedProvider))
		llm.SetSpanOK(span)

		if metrics := llm.GetMetrics(); metrics != nil {
			metrics.RecordModelLoad(ctx, req.ModelName, normalizedProvider, "success", float64(loadDuration)/1000.0)
		}

		e.triggerResourceRefresh()

		return &LoadModelResponse{
			InstanceID: "", // No instance ID for endpoint providers
			Model:      req.ModelName,
			Provider:   normalizedProvider,
			Port:       port, // Port from endpoint provider endpoint
			Status:     "running",
			Node:       "", // Empty - routing layer sets the actual host
			Message:    fmt.Sprintf("Model '%s' loaded successfully", req.ModelName),
		}, nil
	}

	// Check if instance already exists (check again after potential force-stop above)
	existingInstance, found := e.appMgr.Instances().GetByModelEndpoint(req.ModelName, req.Endpoint)
	if found {
		status := existingInstance.GetStatus()
		switch status {
		case instance.StatusRunning:
			// Already running - return immediately. JobID may be empty
			// (launch-job already terminal post-readiness; kept here
			// purely for idempotent-subscribe dedupe if still within
			// the jobs TTL window).
			utils.LogDebugf("[LoadExecutor] Model %s already running on port %d (fast path)", req.ModelName, existingInstance.Port)
			return &LoadModelResponse{
				InstanceID: existingInstance.ID,
				JobID:      existingInstance.StreamJobID,
				Model:      existingInstance.Model,
				Provider:   existingInstance.Provider,
				Port:       existingInstance.Port,
				Status:     string(status),
				Node:       "", // Empty - routing layer sets the actual host
				Message:    "Model already running",
			}, nil

		case instance.StatusStarting:
			// Already starting - return the LIVE jobID so a retry
			// subscribes to the SAME launch progress stream.
			utils.LogDebugf("[LoadExecutor] Model %s is already starting on port %d", req.ModelName, existingInstance.Port)
			return &LoadModelResponse{
				InstanceID: existingInstance.ID,
				JobID:      existingInstance.StreamJobID,
				Model:      existingInstance.Model,
				Provider:   existingInstance.Provider,
				Port:       existingInstance.Port,
				Status:     string(instance.StatusStarting),
				Node:       "", // Empty - routing layer sets the actual host
				Message:    "Model launch already in progress; subscribe to job_id",
			}, nil

		case instance.StatusFailed, instance.StatusStopped:
			// Previous instance failed - clean it up and launch new one
			utils.LogDebugf("[LoadExecutor] Previous instance in state %s, cleaning up...", status)
			if err := e.appMgr.StopInstance(ctx, existingInstance.ID); err != nil && !errors.Is(err, prov_apps.ErrInstanceNotFound) {
				return nil, err
			}
		}
	}

	// Launch new instance (async - returns immediately after process starts)
	utils.LogDebugf("[LoadExecutor] Launching model %s with provider %s", req.ModelName, normalizedProvider)
	launchModel := req.requestedModel
	if launchModel == "" {
		launchModel = req.ModelName
	}
	inst, err := e.launchOnDemand(ctx, launchModel, normalizedProvider, req.Endpoint, req.Parameters, req.Environment)
	if err != nil {
		llm.SetSpanError(span, err)
		if metrics := llm.GetMetrics(); metrics != nil {
			metrics.RecordModelLoad(ctx, req.ModelName, normalizedProvider, "failed", 0)
		}
		return nil, fmt.Errorf("failed to launch model: %w", err)
	}

	// Invalidate cache after launching model (routing table will update on next query)
	e.invalidateCache()

	loadDuration := time.Since(startTime).Milliseconds()
	utils.LogDebugf("[LoadExecutor] Model launch initiated: %s on port %d (instance %s, status: %s)",
		inst.Model, inst.Port, inst.ID, inst.GetStatus())

	// Record metrics and set span attributes
	llm.AddModelLoadResult(span, float64(loadDuration), inst.ID, inst.Port)
	span.SetAttributes(llm.ModelProvider(normalizedProvider))
	llm.SetSpanOK(span)

	if metrics := llm.GetMetrics(); metrics != nil {
		metrics.RecordModelLoad(ctx, req.ModelName, normalizedProvider, "success", float64(loadDuration)/1000.0)
		metrics.RecordInstanceStart(ctx, req.ModelName, normalizedProvider)
	}

	e.triggerResourceRefresh()

	return &LoadModelResponse{
		InstanceID: inst.ID,
		JobID:      inst.StreamJobID,
		Model:      inst.Model,
		Provider:   inst.Provider,
		Port:       inst.Port,
		Status:     string(inst.GetStatus()),
		Node:       "", // Empty - routing layer sets the actual host
		Message:    "Model launch accepted; subscribe to job_id",
	}, nil
}

// PreviewLocalRun generates a preview of what would be run
func (e *LoadExecutor) PreviewLocalRun(ctx context.Context, req *PreviewRunRequest) (*PreviewRunResponse, error) {
	utils.LogDebugf("[LoadExecutor] PreviewLocalRun: model=%s, provider=%s",
		req.ModelName, req.Provider)

	if e.appMgr != nil {
		if err := e.appMgr.ValidateModel(ctx, req.ModelName); err != nil {
			return nil, newProblemError(extractStatusCode(err), http.StatusText(extractStatusCode(err)), err.Error())
		}
	}
	normalizedApp := utils.NormalizeAppType(req.Provider)
	if normalizedApp == "" {
		// A plain error here surfaced as a 500 "failed to preview run"
		// with no cause; the caller simply left a field out.
		return nil, newProblemError(http.StatusBadRequest, "Bad Request",
			"provider is required: preview renders the launch command for one provider")
	}
	// Reject unknown or disabled providers up front so the response is a
	// structured error envelope, not a 200 + all-empty PreviewRunResponse.
	// Coord has an equivalent fast-path gate in RunsController.PreviewRun;
	// this worker-side check covers the drift window when coord and
	// worker have different appsConfig snapshots (peer-sync in flight).
	if e.isProviderEnabled != nil && !e.isProviderEnabled(normalizedApp) {
		return nil, newProblemError(http.StatusBadRequest, "Bad Request",
			fmt.Sprintf("provider %q is not configured or enabled", normalizedApp))
	}

	if e.appMgr != nil {
		if _, err := e.appMgr.AdmitLocalModel(ctx, req.ModelName); err != nil {
			return nil, newProblemError(extractStatusCode(err), http.StatusText(extractStatusCode(err)), err.Error())
		}
	}

	mr, err := e.mergeAndResolve(req.ModelName, normalizedApp, req.Endpoint, req.Parameters, req.Environment)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve parameters: %w", err)
	}

	// Build preview command string from app runtime config
	fullCommand := fmt.Sprintf("%s run %s", normalizedApp, req.ModelName)
	workingDir := "."
	if mr.ProviderCfg.Runtime != nil {
		exec := mr.ProviderCfg.Runtime.Execution
		if exec.Command != "" {
			fullCommand = exec.Command
			if mr.Command != "" {
				fullCommand = mr.Command
			}
			for _, arg := range exec.Args {
				fullCommand += " " + arg
			}
		}
		if exec.WorkingDir != "" {
			workingDir = exec.WorkingDir
		}
	}

	// Pool errors (no pool, exhausted) are non-fatal: leave Port=0 so the
	// preview still renders. The real launch surfaces the same error.
	projectedPort := 0
	if e.appMgr != nil {
		if p, err := e.appMgr.Ports().ProjectForProvider(normalizedApp); err == nil {
			projectedPort = p
		}
	}

	return &PreviewRunResponse{
		Model:            req.ModelName,
		Provider:         normalizedApp,
		Node:             e.getNodename(),
		Port:             projectedPort,
		FullCommand:      fullCommand,
		Parameters:       mr.Params,
		ParameterSources: mr.ParamSources,
		Environment:      mr.Env,
		WorkingDir:       workingDir,
	}, nil
}

// HandleInternalLoadModel handles POST /zzrouter/internal/runs/load (internal API)
func (e *LoadExecutor) HandleInternalLoadModel(c *gin.Context) {
	var req LoadModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, err.Error())
		return
	}

	resp, err := e.LoadLocalModel(c.Request.Context(), &req)
	if err != nil {
		slog.Error("Failed to load model", "model", err)
		var memory *process.MemoryFitError
		if errors.Is(err, config.ErrModelNameConflict) || errors.As(err, &memory) ||
			errors.Is(err, process.ErrMemoryObservation) || errors.Is(err, process.ErrMemoryBudgetInvalid) ||
			errors.Is(err, prov_apps.ErrAtCapacity) {
			respondProviderErr(c, err)
		} else {
			InternalNodeError(c, "failed to load model")
		}
		return
	}

	// Check for error responses (conflict, etc.)
	if resp.StatusCode != 0 && resp.StatusCode != http.StatusOK {
		// Return full response including instance data (for 409 conflicts with existing instances)
		c.JSON(resp.StatusCode, resp)
		return
	}

	c.JSON(http.StatusOK, resp)
}

// HandleInternalPreviewRun handles POST /zzrouter/internal/runs/preview (internal API)
func (e *LoadExecutor) HandleInternalPreviewRun(c *gin.Context) {
	var req PreviewRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, err.Error())
		return
	}

	preview, err := e.PreviewLocalRun(c.Request.Context(), &req)
	if err != nil {
		// Preserve structured *RoutedError statuses (e.g. 400 unknown
		// provider) so coord's PreviewRun surfaces the right HTTP code
		// to the public caller.
		var rerr *RoutedError
		if errors.As(err, &rerr) {
			rerr.Respond(c)
			return
		}
		slog.Error("Failed to preview run", "run", err)
		InternalNodeError(c, "failed to preview run")
		return
	}

	c.JSON(http.StatusOK, preview)
}
