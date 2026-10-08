// package server provides HTTP handlers for the zzrouter host server.
// Model instance runtime - on-demand container launching and management

package server

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	modelcache "github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

// mergeTiersForTarget resolves the provider tier tree for one model on
// one named node, returning the flat maps plus the tier each parameter
// came from.
//
// The node argument is the node that would RUN the model, not the node
// resolving. Passing the wrong one — or none — is worse than not
// resolving at all: the receiving node layers what arrives over its own
// walk as the request tier, the last word, so a resolution that could
// not see nodes[N] does not merely disagree with N, it outranks it.
func mergeTiersForTarget(
	appsCfg *config.AppsConfig,
	provider, node, model, endpoint string,
	params, environment map[string]string,
) (mergedParams, mergedEnv, sources map[string]string) {
	if appsCfg == nil || provider == "" || node == "" {
		return nil, nil, nil
	}
	cfg, exists := appsCfg.LookupApp(utils.NormalizeAppType(provider))
	if !exists {
		return nil, nil, nil
	}

	resolved := cfg.ResolveEndpoint(node, model, endpoint)
	mergedParams = config.FlattenParameters(resolved.Parameters)
	mergedEnv = config.FlattenEnvironment(resolved.Environment)

	sources = make(map[string]string, len(resolved.Parameters))
	for k, rv := range resolved.Parameters {
		sources[k] = rv.Tier.String()
	}

	// Request-time overlay takes precedence (Tier 4 — ephemeral).
	for k, v := range params {
		mergedParams[k] = v
		sources[k] = config.TierRequest.String()
	}
	for k, v := range environment {
		mergedEnv[k] = v
	}
	return mergedParams, mergedEnv, sources
}

// preMergeForRequest resolves the tier tree into a preview request
// bound for target, and reports which tier each value came from. The
// node that renders the preview walks its own tree too and layers what
// arrives on top, so what this writes decides the answer.
//
// Flattening is what destroys provenance: the receiving node sees only
// a map of values arriving as request parameters and can do nothing but
// call them all `request`. The coordinator owns the provider tree, so
// this is the only place that can answer "where did this value come
// from" truthfully, and the answer has to be carried out rather than
// recomputed downstream.
//
// An empty target means the caller could not say which node would run
// the model — a broadcast, or a model no node has claimed. Then nothing
// is merged and whichever node answers resolves its own tree, which is
// the right answer for a node this one cannot name.
func preMergeForRequest(appsCfg *config.AppsConfig, req *LoadModelRequest, target string) map[string]string {
	params, env, sources := mergeTiersForTarget(
		appsCfg, req.Provider, target, req.ModelName, req.Endpoint, req.Parameters, req.Environment)
	if sources == nil {
		return nil
	}
	req.Parameters = params
	req.Environment = env

	utils.LogDebugf("[PreMerge] Merged %d params + %d env vars from coordinator config for %s/%s (node=%s)",
		len(req.Parameters), len(req.Environment), req.Provider, req.ModelName, target)
	return sources
}

// ============================================================================
// Parameter Resolve (launcher path)
// ============================================================================

// mergeResolveResult holds the resolved parameters a preview reports.
type mergeResolveResult struct {
	Params       map[string]string
	Env          map[string]string
	ParamSources map[string]string
	ProviderCfg  config.ServiceConfig
	Command      string
}

// resolveForLaunch walks the 4-tier tree for this node and layers the
// request tier on top, stripping auto sentinels afterwards so a caller
// can neutralise a configured value.
//
// This renders a preview. A launch does NOT come through here: the
// answer a process is started with is computed once, inside
// LaunchInstance, and a second copy of it upstream is how the launch
// path used to hand the manager a merged map that then read as the
// caller's own — see buildInstanceConfig.
func (s *Server) resolveForLaunch(modelName, appType, endpoint string, parameters, environment map[string]string) (*mergeResolveResult, error) {
	normalizedType := utils.NormalizeAppType(appType)

	if s.appsConfig == nil {
		return nil, fmt.Errorf("providers config not loaded")
	}
	cfg, exists := s.appsConfig.LookupApp(normalizedType)
	if !exists {
		return nil, fmt.Errorf("provider %s not configured", normalizedType)
	}

	// Walk the tier tree for this node, then layer the request on top.
	// The only caller is PreviewLocalRun: a launch resolves inside
	// LaunchInstance, and a preview rendered from anything but this
	// node's own tree would describe a launch that will not happen.
	resolved := cfg.ResolveEndpoint(s.node.Nodename(), modelName, endpoint)
	mergedParams := config.FlattenParameters(resolved.Parameters)
	mergedEnv := config.FlattenEnvironment(resolved.Environment)
	paramSources := make(map[string]string)
	for k, rv := range resolved.Parameters {
		paramSources[k] = rv.Tier.String()
	}
	for k, v := range parameters {
		mergedParams[k] = v
		paramSources[k] = config.TierRequest.String()
	}
	for k, v := range environment {
		mergedEnv[k] = v
	}

	command := ""
	if s.providers.appMgr != nil {
		effective, err := s.providers.appMgr.ResolveLaunchParameters(prov_apps.LaunchRequest{Provider: normalizedType, Model: modelName, Endpoint: prov_apps.Endpoint(endpoint), Parameters: parameters, EnvVars: environment})
		if err != nil {
			return nil, err
		}
		mergedParams, mergedEnv, cfg = effective.Params, effective.Environment, effective.Config
		for key, source := range effective.Sources {
			paramSources[key] = source
		}
		command = effective.ExecutionCommand(normalizedType)
	} else {
		features, err := prov_apps.LocalizeModelFeatures(cfg, modelName, mergedParams)
		if err != nil {
			return nil, err
		}
		mergedParams = features.Params
		for key, source := range features.Sources {
			paramSources[key] = source
		}
	}
	mergedParams = config.FilterAutoValues(mergedParams)

	return &mergeResolveResult{
		Params:       mergedParams,
		Env:          mergedEnv,
		ParamSources: paramSources,
		ProviderCfg:  cfg,
		Command:      command,
	}, nil
}

// ============================================================================
// Build Instance Config
// ============================================================================

// instanceBuildResult wraps the result of buildInstanceConfig.
type instanceBuildResult struct {
	LaunchReq      prov_apps.LaunchRequest
	ProviderConfig *config.ServiceConfig
	ModelPath      string
}

// buildInstanceConfig builds instance configuration for launch: model
// metadata, the disk path its weights live at, and the provider's
// runtime config. Parameter resolution is deliberately not part of it.
func (s *Server) buildInstanceConfig(ctx context.Context, modelName, providerType, endpoint string, parameters, environment map[string]string) (*instanceBuildResult, error) {
	if err := s.validateModel(ctx, modelName); err != nil {
		return nil, err
	}
	normalizedType := utils.NormalizeAppType(providerType)

	// Get model metadata
	if s.model.Registry == nil {
		return nil, fmt.Errorf("model registry not available - check provider configuration")
	}
	models, _, err := s.model.Registry.ModelSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}

	if s.appsConfig == nil {
		return nil, fmt.Errorf("providers config not loaded")
	}
	providerCfg, exists := s.appsConfig.LookupApp(normalizedType)
	if !exists {
		return nil, fmt.Errorf("provider %s not configured", normalizedType)
	}
	if providerCfg.Runtime == nil {
		return nil, fmt.Errorf("provider %s has no runtime configuration", normalizedType)
	}

	// A variant has no weights of its own: it loads its base's, while the
	// run keeps the variant's name, parameters and alias.
	weights := providerCfg.WeightsOf(modelName)
	if s.providers.appMgr != nil {
		weights = s.providers.appMgr.CanonicalModelName(weights)
	}
	var modelMetadata *metadata.ModelMetadata
	for _, model := range models {
		if model.Name == weights || utils.FormatModelName(model.Name) == weights {
			modelMetadata = model
			break
		}
	}
	if modelMetadata == nil {
		return nil, fmt.Errorf("model metadata not found for: %s", weights)
	}

	modelPath, err := s.resolveModelPath(ctx, modelMetadata, normalizedType)
	if err != nil {
		return nil, fmt.Errorf("model path not found: %w", err)
	}

	// Parameters and environment go over as the caller sent them, and
	// nothing else. LaunchInstance walks the tier tree itself and stores
	// what it was handed as the request tier — the tier a restart
	// replays. Resolving here too would hand it the whole merged set,
	// and every configured value would then be replayed as if the
	// caller had typed it, outranking the config it came from: a
	// restart could never pick up a parameter change.
	launchReq := prov_apps.LaunchRequest{
		Provider:   normalizedType,
		Model:      modelName,
		Endpoint:   prov_apps.Endpoint(endpoint),
		Port:       0,
		Parameters: parameters,
		EnvVars:    environment,
	}

	if normalizedType != "mlx" {
		launchReq.Files = []instance.FileMount{{Name: "model", NodePath: modelPath, ReadOnly: true}}
	}

	return &instanceBuildResult{
		LaunchReq:      launchReq,
		ProviderConfig: &providerCfg,
		ModelPath:      modelPath,
	}, nil
}

// launchOnDemandContainerWithParams launches an on-demand container with
// custom runtime parameters and environment variables.
func (s *Server) launchOnDemandContainerWithParams(ctx context.Context, modelName, providerType, endpoint string, parameters, environment map[string]string) (*instance.Instance, error) {
	normalizedType := utils.NormalizeAppType(providerType)
	utils.LogDebugf("Launching model %s with provider %s", modelName, normalizedType)

	// Build launch request with in-memory parameter resolution
	// Note: Parameters like "auto" are resolved in-memory during this step
	// We never save the resolved values - they're calculated fresh each launch
	result, err := s.buildInstanceConfig(ctx, modelName, providerType, endpoint, parameters, environment)
	if err != nil {
		slog.Error("Failed to build instance config", "error", err)
		return nil, err
	}

	// Only on-demand providers can be launched as instances
	if result.ProviderConfig.HasEndpoint() {
		slog.Error("launchOnDemandContainerWithParams called for non-launchable provider", "normalized_type", normalizedType, "mode", result.ProviderConfig.Mode)
		return nil, fmt.Errorf("%s providers like %s don't need instance launching - use the provider's API directly", result.ProviderConfig.Mode, normalizedType)
	}

	// The request tier only; the tree is resolved over it inside
	// LaunchInstance, which is where the final argv is decided.
	slog.Debug("Launching with caller-supplied config", "params", len(result.LaunchReq.Parameters), "env", len(result.LaunchReq.EnvVars))

	// Launch instance (async - health check happens in background)
	inst, err := s.providers.appMgr.LaunchInstance(ctx, result.LaunchReq)
	if err != nil {
		slog.Error("Failed to launch instance", "instance", err)
		return nil, fmt.Errorf("failed to launch instance: %w", err)
	}
	utils.LogDebugf("Instance launched successfully: ID=%s, Port=%d", inst.ID, inst.Port)

	// Don't wait for health here - the async goroutine in runs manager handles that
	// The instance will transition from StatusStarting -> StatusRunning when healthy
	return inst, nil
}

// resolveModelPath resolves the filesystem path for a model from its metadata.
// For MLX provider, returns the model name (HuggingFace repo name, not filesystem path).
// For shared storage, caches locally on first use and returns the cache path.
func (s *Server) resolveModelPath(ctx context.Context, model *metadata.ModelMetadata, providerType string) (string, error) {
	// MLX expects HuggingFace repo names, not filesystem paths
	if providerType == "mlx" {
		return model.Name, nil
	}

	// Shared storage: cache locally for fast access
	if s.config.Models.HasShared() {
		return s.resolveModelPathWithCache(ctx, model)
	}

	return model.FullPath, nil
}

// resolveModelPathWithCache handles shared storage mode:
// - If shared == cache (same directory), use directly (no copy needed)
// - If model exists in local cache, return cache path (fast)
// - If model only exists in shared storage, copy to cache first
// - Return cache path for local fast access
func (s *Server) resolveModelPathWithCache(ctx context.Context, model *metadata.ModelMetadata) (string, error) {
	sharedDir := s.config.Models.GetShared()

	// Get local cache directory (use GetModelsCacheDir for correct resolution)
	cacheDir, err := modelregistry.GetModelsCacheDir()
	if err != nil {
		return "", fmt.Errorf("failed to get cache directory: %w", err)
	}

	// Normalize paths for comparison (resolve symlinks, clean paths)
	sharedDirClean := filepath.Clean(sharedDir)
	cacheDirClean := filepath.Clean(cacheDir)

	// If shared and cache are the same directory, use directly (no copy needed)
	// This handles the coordinator case where local storage IS the shared storage
	if sharedDirClean == cacheDirClean {
		utils.LogDebugf("[Models] Shared and cache are same directory, using directly: %s", model.FullPath)
		return model.FullPath, nil
	}

	// Calculate relative path from shared storage
	// model.FullPath is absolute, we need to extract the relative portion
	var relativePath string
	if after, ok := strings.CutPrefix(model.FullPath, sharedDirClean); ok {
		relativePath = after
		relativePath = strings.TrimPrefix(relativePath, string(filepath.Separator))
	} else {
		// Model path doesn't start with shared dir - might be a local-only model
		// or the path format is different. Use the full path as-is.
		utils.LogDebugf("[Models] Model path '%s' not under shared dir '%s', using as-is", model.FullPath, sharedDir)
		return model.FullPath, nil
	}

	sharedPath := model.FullPath
	cachePath := filepath.Join(cacheDir, relativePath)

	// If paths resolve to the same location, use directly
	if filepath.Clean(sharedPath) == filepath.Clean(cachePath) {
		utils.LogDebugf("[Models] Shared and cache paths resolve to same location: %s", cachePath)
		return cachePath, nil
	}

	// Check if model already exists in cache
	if modelcache.FileExists(cachePath) {
		// Quick integrity check on cached model
		if modelcache.HasManifest(cachePath) {
			valid, err := s.model.Verifier.QuickVerify(context.WithoutCancel(ctx), cachePath) //nolint:contextcheck // Shared-cache verification must outlive a client disconnect.
			if err != nil || !valid {
				slog.Info("[Models] Cached model failed integrity check, will re-cache", "cache", cachePath)
				// Invalidate and continue to re-copy
				s.model.Verifier.InvalidateCache(cachePath)
			} else {
				utils.LogDebugf("[Models] Using verified cached model: %s", cachePath)
				return cachePath, nil
			}
		} else {
			// No manifest in cache - trust it for now but log warning
			utils.LogDebugf("[Models] Using cached model (no manifest): %s", cachePath)
			return cachePath, nil
		}
	}

	// Model not in cache - copy from shared storage with integrity verification
	slog.Info("[Models] Caching from shared storage...", "name", model.Name)

	// Check if shared storage has integrity manifest
	if modelcache.HasManifest(sharedPath) {
		// Use zero-trust copy: verify source → copy → verify destination
		result, err := s.model.Verifier.VerifyAndCopy(context.WithoutCancel(ctx), sharedPath, cachePath) //nolint:contextcheck // A client disconnect must not abort a shared-cache copy.
		if err != nil {
			slog.Info("[Models] Verified cache failed: , using shared storage directly", "failed", err)
			return sharedPath, nil
		}
		if !result.Valid {
			slog.Info("[Models] Destination verification failed after copy, using shared storage")
			return sharedPath, nil
		}
		slog.Info("[Models] Model cached with integrity verification: ( files verified)", "verification", cachePath, "files_checked", result.FilesChecked)
		return cachePath, nil
	}

	// No manifest in shared storage - use plain copy and create manifest
	if err := modelcache.CopyDir(sharedPath, cachePath); err != nil {
		slog.Info("[Models] Cache failed, using shared storage directly", "directly", err)
		return sharedPath, nil
	}

	// Create integrity manifest for future verifications
	manifest, err := modelcache.CreateManifest(context.WithoutCancel(ctx), cachePath, model.Name, model.DownloadedFrom) //nolint:contextcheck // The shared cache needs its manifest even after a client disconnect.
	if err != nil {
		slog.Warn("[Models] Warning: failed to create integrity manifest", "manifest", err)
	} else {
		if err := modelcache.WriteManifest(cachePath, manifest); err != nil {
			slog.Warn("[Models] Warning: failed to write integrity manifest", "manifest", err)
		}
	}

	slog.Info("[Models] Model cached successfully", "successfully", cachePath)
	return cachePath, nil
}
