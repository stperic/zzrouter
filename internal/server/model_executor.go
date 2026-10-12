package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/utils"
)

// InternalExecutor handles direct execution against local providers
// Routing remains with the caller; local mutations publish catalog evidence.
type InternalExecutor struct {
	publishCatalog     func(context.Context) error
	deletionManager    *prov_apps.ProviderAppManager
	modelCache         *cache.Cache
	registry           *modelregistry.Registry
	getNodename        func() string
	appsConfig         func() *pkgConfig.AppsConfig
	httpClient         *http.Client
	getHFConnector     func() *huggingface.Connector
	isCloudProvider    func(string) bool
	configStore        *pkgConfig.AppsConfigStore
	getResourceMetrics func() any
}

// NewInternalExecutor creates a new internal API executor
func NewInternalExecutor(
	registry *modelregistry.Registry,
	getNodename func() string,
	appsConfig func() *pkgConfig.AppsConfig,
	httpClient *http.Client,
	getHFConnector func() *huggingface.Connector,
	isCloudProvider func(string) bool,
	configStore *pkgConfig.AppsConfigStore,
	publishCatalog func(context.Context) error,
	getResourceMetrics func() any,
) *InternalExecutor {
	return &InternalExecutor{
		registry:           registry,
		getNodename:        getNodename,
		appsConfig:         appsConfig,
		httpClient:         httpClient,
		getHFConnector:     getHFConnector,
		isCloudProvider:    isCloudProvider,
		configStore:        configStore,
		publishCatalog:     publishCatalog,
		getResourceMetrics: getResourceMetrics,
	}
}

// GetLocalModels retrieves models from local providers (Ollama, vLLM, HuggingFace, etc.)
// This is the V2 equivalent of getLocalModels but as a proper module
func (e *InternalExecutor) GetLocalModels(ctx context.Context) ([]map[string]any, error) {
	models, _, err := e.localModelSnapshot(ctx)
	return models, err
}

func (e *InternalExecutor) localModelSnapshot(ctx context.Context) ([]map[string]any, []cache.InventoryUnavailable, error) {
	if e.modelCache == nil {
		return nil, nil, fmt.Errorf("model cache not initialized")
	}
	return e.modelCache.LocalModelSnapshot(ctx)
}

// ShowLocalModel reads current local source evidence for the selected physical identity.
func (e *InternalExecutor) ShowLocalModel(ctx context.Context, req *ShowModelRequest) (map[string]any, error) {
	target, weights, err := e.localShowTarget(ctx, req)
	if err != nil {
		return nil, err
	}
	app := req.App
	if target != nil {
		app = target.AssignedApp
	}
	source := constants.RepoOllama
	if target != nil {
		source = target.SourceRepo
	}
	var details map[string]any
	switch source {
	case constants.RepoOllama:
		backend, ok := backend.NewResolver(e.appsConfig).Resolve(app)
		if !ok {
			return nil, newRouteProblemError(http.StatusServiceUnavailable, "Service Unavailable", "selected provider backend is unavailable", httperr.CodeClusterUnavailable, e.getNodename())
		}
		details, err = e.getOllamaModelDetails(ctx, backend, weights, req.Verbose)
	case constants.RepoHuggingFace:
		connector := e.getHFConnector()
		if connector == nil {
			return nil, newProblemError(http.StatusServiceUnavailable, "Service Unavailable", "HuggingFace metadata reader is unavailable")
		}
		details, err = connector.ShowModel(ctx, target.SourceID)
	default:
		return nil, newRouteProblemError(http.StatusNotImplemented, "Not Implemented", "this model source has no detail reader; use the catalog and serving probes", httperr.CodeModelDetailUnsupported, e.getNodename())
	}
	if err != nil {
		var wire httperr.Error
		var routed *RoutedError
		if errors.As(err, &wire) || errors.As(err, &routed) {
			return nil, err
		}
		return nil, newRoutedTransportError(err, e.getNodename())
	}
	addressed := req.ModelName
	if target != nil && weights == req.ModelName {
		addressed = target.Name
	}
	details["name"], details["node"], details["assigned_app"], details["_provider"] = addressed, e.getNodename(), app, source
	if target != nil {
		details["source_repo"], details["source_id"] = target.SourceRepo, target.SourceID
	}
	if req.Compact {
		return compactModelShow(details)
	}
	return details, nil
}

// localShowTarget selects current physical metadata before the provider read.
func (e *InternalExecutor) localShowTarget(ctx context.Context, req *ShowModelRequest) (*metadata.ModelMetadata, string, error) {
	if e.registry == nil {
		return nil, "", newRouteProblemError(http.StatusServiceUnavailable, "Service Unavailable", "model registry is unavailable", httperr.CodeClusterUnavailable, e.getNodename())
	}
	models, failures, err := e.registry.ModelSnapshot(ctx)
	if err != nil {
		return nil, "", newRoutedTransportError(err, e.getNodename())
	}
	if failures[req.App] != "" {
		return nil, "", newRouteProblemError(http.StatusServiceUnavailable, "Service Unavailable", "selected provider inventory is unavailable", httperr.CodeClusterUnavailable, e.getNodename())
	}
	weights := req.ModelName
	provider, configured := e.appsConfig().LookupApp(req.App)
	if configured {
		weights = provider.WeightsOf(weights)
	}
	weights, _, _ = strings.Cut(weights, "#")
	var target *metadata.ModelMetadata
	for _, model := range models {
		nameMatches := strings.EqualFold(model.Name, weights) || (req.SourceID == "" && strings.EqualFold(model.SourceID, weights))
		providerMatches := req.App == "" || strings.EqualFold(model.AssignedApp, req.App)
		sourceMatches := req.Registry == "" || strings.EqualFold(model.SourceRepo, req.Registry)
		idMatches := req.SourceID == "" || strings.EqualFold(model.SourceID, req.SourceID)
		if !nameMatches || !providerMatches || !sourceMatches || !idMatches {
			continue
		}
		if target != nil {
			return nil, "", newRouteProblemError(http.StatusConflict, "Conflict", "model has several physical sources; use an exact source_id from GET /zzrouter/v1/models", httperr.CodeModelSourceAmbiguous, e.getNodename())
		}
		target = model
	}
	uncataloged := target == nil && req.AllowUncataloged && configured && provider.Protocol == pkgConfig.ProtocolOllama
	if target == nil && !uncataloged {
		if len(failures) > 0 && req.App == "" {
			return nil, "", newRouteProblemError(http.StatusServiceUnavailable, "Service Unavailable", "model inventory is unavailable", httperr.CodeClusterUnavailable, e.getNodename())
		}
		return nil, "", newProblemError(http.StatusNotFound, "Not Found", "model is absent from the local catalog")
	}
	return target, weights, nil
}

// getOllamaModelDetails calls Ollama's /api/show endpoint
func (e *InternalExecutor) getOllamaModelDetails(ctx context.Context, daemon *backend.Resolved, modelName string, verbose bool) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"name": modelName, "verbose": verbose})
	if err != nil {
		return nil, err
	}
	req, err := daemon.NewRequest(ctx, http.MethodPost, "/api/show", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseRoutedError(resp.StatusCode, body, resp.Header)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil || result == nil {
		return nil, newRouteProblemError(http.StatusBadGateway, "Bad Gateway", "provider model detail must be one JSON object", httperr.CodeUpstreamShape, e.getNodename())
	}
	return result, nil
}

// ============================================================================
// Internal HTTP Handlers
// ============================================================================

// HandleInternalListModels is the INTERNAL endpoint for listing models
// Local catalog publication also supplies the worker's admission evidence.
// Endpoint: GET /zzrouter/internal/models
func (e *InternalExecutor) HandleInternalListModels(c *gin.Context) {
	utils.LogDebugf("[InternalAPI] /zzrouter/internal/models called from %s", c.ClientIP())

	models, unavailable, err := e.localModelSnapshot(c.Request.Context())
	if err != nil {
		utils.LogDebugf("[InternalAPI] Error getting local models: %v", err)
		InternalNodeError(c, "Failed to get local models")
		return
	}

	utils.LogDebugf("[InternalAPI] Returning %d models", len(models))

	resp := gin.H{"models": models}
	resp["inventory_unavailable"] = unavailable

	// Piggyback resource metrics so the coordinator doesn't need a separate broadcast
	if metrics := e.getResourceMetrics(); metrics != nil {
		resp["resources"] = metrics
	}

	c.JSON(http.StatusOK, resp)
}

// HandleInternalRescanModels is the INTERNAL endpoint that drops the
// node's registry scan cache so the next ListAllModels call re-queries
// every source (Ollama daemon /api/tags, filesystem GGUF scan, etc.).
//
// Endpoint: POST /zzrouter/v1/internal/models/rescan
//
// Called by the coord-side ModelService.handleRescanModels broadcast
// when an operator hits POST /zzrouter/v1/models/scan without (or with
// a non-local) ?node= filter. Without this handler the broadcast 404s
// and Ollama models pulled outside the manager's lifecycle stay
// invisible to the cluster catalog until the next pull-driven
// invalidation — which never comes for daemons started post-boot.
func (e *InternalExecutor) HandleInternalRescanModels(c *gin.Context) {
	utils.LogDebugf("[InternalAPI] /zzrouter/v1/internal/models/rescan called from %s", c.ClientIP())

	if e.registry == nil {
		InternalNodeError(c, "model registry not initialized")
		return
	}
	if !publishOr503(c, func() error { return e.publishCatalog(c.Request.Context()) }) {
		return
	}

	models, err := e.GetLocalModels(c.Request.Context())
	if err != nil {
		// Invalidation succeeded but the post-invalidate enumerate
		// failed — surface as 5xx so the operator can distinguish
		// "this node truly has zero models" from "the enumerate path
		// errored after we dropped the cache".
		InternalNodeError(c, fmt.Sprintf("post-invalidate enumeration failed: %v", err))
		return
	}
	// Surface a per-source breakdown so an operator (or the verify
	// script) can spot when a downstream connector is dropping
	// entries — e.g. Ollama daemon reports N but the registry
	// returns N-1.
	bySource := make(map[string]int, 4)
	names := make([]string, 0, len(models))
	for _, m := range models {
		src, _ := m["source_repo"].(string)
		if src == "" {
			src = "unknown"
		}
		bySource[src]++
		if n, ok := m["name"].(string); ok {
			names = append(names, n)
		}
	}
	respondSuccess(c, "Rescan completed", gin.H{
		"models_found": len(models),
		"by_source":    bySource,
		"names":        names,
	})
}

// HandleInternalShowModel is the INTERNAL endpoint for showing model details
// Called by the router to get actual model data - NO routing, NO cache
// Endpoint: GET /zzrouter/internal/models/show?model=<name>&verbose=<bool>
func (e *InternalExecutor) HandleInternalShowModel(c *gin.Context) {
	req := &ShowModelRequest{ModelName: QueryModel(c), App: QueryProvider(c), Registry: QueryRegistry(c), SourceID: c.Query("source_id"), Verbose: c.Query("verbose") == "true", Compact: c.Query("compact") == "true", AllowUncataloged: c.Query("allow_uncataloged") == "true"}
	if req.ModelName == "" {
		BadRequest(c, "model parameter is required")
		return
	}
	details, err := e.ShowLocalModel(c.Request.Context(), req)
	if err != nil {
		RespondToError(c, err)
		return
	}
	c.JSON(http.StatusOK, details)
}
