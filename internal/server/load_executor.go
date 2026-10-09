package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	launchOnDemand    func(ctx context.Context, modelName, providerType, endpoint string, parameters, environment map[string]string) (*instance.Instance, error)
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
	launchOnDemand func(ctx context.Context, modelName, providerType, endpoint string, parameters, environment map[string]string) (*instance.Instance, error),
	getNodename func() string,
	refreshResourceMetrics func(),
) *LoadExecutor {
	return &LoadExecutor{
		appMgr:                 appMgr,
		isProviderEnabled:      isProviderEnabled,
		registry:               registry,
		resolveBackend:         resolveBackend,
		launchOnDemand:         launchOnDemand,
		getNodename:            getNodename,
		refreshResourceMetrics: refreshResourceMetrics,
	}
}

// LoadLocalModel loads a model on the local runs manager
// Uses singleflight to prevent thundering herd when multiple clients request the same model
func (e *LoadExecutor) LoadLocalModel(ctx context.Context, req *LoadModelRequest) (*LoadModelResponse, error) {
	prepared, response, err := e.prepareLocalLoad(ctx, req)
	if err != nil || response != nil {
		return response, err
	}
	resolved := *prepared

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

func (e *LoadExecutor) existingLocalLoad(req *LoadModelRequest) *LoadModelResponse {
	// Check if model is already starting/running
	if existingInstance, found := e.appMgr.Instances().GetByModelEndpoint(req.ModelName, req.Endpoint); found {
		status := existingInstance.GetStatus()
		if !req.Force && (status == instance.StatusStarting || status == instance.StatusRunning) {
			if req.Provider != "" && utils.NormalizeAppType(existingInstance.Provider) != req.Provider {
				return &LoadModelResponse{StatusCode: http.StatusBadRequest, Error: fmt.Sprintf("model %q is held by provider %q; requested provider %q cannot reuse that run. Stop it before changing provider", req.ModelName, existingInstance.Provider, req.Provider)}
			}
			detail := fmt.Sprintf("Instance %s is %s. Use --force to restart.", existingInstance.ID, status)
			if status == instance.StatusRunning {
				detail = fmt.Sprintf("Instance %s is running on port %d. Use --force to restart.", existingInstance.ID, existingInstance.Port)
			}
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
			}
		}
	}

	return nil
}

func (e *LoadExecutor) prepareLocalLoad(ctx context.Context, req *LoadModelRequest) (*LoadModelRequest, *LoadModelResponse, error) {
	// Check if provider app manager is available
	if e.appMgr == nil {
		err := fmt.Errorf("provider app manager not initialized")
		return nil, nil, err
	}

	requestedModel := req.requestedModel
	if requestedModel == "" {
		requestedModel = req.ModelName
	}
	if err := e.appMgr.ValidateModel(ctx, requestedModel); err != nil {
		return nil, nil, err
	}
	resolved := *req
	resolved.requestedModel = requestedModel
	resolved.ModelName = e.canonicalModelName(requestedModel)
	req = &resolved

	// Normalize and validate provider
	normalizedProvider, err := e.normalizeAndValidateProvider(req.Provider)
	if err != nil {
		//nolint:nilerr // an unknown or disabled provider is the caller's 400, carried in the response
		return nil, &LoadModelResponse{
			StatusCode: http.StatusBadRequest,
			Error:      err.Error(),
		}, nil
	}

	req.Provider = normalizedProvider
	if response := e.existingLocalLoad(req); response != nil {
		return nil, response, nil
	}

	// Variants use their base weights and the provider that defines them.
	weights := req.ModelName
	if owner, base, ok := e.appMgr.Variant(normalizedProvider, req.ModelName); ok {
		weights, normalizedProvider = base, owner
	}

	// Validate model exists in registry (unless force)
	if !req.Force {
		models, err := e.registry.ListAllModels()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to query model registry: %w", err)
		}
		if matches := findMatchingModels(weights, models); len(matches) == 0 {
			return nil, &LoadModelResponse{
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
				return nil, &LoadModelResponse{
					StatusCode: http.StatusNotFound,
					Error:      err.Error(),
				}, nil
			}
			if errors.Is(err, errAutoDetectMultipleProviders) {
				return nil, &LoadModelResponse{
					StatusCode: http.StatusConflict,
					Error:      "Multiple apps available",
					Details:    err.Error(),
				}, nil
			}
			return nil, nil, err
		}
		normalizedProvider = detectedProvider
	}

	req.Provider = normalizedProvider
	if !req.Force {
		if response := e.existingLocalLoad(req); response != nil {
			return nil, response, nil
		}
		if err := e.stopLocalLoad(ctx, req); err != nil {
			return nil, nil, err
		}
	}
	return req, nil, nil
}

func (e *LoadExecutor) stopLocalLoad(ctx context.Context, req *LoadModelRequest) error {
	if existing, found := e.appMgr.Instances().GetByModelEndpoint(req.ModelName, req.Endpoint); found &&
		(req.Force || existing.GetStatus().IsTerminal()) {
		if err := e.appMgr.StopInstance(ctx, existing.ID); err != nil && !errors.Is(err, prov_apps.ErrInstanceNotFound) {
			return err
		}
	}
	return nil
}

// doLoadLocalModel performs the actual model loading (called by singleflight)
func (e *LoadExecutor) doLoadLocalModel(ctx context.Context, req *LoadModelRequest) (*LoadModelResponse, error) {
	// Start model load span
	ctx, span := llm.StartModelLoadSpan(ctx, req.ModelName, req.Provider)
	defer span.End()
	startTime := utils.Now()

	utils.LogDebugf("[LoadExecutor] doLoadLocalModel: model=%s, provider=%s, force=%v",
		req.ModelName, req.Provider, req.Force)

	normalizedProvider := req.Provider
	if response := e.existingLocalLoad(req); response != nil {
		return response, nil
	}
	if err := e.stopLocalLoad(ctx, req); err != nil {
		return nil, err
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

	if err := e.appMgr.ValidateModel(ctx, req.ModelName); err != nil {
		return nil, newProblemError(extractStatusCode(err), http.StatusText(extractStatusCode(err)), err.Error())
	}
	normalizedApp := utils.NormalizeAppType(req.Provider)
	if normalizedApp == "" {
		// A plain error here surfaced as a 500 "failed to preview run"
		// with no cause; the caller simply left a field out.
		return nil, newProblemError(http.StatusBadRequest, "Bad Request",
			"provider is required: preview renders the launch command for one provider")
	}
	// Check the worker's current config independently of the coordinator snapshot.
	if err := previewProviderError(e.appMgr, normalizedApp); err != nil {
		return nil, err
	}

	if _, err := e.appMgr.AdmitLocalModel(ctx, req.ModelName); err != nil {
		return nil, newProblemError(extractStatusCode(err), http.StatusText(extractStatusCode(err)), err.Error())
	}

	projectedPort := 0
	if p, err := e.appMgr.Ports().ProjectForProvider(normalizedApp); err == nil {
		projectedPort = p
	}
	launch, err := e.appMgr.PreviewLaunch(prov_apps.LaunchRequest{Provider: normalizedApp, Model: req.ModelName, Endpoint: prov_apps.Endpoint(req.Endpoint), Parameters: req.Parameters, EnvVars: req.Environment}, projectedPort)
	if err != nil {
		return nil, err
	}
	workingDir := "."
	if launch.Config.Runtime != nil && launch.Config.Runtime.Execution.WorkingDir != "" {
		workingDir = launch.Config.Runtime.Execution.WorkingDir
	}
	fullCommand := strings.Join(append([]string{launch.Command}, launch.Args...), " ")

	return &PreviewRunResponse{
		Model:            launch.Model,
		Provider:         normalizedApp,
		Node:             e.getNodename(),
		Port:             projectedPort,
		FullCommand:      fullCommand,
		Parameters:       launch.Params,
		ParameterSources: launch.Sources,
		Environment:      launch.Environment,
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
