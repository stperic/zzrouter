package install

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEveryShippedRuntimeRetainsReleaseAuthority(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	apps, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	entries, err := templates.AppsFS.ReadDir("files/providers/on-demand")
	require.NoError(t, err)
	managed := 0
	for _, entry := range entries {
		sc, ok := apps.LookupApp(entry.Name())
		require.True(t, ok)
		if sc.Install == nil {
			continue
		}
		managed += len(sc.Install.Runtimes)
		require.NoError(t, apps.UpdateApp(entry.Name(), func(current *config.ServiceConfig) error {
			current.Description = "unrelated persisted edit"
			return nil
		}))
	}
	require.Positive(t, managed)
	check := func(stage string, apps *config.AppsConfig) {
		t.Helper()
		for _, entry := range entries {
			sc, ok := apps.LookupApp(entry.Name())
			require.True(t, ok)
			if sc.Install == nil {
				continue
			}
			nodes := map[string]bool{"": true, "macbook-pro": true, "worker-1": true, "WINDOWS-WORKER": true}
			for node := range sc.Nodes {
				nodes[node] = true
			}
			for runtime, recipe := range sc.Install.Runtimes {
				for node := range nodes {
					t.Run(stage+"/"+runtime+"/"+node, func(t *testing.T) {
						for _, shape := range []string{"stored", "nil extra slice", "nil extra pointer"} {
							t.Run(shape, func(t *testing.T) {
								candidate := sc
								candidate.Install = &config.InstallConfig{Runtimes: maps.Clone(sc.Install.Runtimes)}
								copied := recipe
								indexes := *recipe.Indexes
								if shape != "stored" {
									if indexes.Extra != nil && len(*indexes.Extra) != 0 {
										return
									}
									indexes.Extra = nil
									if shape == "nil extra slice" {
										var empty []string
										indexes.Extra = &empty
									}
								}
								copied.Indexes = &indexes
								candidate.Install.Runtimes[runtime] = copied
								snapshot, err := ResolveRecipe(candidate, node, runtime, "")
								require.NoError(t, err)
								assert.Equal(t, "release", snapshot.Authority)
								assert.Nil(t, snapshot.Policy)
								assert.Empty(t, snapshot.PolicyFingerprint)
							})
						}
					})
				}
			}
		}
	}
	check("after update", apps)
	require.NoError(t, apps.SaveToDir(dir))
	_, err = templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	reloaded, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	check("after save reconcile reload", reloaded)
}

func TestRestatingShippedEmptyExtraRequiresProtectedPolicy(t *testing.T) {
	sc := shippedRecipeFixture(t, "mlx")
	override, err := config.MergeInstallPatch(nil, json.RawMessage(`{"runtimes":{"mlx":{"indexes":{"extra":[]}}}}`))
	require.NoError(t, err)
	sc.Defaults = &config.AppDefaultsConfig{Install: override}
	_, err = ResolveRecipe(sc, "worker", "mlx", "")
	assert.ErrorIs(t, err, ErrInstallPolicy, "empty normalization must not authorize explicit overrides")
}
