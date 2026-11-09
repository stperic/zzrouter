package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallIndexesResolveWithinEachTier(t *testing.T) {
	recipe := recipeFixture()
	recipe.Indexes.Extra = ptr([]string{})
	recipe.Indexes.Platforms = map[string]InstallPlatformIndexes{"windows": {Extra: ptr([]string{"https://download.pytorch.org/whl/cu132"})}}
	cfg := ServiceConfig{Name: "engine", Install: &InstallConfig{Runtimes: map[string]InstallRecipe{"engine": recipe}}}
	windows, err := cfg.ResolveInstallForOS("worker", "engine", "windows")
	require.NoError(t, err)
	require.Equal(t, []string{"https://download.pytorch.org/whl/cu132"}, *windows.Recipe.Indexes.Extra)
	require.Nil(t, windows.Recipe.Indexes.Platforms)
	require.Equal(t, "release", windows.Provenance["indexes.extra"])
	for key := range windows.Provenance {
		require.False(t, strings.Contains(key, "platforms"), key)
	}
	linux, err := cfg.ResolveInstallForOS("worker", "engine", "linux")
	require.NoError(t, err)
	before := recipe
	indexes := *recipe.Indexes
	indexes.Platforms = nil
	before.Indexes = &indexes
	want, err := json.Marshal(before)
	require.NoError(t, err)
	actual, err := json.Marshal(linux.Recipe)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(actual), "Linux execution recipe remains identical")

	for _, body := range []string{`{"indexes":{"extra":[]}}`, `{"indexes":{"platforms":{"windows":{"extra":[]}}}}`} {
		t.Run(body, func(t *testing.T) {
			patch, err := MergeInstallPatch(nil, []byte(`{"runtimes":{"engine":`+body+`}}`))
			require.NoError(t, err)
			cfg.Nodes = map[string]NodeSpec{"worker": {Install: patch}}
			selected, err := cfg.ResolveInstallForOS("worker", "engine", "windows")
			require.NoError(t, err)
			require.Empty(t, *selected.Recipe.Indexes.Extra)
			require.Equal(t, "nodes.worker", selected.Provenance["indexes.extra"])
			require.True(t, selected.Overridden)
			selected, err = cfg.ResolveInstallForOS("worker", "engine", "linux")
			require.NoError(t, err)
			require.Equal(t, !strings.Contains(body, "platforms"), selected.Overridden)
			raw, err := cfg.ResolveInstall("worker", "engine")
			require.NoError(t, err)
			require.NotEmpty(t, raw.Recipe.Indexes.Platforms, "configuration views retain declarations")
		})
	}
	_, err = cfg.ResolveInstallForOS("worker", "engine", "macos")
	require.ErrorContains(t, err, "unknown platform")
}

func TestInstallIndexesValidateInactiveBranches(t *testing.T) {
	for _, platform := range []map[string]InstallPlatformIndexes{
		{"macos": {Extra: ptr([]string{})}},
		{"windows": {Extra: ptr([]string{"http://unsafe.test"})}},
		{"windows": {Primary: ptr("")}},
		{"windows": {Extra: ptr([]string{"https://a.test", "https://b.test", "https://c.test", "https://d.test", "https://e.test"})}},
	} {
		recipe := recipeFixture()
		recipe.Indexes.Platforms = platform
		cfg := ServiceConfig{Name: "engine", Install: &InstallConfig{Runtimes: map[string]InstallRecipe{"engine": recipe}}}
		require.Error(t, cfg.ValidateInstall())
	}
}
