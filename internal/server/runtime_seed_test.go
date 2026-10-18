package server

import (
	"encoding/json"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
)

func TestRuntimeSeedEnvironmentReportsDrift(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	apps, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	sc, ok := apps.LookupApp("mlx")
	require.True(t, ok)
	desired, err := sc.ResolveInstall("worker", "mlx-vlm")
	require.NoError(t, err)
	require.NotNil(t, desired.Recipe.PinnedVersion)
	result := providerEnvironment{Installed: true, DesiredInstall: &desired}
	previous := desired.Recipe
	previous.PinnedVersion = nil
	snapshot, err := install.ResolveRecipe(sc, "worker", "mlx-vlm", "", nil)
	require.NoError(t, err)
	require.Equal(t, install.Fingerprint(previous), snapshot.Fingerprint, "the install records the recipe without its seed")
	manifest := &fsroot.InstallManifest{Version: "0.7.8", Recipe: &previous, RecipeFingerprint: install.Fingerprint(previous)}
	result.recordInstall(manifest)
	require.False(t, result.RecipeDrift, "adding a seed to an existing recipe is not drift")
	previous = desired.Recipe
	manifest.RecipeFingerprint = snapshot.Fingerprint
	seed := "0.7.7"
	desired.Recipe.PinnedVersion = &seed
	result.recordInstall(manifest)
	require.False(t, result.RecipeDrift, "a newer installed runtime has no drift after a seed change")
	require.Equal(t, "0.7.8", manifest.Version)
	data, err := json.Marshal(result)
	require.NoError(t, err)
	var report map[string]any
	require.NoError(t, json.Unmarshal(data, &report))
	require.Equal(t, seed, report["desired_install"].(map[string]any)["recipe"].(map[string]any)["pinned_version"])
	timeout := "40m"
	desired.Recipe.Timeout = &timeout
	result.recordInstall(manifest)
	require.True(t, result.RecipeDrift, "a change to the recipe itself still reports drift")
}
