package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func platformRecipeFixture() ServiceConfig {
	recipe := recipeFixture()
	recipe.Platforms = map[string]InstallPlatformRecipe{
		"darwin": {
			Companions: map[string]InstallCompanion{"tensor": {Build: ptr("none"), Constraint: ptr(">=3")}},
			Verify:     &InstallVerify{Checks: ptr([]string{"pip_check", "imports", "mps_available"})},
		},
	}
	return ServiceConfig{Name: "engine", Install: &InstallConfig{Runtimes: map[string]InstallRecipe{"engine": recipe}}}
}

func TestRecipePlatformSelectionPrecedesTierMerge(t *testing.T) {
	for _, body := range []string{
		`{"companions":{"tensor":{"constraint":">=4"}},"verify":{"checks":["pip_check","imports"]}}`,
		`{"platforms":{"darwin":{"companions":{"tensor":{"constraint":">=4"}},"verify":{"checks":["pip_check","imports"]}}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			cfg := platformRecipeFixture()
			override, err := MergeInstallPatch(nil, []byte(`{"runtimes":{"engine":`+body+`}}`))
			require.NoError(t, err)
			cfg.Nodes = map[string]NodeSpec{"worker": {Install: override}}
			before, err := json.Marshal(cfg)
			require.NoError(t, err)
			selected, err := cfg.ResolveInstallForOS("worker", "engine", "darwin")
			require.NoError(t, err)
			require.Equal(t, ">=4", *selected.Recipe.Companions["tensor"].Constraint)
			require.Equal(t, "none", *selected.Recipe.Companions["tensor"].Build)
			require.Equal(t, []string{"engine.server"}, *selected.Recipe.Verify.Imports)
			require.Equal(t, []string{"pip_check", "imports"}, *selected.Recipe.Verify.Checks)
			require.Equal(t, "nodes.worker", selected.Provenance["verify.checks"])
			require.True(t, selected.Overridden)
			require.Nil(t, selected.Recipe.Platforms)
			*selected.Recipe.Companions["tensor"].Build = "changed"
			*selected.Recipe.Verify.Imports = []string{"changed"}
			after, err := json.Marshal(cfg)
			require.NoError(t, err)
			require.JSONEq(t, string(before), string(after), "selection and returned values cannot mutate configuration")
			raw, err := cfg.ResolveInstall("worker", "engine")
			require.NoError(t, err)
			require.Contains(t, raw.Recipe.Platforms, "darwin")
		})
	}
}

func TestInactiveRecipePlatformDoesNotOverride(t *testing.T) {
	cfg := platformRecipeFixture()
	override, err := MergeInstallPatch(nil, []byte(`{"runtimes":{"engine":{"platforms":{"darwin":{"verify":{"checks":["pip_check","imports"]}}}}}}`))
	require.NoError(t, err)
	cfg.Nodes = map[string]NodeSpec{"worker": {Install: override}}
	selected, err := cfg.ResolveInstallForOS("worker", "engine", "linux")
	require.NoError(t, err)
	require.False(t, selected.Overridden)
	require.Equal(t, ">=2", *selected.Recipe.Companions["tensor"].Constraint)
	require.Equal(t, []string{"pip_check", "imports"}, *selected.Recipe.Verify.Checks)
	require.Nil(t, selected.Recipe.Platforms)
}

func TestRecipePlatformValidatesShadowedDeclarations(t *testing.T) {
	for _, body := range []string{
		`{"platforms":{"macos":{"verify":{"checks":["pip_check","imports"]}}}}`,
		`{"platforms":{"darwin":{"companions":{"tensor":{"build":"mps"}}}}}`,
		`{"platforms":{"darwin":{"companions":{"tensor":{"constraint":"--unsafe"}}}}}`,
		`{"platforms":{"darwin":{"verify":{"checks":["execute_code"]}}}}`,
		`{"platforms":{"darwin":{"verify":{"imports":["not a module"]}}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			cfg := platformRecipeFixture()
			bad, err := MergeInstallPatch(nil, []byte(`{"runtimes":{"engine":`+body+`}}`))
			require.NoError(t, err)
			good, err := MergeInstallPatch(nil, []byte(`{"runtimes":{"engine":{"platforms":{"darwin":{"companions":{"tensor":{"build":"none","constraint":">=5"}},"verify":{"imports":["engine.server"],"checks":["pip_check","imports"]}}}}}}`))
			require.NoError(t, err)
			cfg.Defaults = &AppDefaultsConfig{Install: bad}
			cfg.Nodes = map[string]NodeSpec{"worker": {Install: good}}
			_, err = cfg.ResolveInstallForOS("worker", "engine", "linux")
			require.Error(t, err, "invalid inactive values cannot hide behind a later tier")
			require.Error(t, cfg.ValidateInstall())
		})
	}
}

func TestRecipePlatformExplicitEmptyVerifyFails(t *testing.T) {
	for _, field := range []string{"imports", "checks"} {
		cfg := platformRecipeFixture()
		override, err := MergeInstallPatch(nil, []byte(`{"runtimes":{"engine":{"platforms":{"darwin":{"verify":{"`+field+`":[]}}}}}}`))
		require.NoError(t, err)
		cfg.Nodes = map[string]NodeSpec{"worker": {Install: override}}
		_, err = cfg.ResolveInstallForOS("worker", "engine", "darwin")
		require.Error(t, err, "empty arrays replace rather than inherit")
		_, err = cfg.ResolveInstallForOS("worker", "engine", "linux")
		require.NoError(t, err)
		require.Error(t, cfg.ValidateInstall(), "inactive effective branches must also be valid")
	}
}

func TestRecipePlatformRejectsExecutionFields(t *testing.T) {
	for _, field := range []string{"source", "package", "platforms", "command", "indexes"} {
		_, err := MergeInstallPatch(nil, []byte(`{"runtimes":{"engine":{"platforms":{"darwin":{"`+field+`":{}}}}}}`))
		require.Error(t, err, field)
	}
}

func TestPlatformDeclarationsSurviveProviderSaveReload(t *testing.T) {
	cfg := platformRecipeFixture()
	cfg.Mode, cfg.RunScope = "on-demand", RunScopeNode
	cfg.Requirements = &AppRequirements{GPU: &GPURequirement{Vendor: "nvidia", Policy: "required", Platforms: map[string]string{"darwin": "apple"}}}
	cfg.Runtime = &AppRuntimeConfig{BasePort: 8188, PortRange: []int{8188, 8188}, Execution: ExecutionConfig{Type: "python", Command: "python", Args: []string{"-m", "engine.server"}}}
	apps := &AppsConfig{}
	require.NoError(t, apps.AddApp("engine", cfg))
	dir := t.TempDir()
	require.NoError(t, apps.SaveToDir(dir))
	loaded, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	after, ok := loaded.LookupApp("engine")
	require.True(t, ok)
	require.Equal(t, cfg.Requirements, after.Requirements)
	require.Equal(t, cfg.Install, after.Install)
	selected, err := after.ResolveInstallForOS("worker", "engine", "darwin")
	require.NoError(t, err)
	require.Contains(t, *selected.Recipe.Verify.Checks, "mps_available")
}
