package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestAdditionalPackagesEnvironmentReportsPolicyDrift(t *testing.T) {
	for _, change := range []string{"unchanged", "add", "remove", "constraint", "unrelated", "legacy empty"} {
		t.Run(change, func(t *testing.T) {
			recipe := config.InstallRecipe{Source: &config.InstallSource{Version: "1"}}
			actual := map[string]string{"torchaudio": ">=2,<3"}
			selected := map[string]string{"torchaudio": ">=2,<3"}
			runtimes := map[string]any{"engine/engine": map[string]any{"additional_packages": selected}}
			path := filepath.Join(t.TempDir(), "policy.yaml")
			switch change {
			case "add":
				selected["new-engine"] = ""
			case "remove":
				delete(selected, "torchaudio")
			case "constraint":
				selected["torchaudio"] = "==2.5"
			case "unrelated":
				runtimes["other/other"] = map[string]any{"packages": map[string]string{"other": "==9"}}
			case "legacy empty":
				actual, selected = nil, map[string]string{}
				runtimes["engine/engine"] = map[string]any{"additional_packages": selected}
			}
			body, err := yaml.Marshal(map[string]any{"runtimes": runtimes})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, body, 0600))
			// The mapper consumes already authorized grants. Protected-file I/O is tested in install.
			body, err = os.ReadFile(path)
			require.NoError(t, err)
			var observed install.InstallPolicy
			require.NoError(t, yaml.Unmarshal(body, &observed))
			grant := observed.Runtimes["engine/engine"]
			result := providerEnvironment{Installed: true, DesiredInstall: &config.ResolvedInstall{Recipe: recipe}, InstallAuthority: &install.PolicyCapabilities{Grant: &grant}}
			result.recordInstall(&fsroot.InstallManifest{Recipe: &recipe, RecipeFingerprint: install.Fingerprint(recipe), AdditionalPackages: actual})
			want := change == "add" || change == "remove" || change == "constraint"
			require.Equal(t, want, result.RecipeDrift)
			wire, err := json.Marshal(result)
			require.NoError(t, err)
			require.Contains(t, string(wire), fmt.Sprintf(`"recipe_drift":%t`, want))
			// Older response consumers ignore the optional additions evidence.
			var old struct {
				RecipeDrift bool `json:"recipe_drift"`
			}
			require.NoError(t, json.Unmarshal(wire, &old))
			require.Equal(t, want, old.RecipeDrift)
		})
	}
}

func TestSourceInstallCannotSelectAdditionalPackages(t *testing.T) {
	e := &ProvidersExecutor{}
	worker := gin.New()
	worker.POST("/providers/:name/install", e.HandleInternalInstallProvider)
	response := httptest.NewRecorder()
	worker.ServeHTTP(response, httptest.NewRequest("POST", "/providers/comfyui/install", strings.NewReader(`{"additional_packages":{"unapproved":""}}`)))
	require.Equal(t, 400, response.Code)
	require.Contains(t, response.Body.String(), "additional_packages")
}

func TestInstalledSourcePolicyEvidenceNeverBecomesAnOverride(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
	store, err := config.NewAppsConfigStore(root)
	require.NoError(t, err)
	apps := store.Config()
	cfg, ok := apps.LookupApp("comfyui")
	require.True(t, ok)
	recipe := cfg.Install.Runtimes["comfyui"]
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	mgr, err := prov_apps.NewProviderAppManager(apps, prov_apps.WithRuntimeChecks(providerRuntimeChecks(store)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(t.Context()) })
	e := &ProvidersExecutor{mgr: mgr, nodeName: "worker", configStore: store, appsConfig: func() *config.AppsConfig { return apps }}
	manifest := &fsroot.InstallManifest{Version: recipe.Source.Version, Recipe: &recipe, RecipeFingerprint: install.Fingerprint(recipe), PolicyFingerprint: "historical-policy", WorkingDir: "source", PlanID: "completed", Interpreter: &fsroot.ManifestInterpreter{Path: "python", Version: "3.12.1"}, Inventory: []fsroot.ManifestPackage{{Name: "torch", Version: "2.5", Requested: true}}}
	snapshot, err := e.installedRecipeAuthority("comfyui", "comfyui", manifest)
	require.NoError(t, err, "source with no added roots retains release authority despite historical policy evidence")
	require.Equal(t, "release", snapshot.Authority)
	manifest.AdditionalPackages = map[string]string{"torchaudio": ">=2"}
	manifest.Inventory = append(manifest.Inventory, fsroot.ManifestPackage{Name: "torchaudio", Version: "2.5", Requested: true})
	_, err = e.installedRecipeAuthority("comfyui", "comfyui", manifest)
	require.ErrorIs(t, err, install.ErrInstallPolicy, "recorded roots require current protected authority")
	marker := filepath.Join(t.TempDir(), "import-started")
	python := fsroot.ProviderVenvPython("comfyui")
	require.NoError(t, os.MkdirAll(filepath.Dir(python), 0700))
	require.NoError(t, os.WriteFile(python, []byte("#!/bin/sh\ntouch "+fsroot.ShellQuote(marker)+"\n"), 0700))
	require.NoError(t, fsroot.WriteInstallManifest("comfyui", manifest))
	worker := gin.New()
	worker.POST("/providers/:name/install/verify", e.HandleInternalVerifyInstall)
	worker.GET("/providers/:name/environment", e.HandleInternalProviderEnvironment)
	verify := httptest.NewRecorder()
	worker.ServeHTTP(verify, httptest.NewRequest("POST", "/providers/comfyui/install/verify", nil))
	require.NotEqual(t, 200, verify.Code)
	require.Contains(t, verify.Body.String(), "protected install policy")
	response := httptest.NewRecorder()
	worker.ServeHTTP(response, httptest.NewRequest("GET", "/providers/comfyui/environment", nil))
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "installed_recipe_authority")
	require.NoFileExists(t, marker, "refused installed authority must not run the native interpreter")
}
