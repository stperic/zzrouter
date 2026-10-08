package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/observability/logger"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// ProvidersController handles the unified /providers API.
// Combines runtime status (from AppsService) with lifecycle operations
// (from ProvidersService) into a single resource hierarchy.
// ProviderRegistrar is the lifecycle contract used by controllers to
// transition providers between enabled/disabled at the end of an
// onboarding or teardown session. Implemented by *Server.
//
// Callers MUST surface errors to the HTTP response — a silent failure
// here means the on-disk install/uninstall succeeded but the enabled
// flag drifted, and the user sees a success response for a broken state.
type ProviderRegistrar interface {
	FinalizeOnboarding(name string) error
	FinalizeOffboarding(name string) error
}

type ProvidersController struct {
	appsService      *AppsService                  // runtime status: list, get, enable/disable (always available)
	versionsService  *ProviderVersionsService      // upstream release reporting (nil on workers: checks are coordinator-side)
	providersService *ProvidersService             // lifecycle: install, upgrade, uninstall (nil on workers)
	providerAppMgr   *prov_apps.ProviderAppManager // direct access for local-only queries (install progress)
	appsConfig       func() *pkgConfig.AppsConfig  // always returns current provider config (for catalog)
	configStore      *pkgConfig.AppsConfigStore    // mutation path for runtime provider instance creation
	registrar        ProviderRegistrar             // registers providers into runtime after verify
}

// NewProvidersController creates a unified provider controller.
// providersSvc may be nil when providerAppMgr is not available (e.g., worker nodes).
// configStore may be nil on workers (runtime instance creation is coordinator-only).
//
// registrar MUST be non-nil. Every session-terminal handler routes through
// it; a nil registrar would turn a wiring bug into "session succeeded,
// provider disabled, HTTP 200" — precisely the failure mode the choke-point
// exists to prevent. Panics at wire time rather than silently degrading.
func NewProvidersController(appsSvc *AppsService, providersSvc *ProvidersService, appsConfig func() *pkgConfig.AppsConfig, configStore *pkgConfig.AppsConfigStore, registrar ProviderRegistrar, mgr *prov_apps.ProviderAppManager, versionsSvc *ProviderVersionsService) *ProvidersController {
	if registrar == nil {
		panic("ProvidersController: registrar is required")
	}
	return &ProvidersController{
		appsService:      appsSvc,
		versionsService:  versionsSvc,
		providersService: providersSvc,
		providerAppMgr:   mgr,
		appsConfig:       appsConfig,
		configStore:      configStore,
		registrar:        registrar,
	}
}

// RegisterPublicRoutes registers the unified /providers route hierarchy.
func (ctrl *ProvidersController) RegisterPublicRoutes(router *gin.RouterGroup) {
	// Runtime status (always available)
	router.GET("/providers", ctrl.ListProviders)
	router.GET("/providers/catalog", ctrl.ListProviderCatalog)
	router.GET("/providers/:name", ctrl.GetProvider)

	// Upstream release reporting. Coordinator-only: the check is one
	// outbound call for the cluster, not one per worker.
	if ctrl.versionsService != nil {
		router.GET("/providers/:name/versions", ctrl.GetProviderVersions)
	}
	router.PATCH("/providers/:name", ctrl.UpdateProvider)
	router.POST("/providers/:name/verify", ctrl.VerifyProvider)

	// Runtime provider instance creation (coordinator-only; no-op on workers
	// where configStore is nil).
	if ctrl.configStore != nil {
		router.POST("/providers/instances", ctrl.AddProviderInstance)
	}

	// Lifecycle operations (only when providerAppMgr is available)
	if ctrl.providersService != nil {
		router.GET("/providers/:name/status", ctrl.GetProviderStatus)
		router.GET("/providers/:name/environment", ctrl.GetProviderEnvironment)
		router.POST("/providers/:name/install", ctrl.InstallProvider)
		router.GET("/providers/:name/install/status", ctrl.GetStepStatus)
		router.POST("/providers/:name/install/preflight", ctrl.PreflightInstall)
		router.POST("/providers/:name/install/plan", ctrl.GetInstallPlan)
		router.POST("/providers/:name/install/verify", ctrl.VerifyInstall)
		router.POST("/providers/:name/install/verify-step", ctrl.VerifyInstallStep)
		router.POST("/providers/:name/install/execute-step", ctrl.ExecuteInstallStep)
		router.DELETE("/providers/:name/install/disposable/:plan_id", ctrl.DeleteDisposable)
		router.POST("/providers/:name/upgrade", ctrl.UpgradeProvider)
		router.DELETE("/providers/:name", ctrl.UninstallProvider)
	}
}

// ============================================================================
// Runtime Status Handlers (delegated to AppsService)
// ============================================================================

// ListProviders handles GET /zzrouter/v1/providers
// Returns paginated list of configured providers with runtime status.
func (ctrl *ProvidersController) ListProviders(c *gin.Context) {
	req := &ListAppsRequest{
		Node:    QueryNode(c),
		App:     QueryProvider(c),
		Name:    c.Query("name"),
		Key:     c.Query("key"),
		Running: c.Query("running"),
		Format:  c.Query("format"),
		Refresh: c.Query("refresh") == "true",
	}

	pagination, ok := ParsePagination(c)
	if !ok {
		return
	}

	logger.Debug("[ProvidersController] List request", "node", req.Node, "provider", req.App)

	resp, err := ctrl.appsService.ListApps(c.Request.Context(), req)
	if err != nil {
		logger.Error("[ProvidersController] List failed", "error", err)
		InternalNodeError(c, "Failed to list providers")
		return
	}

	// Cache-only: a provider list must never wait on an upstream, so a cold
	// cache simply leaves the upstream fields empty.
	if ctrl.versionsService != nil {
		ctrl.versionsService.DecorateApps(resp.Data)
	}

	paginatedData, total, hasMore := ApplyPagination(resp.Data, pagination)
	respondList(c, paginatedData, total, hasMore)
}

// GetProvider handles GET /zzrouter/v1/providers/:name
// Returns full detail for a specific provider.
func (ctrl *ProvidersController) GetProvider(c *gin.Context) {
	req := &GetAppRequest{
		Name: c.Param("name"),
		Node: QueryNode(c),
	}

	logger.Debug("[ProvidersController] Get request", "name", req.Name, "node", req.Node)

	resp, err := ctrl.appsService.GetApp(c.Request.Context(), req)
	if err != nil {
		logger.Error("[ProvidersController] Get failed", "error", err)
		RespondToLookupError(c, err)
		return
	}

	if ctrl.versionsService != nil {
		one := []AppInfo{resp.App}
		ctrl.versionsService.DecorateApps(one)
		resp.App = one[0]
	}

	respondSuccess(c, "Provider retrieved", resp.App)
}

// GetProviderVersions handles GET /zzrouter/v1/providers/:name/versions
//
// Reports the newest release the provider's configured upstream publishes,
// alongside what each node has installed and what the config pin seeds new
// installs with. Pass refresh=true to bypass the cache.
//
// An unreachable upstream is not an HTTP error: the installed versions stay
// reportable and every status reads unknown, because a failed check that
// rendered as "up to date" is the one wrong answer here.
func (ctrl *ProvidersController) GetProviderVersions(c *gin.Context) {
	name := c.Param("name")

	installed, err := ctrl.installedVersions(c.Request.Context(), name)
	if err != nil {
		logger.Error("[ProvidersController] Versions: installed lookup failed", "name", name, "error", err)
		InternalNodeError(c, "Failed to collect installed provider versions")
		return
	}

	report, err := ctrl.versionsService.Report(c.Request.Context(), name, installed, c.Query("refresh") == "true")
	if err != nil {
		logger.Error("[ProvidersController] Versions failed", "name", name, "error", err)
		NotFound(c, err.Error())
		return
	}

	respondSuccess(c, "Provider versions retrieved", report)
}

// installedVersions collects node to installed-version for one provider from
// the same cluster view the list endpoint serves.
// It carries each node's OS alongside its version because the upstream
// ceiling can differ per platform: a macOS node installing through Homebrew
// cannot reach a release that Homebrew has not packaged yet.
func (ctrl *ProvidersController) installedVersions(ctx context.Context, name string) ([]nodeInstall, error) {
	resp, err := ctrl.appsService.ListApps(ctx, &ListAppsRequest{Name: name})
	if err != nil {
		return nil, err
	}
	installed := make([]nodeInstall, 0, len(resp.Data))
	for _, app := range resp.Data {
		if app.Name != name || app.Node == "" {
			continue
		}
		installed = append(installed, nodeInstall{Node: app.Node, Version: app.Version, OS: app.OS})
	}
	return installed, nil
}

// UpdateProvider handles PATCH /zzrouter/v1/providers/:name
// Updates provider configuration (e.g., {"enabled": true}).
func (ctrl *ProvidersController) UpdateProvider(c *gin.Context) {
	var req UpdateAppRequest
	if !BindJSONStrict(c, &req) {
		return
	}
	req.Name = c.Param("name")
	req.Node = QueryNode(c)

	// A body with nothing actionable in it used to answer 200 with a
	// zero-valued response: name "", enabled false, for a provider that
	// was neither. `{"pinned_version": null}` lands here, because null
	// into a *string is indistinguishable from absent — a caller
	// carrying over the merge-patch convention from
	// PATCH /providers/:name/parameters, where null deletes a key, got
	// silence dressed as success. Clearing the pin is "" (see
	// UpdateAppRequest).
	if req.Enabled == nil && req.PinnedVersion == nil {
		BadRequest(c, `no supported field to update: send "enabled", or "pinned_version" `+
			`(a version to pin, or "" to clear the pin and resolve the latest release)`)
		return
	}

	logger.Debug("[ProvidersController] Update request", "name", req.Name, "enabled", req.Enabled)

	resp, err := ctrl.appsService.UpdateApp(c.Request.Context(), &req)
	if err != nil {
		logger.Error("[ProvidersController] Update failed", "error", err)
		// RespondToError preserves *RoutedError status codes, so a 404
		// from the internal handler (unknown provider) surfaces as 404
		// to the client instead of being collapsed to a generic 500.
		RespondToError(c, err)
		return
	}

	respondSuccess(c, "Provider updated", resp)
}

// ListProviderCatalog handles GET /zzrouter/v1/providers/catalog
// Returns provider definitions from provider config (including not-yet-installed).
// Query params:
//   - type=cloud|local — filter by cloud or local (on-demand/external) providers
//   - mode=registry|all — by default, registries (search-only) are excluded;
//     pass mode=registry for only registries, or mode=all for everything
func (ctrl *ProvidersController) ListProviderCatalog(c *gin.Context) {
	cfg := ctrl.appsConfig()
	if cfg == nil {
		respondList(c, []any{}, 0, false)
		return
	}

	typeFilter := c.Query("type") // "cloud", "local", or "" (all)
	modeFilter := c.Query("mode") // legacy: "registry", "all", or "" (exclude registries)
	kindFilter := c.Query("kind") // canonical alias for mode — same values

	if !ValidateEnum(c, "type", typeFilter, ProviderCatalogTypes) {
		return
	}
	if !ValidateEnum(c, "mode", modeFilter, ProviderCatalogModes) {
		return
	}
	if !ValidateEnum(c, "kind", kindFilter, ProviderCatalogModes) {
		return
	}
	// kind takes precedence during the kind-split arc. When callers
	// send both, kind wins — gives new clients an opt-in migration
	// path without breaking old ones that still use mode.
	if kindFilter != "" {
		modeFilter = kindFilter
	}

	type catalogEntry struct {
		Key             string   `json:"key"`
		Name            string   `json:"name"`
		Description     string   `json:"description,omitempty"`
		Mode            string   `json:"mode"` // legacy; equal to Kind during arc
		Kind            string   `json:"kind"` // canonical new discriminator
		Protocol        string   `json:"protocol"`
		IsCloud         bool     `json:"is_cloud"`
		Installed       bool     `json:"installed"`
		Available       bool     `json:"available"`                   // Cloud: has credentials; Local: is enabled
		RequiredEnvVars []string `json:"required_env_vars,omitempty"` // Required env vars (endpoint + auth)
	}

	// Build set of installed provider names (from running apps)
	installedSet := make(map[string]bool)
	if ctrl.appsService != nil {
		resp, err := ctrl.appsService.ListApps(c.Request.Context(), &ListAppsRequest{})
		if err == nil {
			for _, app := range resp.Data {
				installedSet[app.Name] = true
			}
		}
	}

	var entries []catalogEntry
	cfg.RangeApps(func(key string, svc pkgConfig.ServiceConfig) bool {
		// Mode filter: exclude registries by default
		if modeFilter == constants.AppModeRegistry && svc.Mode != constants.AppModeRegistry {
			return true
		} else if modeFilter != "all" && modeFilter != constants.AppModeRegistry && svc.Mode == constants.AppModeRegistry {
			return true
		}

		isCloud := svc.IsCloudProvider()
		if typeFilter == constants.AppModeCloud && !isCloud {
			return true
		}
		if typeFilter == "local" && isCloud {
			return true
		}

		name := svc.Name
		if name == "" {
			name = key
		}

		// Determine availability and required env vars
		available := svc.IsEnabled()
		var requiredEnvVars []string
		if isCloud && svc.Runtime != nil {
			available = svc.IsCloudAvailable()
			// Order matters: endpoint first, then auth key — TUI prompts in this order
			if ev := extractEnvVar(svc.Runtime.Endpoint); ev != "" {
				requiredEnvVars = append(requiredEnvVars, ev)
			}
			// Extract env vars from API auth (token + custom headers)
			if svc.Runtime.API != nil {
				if ev := extractEnvVarFromAPI(svc.Runtime.API); ev != "" {
					requiredEnvVars = append(requiredEnvVars, ev)
				}
			}
		}

		// Source Kind from the typed provider view rather than from
		// svc.Mode directly. The legacy Mode distinguishes
		// service/external/on-demand/cloud/registry but the typed kind
		// collapses service+external into KindExternal, so deriving
		// from Mode here (as an earlier version did) produced
		// `kind: "service"` for ollama while hosts.go:buildLocalAppsInfo
		// emitted `kind: "external"` for the same provider. Same
		// binary, two answers — the client picks one at random. Route
		// through Find so both endpoints agree.
		// TODO(6): drop the `Mode` field from catalogEntry entirely.
		var kind string
		if p := cfg.Find(key); p != nil {
			kind = string(p.Kind())
		}
		entries = append(entries, catalogEntry{
			Key:             key,
			Name:            name,
			Description:     svc.Description,
			Mode:            svc.Mode,
			Kind:            kind,
			Protocol:        string(svc.Protocol),
			IsCloud:         isCloud,
			Installed:       installedSet[key],
			Available:       available,
			RequiredEnvVars: requiredEnvVars,
		})
		return true
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })

	respondList(c, entries, len(entries), false)
}

// VerifyProvider handles POST /zzrouter/v1/providers/:name/verify
// Tests connectivity to a provider by hitting its health check endpoint.
// Uses the provider's configured endpoint, auth, and health check path from provider config.
// Works for both cloud (API key auth) and local (no auth) providers.
func (ctrl *ProvidersController) VerifyProvider(c *gin.Context) {
	name := c.Param("name")
	cfg := ctrl.appsConfig()
	if cfg == nil {
		NotFound(c, "provider config not loaded")
		return
	}

	svcConfig, exists := cfg.LookupApp(name)
	if !exists {
		NotFound(c, fmt.Sprintf("provider %q not found", name))
		return
	}

	if svcConfig.Runtime == nil {
		BadRequest(c, fmt.Sprintf("provider %q has no runtime configuration", name))
		return
	}

	// Force-reload .env so newly saved keys/endpoints are picked up (client may have just written them)
	cm := pkgConfig.NewConfigManager("zzrouter")
	cm.ReloadConfigDirEnv()

	// Build the verify URL from endpoint + health check path
	endpoint := pkgConfig.NormalizeEndpoint(svcConfig.Runtime.Endpoint)
	if endpoint == "" {
		BadRequest(c, fmt.Sprintf("provider %q has no endpoint configured", name))
		return
	}

	checkPath := svcConfig.Runtime.HealthCheck.Path
	if checkPath == "" {
		checkPath = "/v1/models" // Default health check for OpenAI-compatible APIs
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()

	// Built from config, not Resolve: verification precedes enabling.
	target := &backend.Resolved{Endpoint: endpoint, Upstream: backend.ForProvider(svcConfig.API())}
	req, err := target.NewRequest(ctx, http.MethodGet, checkPath, nil)
	if err != nil {
		InternalNodeError(c, fmt.Sprintf("failed to create request: %v", err))
		return
	}

	// Execute request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		respondSuccess(c, "Provider verification failed", gin.H{
			"provider": name,
			"status":   "failed",
			"message":  fmt.Sprintf("connection failed: %v", err),
		})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Check response
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Terminal success of the cloud verify session — finalize the
		// provider. If this fails the config flag did not flip, so we
		// MUST report the drift rather than lying with a 200.
		if err := ctrl.registrar.FinalizeOnboarding(name); err != nil {
			respondFinalizeErr(c, "verify succeeded but finalize failed", err)
			return
		}
		respondSuccess(c, "Provider verified", gin.H{
			"provider": name,
			"status":   "ok",
			"message":  fmt.Sprintf("verified (HTTP %d)", resp.StatusCode),
		})
		return
	}

	// Read a small portion of the body for error hints
	bodyBuf := make([]byte, 512)
	n, _ := resp.Body.Read(bodyBuf)
	bodyPreview := string(bodyBuf[:n])

	message := fmt.Sprintf("HTTP %d", resp.StatusCode)
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		message = "authentication failed: check your API key"
	case resp.StatusCode == 429:
		message = "rate limited: try again later"
	case resp.StatusCode >= 500:
		message = fmt.Sprintf("server error (HTTP %d)", resp.StatusCode)
	}

	// Check body for additional hints
	lower := strings.ToLower(bodyPreview)
	if strings.Contains(lower, "invalid") && strings.Contains(lower, "key") {
		message = "invalid API key"
	} else if strings.Contains(lower, "quota") || strings.Contains(lower, "billing") {
		message = "account quota or billing issue"
	}

	respondSuccess(c, "Provider verification failed", gin.H{
		"provider": name,
		"status":   "failed",
		"message":  message,
	})
}
