package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentWorkaroundReportsNodePatchAndActiveState(t *testing.T) {
	checks := schema.RuntimeChecks{Workaround: map[string]string{"SAMPLER": "0"}}
	for _, value := range []string{"0", "1", "unrecognized-secret"} {
		result := environmentWorkarounds("arbitrary", "worker-1", checks, map[string]string{"SAMPLER": value, "SECRET": "never-return"})
		require.Len(t, result, 1)
		assert.Equal(t, value == "0", result[0].Active)
		assert.Equal(t, "/zzrouter/v1/providers/arbitrary/parameters", result[0].Path)
		data, err := json.Marshal(result)
		require.NoError(t, err)
		assert.JSONEq(t, `{"nodes":{"worker-1":{"environment":{"SAMPLER":"0"}}}}`, string(mustMarshal(t, result[0].Body)))
		assert.NotContains(t, string(data), "never-return")
		assert.NotContains(t, string(data), "unrecognized-secret")
	}
}

func TestEnvironmentRoutesToWorkerAndResolvesItsNodeTier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix managed interpreter fixture")
	}
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	apps := store.Config()
	cfg, ok := apps.LookupApp("mlx")
	require.True(t, ok)
	cfg.Nodes = map[string]pkgConfig.NodeSpec{"worker-1": {Environment: map[string]string{"MLX_TEST_VALUE": "worker-tier"}}}
	require.NoError(t, apps.UpdateApp("mlx", func(sc *pkgConfig.ServiceConfig) error { *sc = cfg; return nil }))
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	python := fsroot.ProviderVenvPython("mlx-vlm")
	require.NoError(t, os.MkdirAll(filepath.Dir(python), 0o700))
	require.NoError(t, os.WriteFile(python, []byte("#!/bin/sh\nprintf 'ZZROUTER_RUNTIME_CHECKS={\"packages\":{\"observed\":{\"installed\":true,\"version\":\"%s\",\"reason\":\"\"}},\"devices\":[]}\\n' \"$MLX_TEST_VALUE\"\n"), 0o700))
	require.NoError(t, os.WriteFile(fsroot.ProviderVersionFile("mlx-vlm"), []byte("1.0.0"), 0o600))
	mgr, err := prov_apps.NewProviderAppManager(apps, prov_apps.WithNodename(func() string { return "worker-1" }), prov_apps.WithRuntimeChecks(providerRuntimeChecks(store)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(t.Context()) })
	executor := &ProvidersExecutor{mgr: mgr, nodeName: "worker-1", appsConfig: func() *pkgConfig.AppsConfig { return apps }, configStore: store}
	worker := gin.New()
	worker.GET("/zzrouter/v1/internal/providers/:name/environment", executor.HandleInternalProviderEnvironment)
	router := &featureResolvedRouter{worker: worker}
	service := NewProvidersService(router)
	result, err := service.GetProviderEnvironment(t.Context(), "mlx", "mlx-vlm", "worker-1")
	require.NoError(t, err)
	require.NotNil(t, router.request)
	assert.Equal(t, "worker-1", router.request.Node)
	assert.Equal(t, "/zzrouter/v1/internal/providers/mlx/environment?runtime=mlx-vlm", router.request.Path)
	assert.Equal(t, "worker-1", result.Node)
	assert.Equal(t, "mlx-vlm", result.Runtime)
	assert.True(t, result.Installed)
	require.NotNil(t, result.Environment)
	assert.Equal(t, "worker-tier", result.Environment.Packages["observed"].Version)

	// Unknown runtime strings stay query data and are rejected by the worker.
	_, err = service.GetProviderEnvironment(t.Context(), "mlx", "mlx-vlm&runtime=mlx", "worker-1")
	require.Error(t, err)
	assert.Contains(t, router.request.Path, "runtime=mlx-vlm%26runtime%3Dmlx")
	response := httptest.NewRecorder()
	worker.ServeHTTP(response, httptest.NewRequest("GET", router.request.Path, nil))
	assert.Equal(t, 400, response.Code)
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func TestEnvironmentCannotAcceptLegacyWorkerContract(t *testing.T) {
	for _, contract := range []string{"", "future_contract", environmentContract} {
		body := mustMarshal(t, providerEnvironment{Contract: contract, Node: "worker-1"})
		service := NewProvidersService(&mockRouter{response: &routing.Response{StatusCode: 200, Body: body}})
		result, err := service.GetProviderEnvironment(t.Context(), "arbitrary", "", "worker-1")
		if contract == environmentContract {
			require.NoError(t, err)
			assert.Equal(t, "worker-1", result.Node)
		} else {
			require.ErrorContains(t, err, "upgrade")
		}
	}
}

func TestEnvironmentRouteRequiresAdmin(t *testing.T) {
	server := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, ClusterMode: pkgConfig.ClusterModeCoordinator})
	for _, tc := range []struct {
		key    string
		status int
	}{{"", http.StatusUnauthorized}, {TestUserKey, http.StatusForbidden}} {
		resp := makeAuthRequest(t, server, "GET", "/zzrouter/v1/providers/vllm/environment", tc.key, nil)
		assert.Equal(t, tc.status, resp.Code, string(resp.Body))
	}
	resp := makeAuthRequest(t, server, "GET", apiDiscoveryPrefix, TestAdminKey, nil)
	require.Equal(t, 200, resp.Code)
	assert.Contains(t, string(resp.Body), "GET /zzrouter/v1/providers/{name}/environment")
	assert.Contains(t, string(resp.Body), "human remediation")
}

func TestInstalledVerifySurvivesDesiredConstraintDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix managed interpreter fixture")
	}
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	apps := store.Config()
	require.NoError(t, apps.UpdateApp("mlx", func(sc *pkgConfig.ServiceConfig) error {
		constraint := ">=99"
		sc.Defaults.Install = &pkgConfig.InstallConfig{Runtimes: map[string]pkgConfig.InstallRecipe{"mlx": {VersionConstraint: &constraint}}}
		return nil
	}))
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	python := fsroot.ProviderVenvPython("mlx")
	require.NoError(t, os.MkdirAll(filepath.Dir(python), 0700))
	// Synthetic output isolates HTTP desired-version admission from native runtime verification.
	require.NoError(t, os.WriteFile(python, []byte("#!/bin/sh\nprintf 'ZZROUTER_RUNTIME_CHECKS=[{\"name\":\"fixture\",\"passed\":true}]\\n'\n"), 0700))
	require.NoError(t, os.WriteFile(fsroot.ProviderVersionFile("mlx"), []byte("1.0"), 0600))
	mgr, err := prov_apps.NewProviderAppManager(apps, prov_apps.WithRuntimeChecks(providerRuntimeChecks(store)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(t.Context()) })
	executor := &ProvidersExecutor{mgr: mgr, nodeName: "worker", appsConfig: func() *pkgConfig.AppsConfig { return apps }, configStore: store}
	worker := gin.New()
	worker.POST("/zzrouter/v1/internal/providers/:name/install/verify", executor.HandleInternalVerifyInstall)
	response := httptest.NewRecorder()
	worker.ServeHTTP(response, httptest.NewRequest("POST", "/zzrouter/v1/internal/providers/mlx/install/verify", nil))
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"all_ok":true`)
	require.NoError(t, apps.UpdateApp("mlx", func(sc *pkgConfig.ServiceConfig) error {
		sc.Defaults = nil
		return nil
	}))
	response = httptest.NewRecorder()
	worker.ServeHTTP(response, httptest.NewRequest("POST", "/zzrouter/v1/internal/providers/mlx/install/verify", nil))
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"all_ok":true`)
}

func TestInstalledImportsRequireCurrentAuthority(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix managed interpreter fixture")
	}
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	apps := store.Config()
	cfg, ok := apps.LookupApp("mlx")
	require.True(t, ok)
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	python := fsroot.ProviderVenvPython("mlx")
	marker := filepath.Join(t.TempDir(), "probe-started")
	require.NoError(t, os.MkdirAll(filepath.Dir(python), 0700))
	require.NoError(t, os.WriteFile(python, []byte("#!/bin/sh\ntouch '"+marker+"'\nprintf 'ZZROUTER_RUNTIME_CHECKS=[]\\n'\n"), 0700))
	require.NoError(t, os.WriteFile(fsroot.ProviderVersionFile("mlx"), []byte("1.0"), 0600))
	mgr, err := prov_apps.NewProviderAppManager(apps, prov_apps.WithRuntimeChecks(providerRuntimeChecks(store)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(t.Context()) })
	executor := &ProvidersExecutor{mgr: mgr, nodeName: "worker", appsConfig: func() *pkgConfig.AppsConfig { return apps }, configStore: store}
	worker := gin.New()
	worker.POST("/zzrouter/v1/internal/providers/:name/install/verify", executor.HandleInternalVerifyInstall)
	worker.GET("/zzrouter/v1/internal/providers/:name/environment", executor.HandleInternalProviderEnvironment)
	for _, changed := range []bool{false, true} {
		recipe := cfg.Install.Runtimes["mlx"]
		if changed {
			imports := append(append([]string{}, *recipe.Verify.Imports...), "formerly_approved.module")
			recipe.Verify = &pkgConfig.InstallVerify{Imports: &imports, Checks: recipe.Verify.Checks}
		}
		manifest := fsroot.InstallManifest{Version: "1.0", Recipe: &recipe, PolicyFingerprint: "formerly-approved"}
		require.NoError(t, os.WriteFile(fsroot.ProviderManifestPath("mlx"), mustMarshal(t, manifest), 0600))
		response := httptest.NewRecorder()
		worker.ServeHTTP(response, httptest.NewRequest("POST", "/zzrouter/v1/internal/providers/mlx/install/verify", nil))
		assert.NotEqual(t, 200, response.Code)
		assert.Contains(t, response.Body.String(), "protected install policy")
		response = httptest.NewRecorder()
		worker.ServeHTTP(response, httptest.NewRequest("GET", "/zzrouter/v1/internal/providers/mlx/environment", nil))
		assert.Equal(t, 200, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "installed_recipe_authority")
		assert.NoFileExists(t, marker, "revoked policy must prevent native imports")
	}
}

func TestInstalledInventoryRespectsCurrentPackageGrants(t *testing.T) {
	name := "engine"
	snapshot := install.RecipeSnapshot{ResolvedInstall: pkgConfig.ResolvedInstall{Recipe: pkgConfig.InstallRecipe{Package: &name, Companions: map[string]pkgConfig.InstallCompanion{"companion": {}}}}, Policy: &install.RuntimePolicy{Packages: map[string]string{"engine": ">=1,<2", "companion": "==3.*"}}}
	manifest := &fsroot.InstallManifest{Inventory: []fsroot.ManifestPackage{{Name: "engine", Version: "1.5", Requested: true}, {Name: "companion", Version: "3.2", Requested: true}}}
	require.NoError(t, checkInstalledRootGrants(snapshot, manifest))
	manifest.Inventory[0].Version = "2.0"
	require.ErrorIs(t, checkInstalledRootGrants(snapshot, manifest), install.ErrInstallPolicy)
	manifest.Inventory[0].Version = "1.5"
	manifest.Inventory[1].Requested = false
	require.ErrorIs(t, checkInstalledRootGrants(snapshot, manifest), install.ErrInstallPolicy)
}
