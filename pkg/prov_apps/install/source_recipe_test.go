package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestShippedSourceReconcilesAndReceivesOnlyReleaseAuthority(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, templates.InstallDefaults(directory))
	path := filepath.Join(directory, "on-demand", "comfyui", "config.yaml")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var prior map[string]any
	require.NoError(t, yaml.Unmarshal(body, &prior))
	delete(prior, "install")
	prior["platforms"] = []map[string]any{{"os": "linux"}}
	prior["requirements"] = map[string]any{"gpu": map[string]any{"vendor": "nvidia", "policy": "required"}}
	prior["enabled"] = true
	prior["nodes"] = map[string]any{"worker": map[string]any{"parameters": map[string]any{"gpu-reserve-mib": "16384"}}}
	body, err = yaml.Marshal(prior)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, body, 0600))
	changed, err := templates.ReconcileManagedSpine(directory)
	require.NoError(t, err)
	require.Contains(t, changed, "on-demand/comfyui/config.yaml")
	apps, err := config.LoadAppsConfig(directory)
	require.NoError(t, err)
	sc, ok := apps.LookupApp("comfyui")
	require.True(t, ok)
	assert.True(t, sc.IsEnabled())
	assert.Equal(t, "16384", sc.Nodes["worker"].Parameters["gpu-reserve-mib"])
	assert.Equal(t, []config.InstallPlatform{{OS: "linux", Arch: "amd64"}}, sc.Platforms)
	require.NotNil(t, sc.Requirements)
	assert.Equal(t, "20GB", sc.Requirements.DiskSpace)
	assert.Equal(t, &config.PythonRequirement{Min: "3.12", Max: "3.13"}, sc.Requirements.Python)
	assert.Equal(t, &config.GPURequirement{Vendor: "nvidia", Policy: "required"}, sc.Requirements.GPU)
	require.Nil(t, sc.VersionSource)
	snapshot, err := ResolveRecipe(sc, "worker", "comfyui", "")
	require.NoError(t, err)
	assert.Equal(t, "release", snapshot.Authority)
	assert.Nil(t, snapshot.Policy)
	assert.Equal(t, "0.40.0", snapshot.PinnedVersion)
	assert.Equal(t, "source", snapshot.WorkingDir)
	assert.Equal(t, &config.InstallSource{Version: "0.40.0", URL: "https://github.com/Comfy-Org/ComfyUI/archive/refs/tags/v0.40.0.tar.gz", SHA256: "d82636cc721b8060099cf31b8d9ed40807c2147db438f88321a0396b7894e8e5", StripComponents: 1, Requirements: "requirements.txt", Entrypoint: "main.py"}, snapshot.Recipe.Source)
	require.NoError(t, snapshot.Recheck(t.Context()))
	changed, err = templates.ReconcileManagedSpine(directory)
	require.NoError(t, err)
	assert.Empty(t, changed, "reconciliation is idempotent")
	recipe := sc.Install.Runtimes["comfyui"]
	changedSource := *recipe.Source
	changedSource.SHA256 = strings.Repeat("a", 64)
	recipe.Source = &changedSource
	sc.Install.Runtimes["comfyui"] = recipe
	_, err = ResolveRecipe(sc, "worker", "comfyui", "")
	require.ErrorIs(t, err, ErrInstallPolicy, "modified source bytes must not acquire shipped authority")
}

func TestUnshippedSourceCannotAcquireInstallAuthority(t *testing.T) {
	// Keep all vLLM checks while replacing its pip mode, so the authority
	// refusal discriminates source ownership rather than a missing import.
	sc := shippedRecipeFixture(t, "vllm")
	r := sc.Install.Runtimes["vllm"]
	r.Package, r.StartupImport, r.VersionConstraint, r.Toolkit = nil, nil, nil, nil
	r.Source = &config.InstallSource{Version: "1.0.0", URL: "https://github.com/example/engine/archive/refs/tags/v1.0.0.tar.gz", SHA256: strings.Repeat("a", 64), StripComponents: 1, Requirements: "requirements.txt", Entrypoint: "main.py"}
	sc.VersionSource = nil
	sc.Runtime = &config.AppRuntimeConfig{DataDir: "data", Execution: config.ExecutionConfig{Type: "python", WorkingDir: "source", Args: []string{"main.py"}}}
	for _, name := range []string{"vllm", "unshipped-engine"} {
		t.Run(name, func(t *testing.T) {
			sc.Name = name
			sc.Install = &config.InstallConfig{Runtimes: map[string]config.InstallRecipe{name: r}}
			snapshot, err := ResolveRecipe(sc, "worker", name, "")
			require.ErrorIs(t, err, ErrInstallPolicy)
			assert.Contains(t, err.Error(), "source recipes require an unchanged shipped declaration")
			assert.Empty(t, snapshot.Authority)
			assert.Equal(t, r.Source.Version, snapshot.PinnedVersion)
		})
	}
	grant := RuntimePolicy{Packages: map[string]string{"torch": "", "torchvision": ""}, Entrypoints: []string{"main.py"}, Indexes: []string{"https://pypi.org/simple"}}
	require.ErrorContains(t, grant.Authorize(r), "release-owned", "operator grants cannot admit source recipes")
}

func TestSourceRuntimeChecksHaveNoPipEntrypointPackage(t *testing.T) {
	imports, checks := []string{"torch", "torchvision"}, []string{"pip_check", "imports", "cuda_companions"}
	r := config.InstallRecipe{Source: &config.InstallSource{}, Verify: &config.InstallVerify{Imports: &imports, Checks: &checks}, Companions: map[string]config.InstallCompanion{"torchvision": {}, "torch": {}}}
	snapshot := RecipeSnapshot{ResolvedInstall: config.ResolvedInstall{Recipe: r}}
	out := snapshot.RuntimeChecks(schema.RuntimeChecks{Packages: []string{"stale-package"}})
	assert.Equal(t, []string{"torch", "torchvision"}, out.Packages)
	assert.Equal(t, imports, out.Imports)
	assert.Equal(t, checks, out.Checks)
	out.Imports[0] = "changed"
	assert.Equal(t, "torch", imports[0])
}
