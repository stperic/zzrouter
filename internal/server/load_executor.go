package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
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
	appMgr      *prov_apps.ProviderAppManager
	registry    modelLister
	getNodename func() string
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
	if owner, _, ok := e.appMgr.Variant("", modelName); ok {
		return owner, nil
	}
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

// NewLoadExecutor creates the local load and preview adapter.
func NewLoadExecutor(appMgr *prov_apps.ProviderAppManager, registry modelLister, getNodename func() string) *LoadExecutor {
	return &LoadExecutor{appMgr: appMgr, registry: registry, getNodename: getNodename}
}

// LoadLocalModel delegates every start decision to the manager.
func (e *LoadExecutor) LoadLocalModel(ctx context.Context, req *LoadModelRequest) (*LoadModelResponse, error) {
	if e.appMgr == nil {
		return nil, fmt.Errorf("provider app manager not initialized")
	}
	if err := validateRunTarget(e.appMgr.AppsConfig, req.Provider, req.ModelName, req.Endpoint); err != nil {
		return nil, err
	}
	provider := req.Provider
	if provider == "" {
		var err error
		provider, err = e.autoDetectProvider(e.appMgr.CanonicalModelName(req.ModelName))
		if err != nil {
			return nil, err
		}
	}
	result, err := e.appMgr.EnsureRun(ctx, prov_apps.EnsureRequest{Provider: provider, Model: req.ModelName, Endpoint: prov_apps.Endpoint(req.Endpoint), Parameters: req.Parameters, EnvVars: req.Environment, Force: req.Force})
	if err != nil {
		return nil, err
	}
	run := result.Run
	if run == nil {
		return &LoadModelResponse{Model: e.appMgr.CanonicalModelName(req.ModelName), Provider: utils.NormalizeAppType(provider), Status: "running", Message: "The provider serves this model without a run"}, nil
	}
	status := run.GetStatus()
	reused := !result.Started && (status == instance.StatusRunning || status == instance.StatusStarting)
	message := "Model launch accepted; subscribe to job_id"
	if reused {
		message = fmt.Sprintf("Model '%s' is already %s", run.Model, status)
	}
	return &LoadModelResponse{BaseURL: run.BaseURL, Reused: reused, InstanceID: run.ID, JobID: run.StreamJobID, Model: run.Model, Provider: run.Provider, Port: run.Port, Status: string(status), Message: message}, nil
}

// PreviewLocalRun generates a preview of what would be run
func (e *LoadExecutor) PreviewLocalRun(ctx context.Context, req *PreviewRunRequest) (*PreviewRunResponse, error) {
	utils.LogDebugf("[LoadExecutor] PreviewLocalRun: model=%s, provider=%s",
		req.ModelName, req.Provider)

	if err := validateRunTarget(e.appMgr.AppsConfig, req.Provider, req.ModelName, req.Endpoint); err != nil {
		return nil, err
	}
	if err := e.appMgr.ValidateModel(ctx, req.ModelName); err != nil {
		return nil, err
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
		return nil, err
	}

	projectedPort := 0
	if p, err := e.appMgr.Ports().ProjectForProvider(normalizedApp); err == nil {
		projectedPort = p
	}
	launch, err := e.appMgr.PreviewLaunch(ctx, prov_apps.EnsureRequest{Provider: normalizedApp, Model: req.ModelName, Endpoint: prov_apps.Endpoint(req.Endpoint), Parameters: req.Parameters, EnvVars: req.Environment}, projectedPort)
	if err != nil {
		return nil, err
	}

	workingDir := launch.WorkingDir
	if workingDir == "" {
		workingDir = "."
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
		slog.Error("Failed to load model", "model", req.ModelName, "error", err)
		RespondToError(c, err)
		return
	}

	status := http.StatusOK
	if resp.Reused {
		status = http.StatusConflict
	}
	c.JSON(status, resp)
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
		slog.Error("Failed to preview run", "model", req.ModelName, "error", err)
		RespondToError(c, err)
		return
	}

	c.JSON(http.StatusOK, preview)
}
