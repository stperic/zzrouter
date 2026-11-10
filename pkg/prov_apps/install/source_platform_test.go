package install

import (
	"maps"
	"runtime"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestComfyUIPlatformRecipesPreserveInstalledAuthority(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	apps, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	sc, ok := apps.LookupApp("comfyui")
	require.True(t, ok)
	// Recorded from the shipped pre-MPS recipes; adding an OS must not stale existing installations.
	for goos, fingerprint := range map[string]string{
		"linux":   "c7f7697fdc6e91681497ae6477f0813f736551b8b93f3edbd2ea38376a02158a",
		"windows": "1791f291bd8a3554f5fcbb07f6fe49acfeb6827c7a0b23cde9b7a0b8b7872669",
	} {
		selected, err := sc.ResolveInstallForOS("worker", "comfyui", goos)
		require.NoError(t, err)
		require.Equal(t, fingerprint, RecipeFingerprint(selected.Recipe), goos)
	}
	darwin, err := sc.ResolveInstallForOS("worker", "comfyui", "darwin")
	require.NoError(t, err)
	require.Equal(t, "none", *darwin.Recipe.Companions["torch"].Build)
	require.Equal(t, "none", *darwin.Recipe.Companions["torchvision"].Build)
	require.Equal(t, []string{"pip_check", "imports", "mps_available"}, *darwin.Recipe.Verify.Checks)
	require.Empty(t, *darwin.Recipe.Indexes.Extra)

	snapshot, err := ResolveRecipe(sc, "worker", "comfyui", "", nil)
	require.NoError(t, err)
	require.Equal(t, "release", snapshot.Authority)
	selected, err := sc.ResolveInstallForOS("worker", "comfyui", runtime.GOOS)
	require.NoError(t, err)
	require.Equal(t, RecipeFingerprint(selected.Recipe), snapshot.Fingerprint)
	if runtime.GOOS == "darwin" {
		require.Contains(t, *snapshot.Recipe.Verify.Checks, "mps_available")
		require.NotContains(t, *snapshot.Recipe.Verify.Checks, "cuda_available")
	}
	for _, removed := range []string{"availability", "import"} {
		t.Run(removed, func(t *testing.T) {
			r := sc.Install.Runtimes["comfyui"]
			r.Platforms = maps.Clone(r.Platforms)
			platform := r.Platforms[runtime.GOOS]
			verify := *selected.Recipe.Verify
			if removed == "availability" {
				checks := slices.DeleteFunc(slices.Clone(*verify.Checks), func(check string) bool { return check == "mps_available" || check == "cuda_available" })
				verify.Checks = &checks
			} else {
				imports := slices.DeleteFunc(slices.Clone(*verify.Imports), func(module string) bool { return module == "aiohttp" })
				verify.Imports = &imports
			}
			platform.Verify = &verify
			r.Platforms[runtime.GOOS] = platform
			changed := sc
			changed.Install = &config.InstallConfig{Runtimes: map[string]config.InstallRecipe{"comfyui": r}}
			_, err := ResolveRecipe(changed, "worker", "comfyui", "", nil)
			require.ErrorContains(t, err, "cannot be removed")
		})
	}
	r := sc.Install.Runtimes["comfyui"]
	r.Platforms = maps.Clone(r.Platforms)
	otherOS := "darwin"
	if runtime.GOOS == otherOS {
		otherOS = "windows"
	}
	platform := r.Platforms[otherOS]
	platform.Companions = maps.Clone(platform.Companions)
	if platform.Companions == nil {
		platform.Companions = map[string]config.InstallCompanion{}
	}
	companion := platform.Companions["torch"]
	companion.Constraint = new(">=999")
	platform.Companions["torch"] = companion
	r.Platforms[otherOS] = platform
	sc.Install = &config.InstallConfig{Runtimes: map[string]config.InstallRecipe{"comfyui": r}}
	inactive, err := ResolveRecipe(sc, "worker", "comfyui", "", nil)
	require.NoError(t, err)
	require.Equal(t, snapshot.Fingerprint, inactive.Fingerprint)
	require.Equal(t, "release", inactive.Authority)
}

func TestReleasePolicyUsesEffectivePlatformRecipe(t *testing.T) {
	raw, err := templates.AppsFS.ReadFile(shippedOnDemandDir + "/mlx/config.yaml")
	require.NoError(t, err)
	var shipped config.OnDemandProvider
	require.NoError(t, yaml.Unmarshal(raw, &shipped))
	shipped.Platforms = []config.InstallPlatform{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}}
	r := shipped.Install.Runtimes["mlx"]
	imports := append(slices.Clone(*r.Verify.Imports), "torch")
	r.Platforms = map[string]config.InstallPlatformRecipe{"darwin": {
		Companions: map[string]config.InstallCompanion{"torch": {Build: new("none")}},
		Verify:     &config.InstallVerify{Imports: &imports},
	}}
	shipped.Install.Runtimes["mlx"] = r
	data, err := yaml.Marshal(shipped)
	require.NoError(t, err)
	source, err := templates.AppsFS.ReadFile(shippedOnDemandDir + "/comfyui/config.yaml")
	require.NoError(t, err)
	files := fstest.MapFS{
		shippedOnDemandDir + "/mlx/config.yaml":     &fstest.MapFile{Data: data},
		shippedOnDemandDir + "/comfyui/config.yaml": &fstest.MapFile{Data: source},
	}
	for _, goos := range []string{"darwin", "linux"} {
		arch := "amd64"
		if goos == "darwin" {
			arch = "arm64"
		}
		policy, err := releasePolicy(files, goos, arch)
		require.NoError(t, err)
		_, torch := policy.Runtimes["mlx/mlx"].Packages["torch"]
		require.Equal(t, goos == "darwin", torch)
		require.Equal(t, goos == "darwin", slices.Contains(policy.Runtimes["mlx/mlx"].Imports, "torch"))
		require.NotContains(t, policy.Runtimes, "comfyui/comfyui", "source recipes retain release authority without pip override grants")
	}
}
