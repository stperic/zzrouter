package prov_apps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestFeatureRuntimeUpgradeTargetSkipsSeed(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	dir := config.NewPathResolver("zzrouter").GetAppsConfigDir()
	require.NoError(t, templates.InstallDefaults(dir))
	apps, err := install.LoadAppsConfig()
	require.NoError(t, err)
	sc, ok := apps.LookupApp("mlx")
	require.True(t, ok)
	require.NotEmpty(t, sc.PinnedVersion)
	snapshot, err := install.ResolveRecipe(sc, "worker", "mlx-vlm", "", nil)
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.PinnedVersion)
	coord := &InstallCoordinator{appsConfig: func() *config.AppsConfig { return apps }}
	require.Empty(t, coord.resolveUpgradeTarget(t.Context(), "mlx-vlm", ""), "the recipe installer must resolve latest, past the feature seed")
	require.Equal(t, "0.7.8", coord.resolveUpgradeTarget(t.Context(), "mlx-vlm", "0.7.8"))
}

func TestRuntimeSeedReconcileKeepsNewerInstall(t *testing.T) {
	isolateProviderRoot(t)
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	file := filepath.Join(dir, "on-demand", "mlx", "config.yaml")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(data, &cfg))
	recipe := cfg["install"].(map[string]any)["runtimes"].(map[string]any)["mlx-vlm"].(map[string]any)
	delete(recipe, "pinned_version")
	data, err = yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, data, 0600))
	installed := fsroot.ProviderDir("mlx-vlm")
	require.NoError(t, os.MkdirAll(installed, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(installed, "version"), []byte("99.0.0"), 0600))
	changed, err := templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	require.Contains(t, changed, "on-demand/mlx/config.yaml")
	apps, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	sc, ok := apps.LookupApp("mlx")
	require.True(t, ok)
	snapshot, err := install.ResolveRecipe(sc, "worker", "mlx-vlm", "", nil)
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.PinnedVersion)
	require.Equal(t, "99.0.0", fsroot.ReadInstalledVersion("mlx-vlm"))
	coord, fake, _, cleanup := newAsyncTestCoord(t, "mlx-vlm")
	defer cleanup()
	fake.installed.Store(true)
	plan := &install.Plan{Provider: "mlx-vlm", Action: "install", Version: snapshot.PinnedVersion, Recipe: &snapshot}
	id, err := coord.ExecuteResolvedAsync(t.Context(), "mlx", plan, install.PlanOptions{Runtime: "mlx-vlm"}, -1, nil)
	require.ErrorIs(t, err, ErrProviderAlreadyInstalled)
	require.Empty(t, id)
	require.Equal(t, "99.0.0", fsroot.ReadInstalledVersion("mlx-vlm"))
}
