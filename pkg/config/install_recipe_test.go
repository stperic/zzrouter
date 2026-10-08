package config

import (
	"encoding/json"
	"testing"

	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func recipeFixture() InstallRecipe {
	return InstallRecipe{Package: ptr("engine"), VersionConstraint: ptr(""), Indexes: &InstallIndexes{Primary: ptr("https://pypi.org/simple"), Extra: ptr([]string{"https://wheels.example.org/simple"})}, Companions: map[string]InstallCompanion{"tensor": {Constraint: ptr(">=2"), Build: ptr("none")}}, Verify: &InstallVerify{Imports: ptr([]string{"engine.server"}), Checks: ptr([]string{"pip_check", "imports"})}, StartupImport: ptr("engine.server"), OnlyBinary: ptr(true), Timeout: ptr("30m"), Toolkit: ptr("")}
}

func TestInstallRecipeOverrideRoundTrip(t *testing.T) {
	sc := ServiceConfig{Name: "engine", Mode: "on-demand", Install: &InstallConfig{Runtimes: map[string]InstallRecipe{"engine": recipeFixture()}}, Defaults: &AppDefaultsConfig{}, Nodes: map[string]NodeSpec{}}
	defaults, err := MergeInstallPatch(nil, json.RawMessage(`{"runtimes":{"engine":{"version_constraint":">=3","indexes":{"extra":[]}}}}`))
	require.NoError(t, err)
	sc.Defaults.Install = defaults
	local, err := MergeInstallPatch(nil, json.RawMessage(`{"runtimes":{"engine":{"version_constraint":"","only_binary":false}}}`))
	require.NoError(t, err)
	sc.Nodes["worker"] = NodeSpec{Install: local}
	p, err := serviceConfigToProvider(sc.Name, sc)
	require.NoError(t, err)
	data, err := yaml.Marshal(p)
	require.NoError(t, err)
	var decoded OnDemandProvider
	require.NoError(t, yaml.Unmarshal(data, &decoded))
	after := providerToServiceConfig(&decoded)
	resolved, err := after.ResolveInstall("worker", "engine")
	require.NoError(t, err)
	assert.Equal(t, "", *resolved.Recipe.VersionConstraint)
	assert.Empty(t, *resolved.Recipe.Indexes.Extra)
	assert.False(t, *resolved.Recipe.OnlyBinary)
	assert.Equal(t, "nodes.worker", resolved.Provenance["version_constraint"])
	assert.Equal(t, "defaults", resolved.Provenance["indexes.extra"])
	assert.True(t, resolved.Overridden)
	deleted, err := MergeInstallPatch(after.Nodes["worker"].Install, json.RawMessage(`{"runtimes":{"engine":{"version_constraint":null,"only_binary":null}}}`))
	require.NoError(t, err)
	after.Nodes["worker"] = NodeSpec{Install: deleted}
	inherited, err := after.ResolveInstall("worker", "engine")
	require.NoError(t, err)
	assert.Equal(t, ">=3", *inherited.Recipe.VersionConstraint)
	assert.True(t, *inherited.Recipe.OnlyBinary)
	// Resolution must not alias the persisted pointer graph.
	*inherited.Recipe.Package = "changed"
	assert.Equal(t, "engine", *after.Install.Runtimes["engine"].Package)
	remote, err := after.ResolveInstall("other", "engine")
	require.NoError(t, err)
	assert.Equal(t, ">=3", *remote.Recipe.VersionConstraint)
}

func TestInstallSpecifierSyntax(t *testing.T) {
	for _, value := range []string{"", "~=1.4.5rc1", ">=1!2.0,!=1!2.1.*", "==2.10+cu130", "==1.0+abc_01", " >= v1.0RC1 "} {
		assert.NoError(t, ValidateSpecifier(value), value)
	}
	for _, value := range []string{"engine @ https://evil.test/w.whl", ">=1;python_version>'3'", "~=1", "~=1.post1", ">1.0+cu130", "==1.0+cu130.*", "==1.0rc1.*", "==", "=1.0", ">=1,,<3"} {
		assert.Error(t, ValidateSpecifier(value), value)
	}
	for _, value := range []string{"http://pypi.org/simple", "https://user:pass@pypi.org/simple", "https://pypi.org:8443/simple", "https://pypi.org/simple#x", "https://pypi.org/simple?x=y", "https://pypi.org%2f.evil/simple"} {
		assert.Error(t, ValidateInstallIndex(value), value)
	}
}

func TestInstallURLPathsCannotExpandShellCommands(t *testing.T) {
	for _, path := range []string{"", "/simple", "/nested/wheels-1.0_foo+bar~baz"} {
		for _, host := range []string{"pypi.org", "wheels.example.org:443", "[2001:db8::1]"} {
			value := "https://" + host + path
			t.Run(value, func(t *testing.T) {
				require.NoError(t, ValidateInstallIndex(value))
				require.NoError(t, ValidateInstallArtifactURL(value))
			})
		}
	}
	for _, path := range []string{`/simple"&echo`, "/simple&echo", "/%NAME%", "/%2B", "/%22", "/%26", "/%25", "/%2522", "/%", "/%GG", "/space here", "/%20", "/%0a", "/tab\there", "/bang!", "/caret^", "/pipe|", "/colon:"} {
		t.Run(path, func(t *testing.T) {
			value := "https://pypi.org" + path
			require.Error(t, ValidateInstallIndex(value))
			if path != "/%2B" {
				require.Error(t, ValidateInstallArtifactURL(value))
			}
		})
	}
	for _, value := range []string{"https://pypi.org/simple?", "https://pypi.org/simple#", "https://pypi.org/simple?x=y", "https://pypi.org/simple#x", "https:opaque", "https://user@pypi.org/simple", "https://pypi.org:8443/simple", "http://pypi.org/simple", "https://bad&host/simple", "https://[fe80::1%25en0]/simple", "https://[pypi.org]/simple", "https://pypi.org:/simple"} {
		t.Run(value, func(t *testing.T) {
			require.Error(t, ValidateInstallIndex(value))
			require.Error(t, ValidateInstallArtifactURL(value))
		})
	}
}

func ptr[T any](value T) *T { return &value }

func TestInstallOverridesSurviveUpdateSaveReconcileReload(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	apps, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	for _, name := range []string{"vllm", "mlx"} {
		require.NoError(t, apps.UpdateApp(name, func(sc *ServiceConfig) error {
			override, err := MergeInstallPatch(nil, json.RawMessage(`{"runtimes":{"`+name+`":{"indexes":{"extra":[]},"only_binary":false}}}`))
			if err != nil {
				return err
			}
			sc.Nodes = map[string]NodeSpec{"worker": {Install: override}}
			return nil
		}))
		require.NoError(t, apps.UpdateApp(name, func(sc *ServiceConfig) error { sc.Description = "test round trip"; return nil }))
	}
	require.NoError(t, apps.SaveToDir(dir))
	_, err = templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	fresh, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	for _, name := range []string{"vllm", "mlx"} {
		sc, ok := fresh.LookupApp(name)
		require.True(t, ok)
		resolved, err := sc.ResolveInstall("worker", name)
		require.NoError(t, err)
		require.NotNil(t, resolved.Recipe.Indexes.Extra)
		assert.Empty(t, *resolved.Recipe.Indexes.Extra)
		assert.False(t, *resolved.Recipe.OnlyBinary)
		assert.True(t, resolved.Overridden)
		plain, err := sc.ResolveInstall("other", name)
		require.NoError(t, err)
		assert.False(t, plain.Overridden)
	}
}
