package config

import (
	"path/filepath"
	"strings"
	"testing"

	configschema "github.com/stperic/zzrouter/pkg/config/schema"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func sourceRecipeFixture() InstallRecipe {
	r := recipeFixture()
	r.Package, r.StartupImport, r.VersionConstraint, r.Toolkit = nil, nil, nil, nil
	r.Source = &InstallSource{Version: "1.0.0", URL: "https://github.com/example/engine/archive/refs/tags/v1.0.0.tar.gz", SHA256: strings.Repeat("a", 64), StripComponents: 1, Requirements: "requirements.txt", Entrypoint: "main.py"}
	return r
}

func sourceServiceFixture() ServiceConfig {
	return ServiceConfig{
		Name: "engine", Mode: "on-demand", RunScope: RunScopeNode,
		Install:  &InstallConfig{Runtimes: map[string]InstallRecipe{"engine": sourceRecipeFixture()}},
		Defaults: &AppDefaultsConfig{}, Nodes: map[string]NodeSpec{},
		Runtime: &AppRuntimeConfig{DataDir: "data", PortRange: []int{8188, 8188}, BasePort: 8188,
			Execution: ExecutionConfig{Type: "python", Command: "python", WorkingDir: "source", Args: []string{"main.py", "--base-directory", PlaceholderDataDir}}},
	}
}

func TestSourceRecipeValidation(t *testing.T) {
	require.NoError(t, sourceRecipeFixture().Validate())
	for name, mutate := range map[string]func(*InstallRecipe){
		"missing version":     func(r *InstallRecipe) { r.Source.Version = "" },
		"unsafe version":      func(r *InstallRecipe) { r.Source.Version = "../1.0" },
		"long version":        func(r *InstallRecipe) { r.Source.Version = strings.Repeat("v", 129) },
		"missing URL":         func(r *InstallRecipe) { r.Source.URL = "" },
		"HTTP":                func(r *InstallRecipe) { r.Source.URL = "http://example.org/release.tar.gz" },
		"credentials":         func(r *InstallRecipe) { r.Source.URL = "https://user:pass@example.org/release.tar.gz" },
		"query":               func(r *InstallRecipe) { r.Source.URL += "?token=secret" },
		"shell URL":           func(r *InstallRecipe) { r.Source.URL = "https://example.org/$(id).tar.gz" },
		"unsupported archive": func(r *InstallRecipe) { r.Source.URL = "https://example.org/release.zip" },
		"missing hash":        func(r *InstallRecipe) { r.Source.SHA256 = "" },
		"short hash":          func(r *InstallRecipe) { r.Source.SHA256 = strings.Repeat("a", 63) },
		"nonhex hash":         func(r *InstallRecipe) { r.Source.SHA256 = strings.Repeat("g", 64) },
		"uppercase hash":      func(r *InstallRecipe) { r.Source.SHA256 = strings.Repeat("A", 64) },
		"no stripping":        func(r *InstallRecipe) { r.Source.StripComponents = 0 },
		"excess stripping":    func(r *InstallRecipe) { r.Source.StripComponents = 2 },
		"pip package":         func(r *InstallRecipe) { r.Package = ptr("engine") },
		"startup import":      func(r *InstallRecipe) { r.StartupImport = ptr("engine.server") },
		"version constraint":  func(r *InstallRecipe) { r.VersionConstraint = ptr("") },
		"toolkit":             func(r *InstallRecipe) { r.Toolkit = ptr("") },
		"source builds":       func(r *InstallRecipe) { r.OnlyBinary = ptr(false) },
		"omitted binary":      func(r *InstallRecipe) { r.OnlyBinary = nil },
		"empty imports":       func(r *InstallRecipe) { r.Verify.Imports = ptr([]string{}) },
		"missing pip check":   func(r *InstallRecipe) { r.Verify.Checks = ptr([]string{"imports"}) },
		"missing CUDA check": func(r *InstallRecipe) {
			r.Companions["tensor"] = InstallCompanion{Build: ptr("cuda")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := sourceRecipeFixture()
			mutate(&r)
			require.Error(t, r.Validate())
		})
	}
	for _, path := range []string{"", ".", "..", "../file", "/file", "C:/file", `dir\file`, "dir//file", "dir/../file", "${FILE}", "file\n", strings.Repeat("a", 257)} {
		for _, field := range []string{"requirements", "entrypoint"} {
			t.Run(field+"/"+path, func(t *testing.T) {
				r := sourceRecipeFixture()
				if field == "requirements" {
					r.Source.Requirements = path
				} else {
					r.Source.Entrypoint = path
				}
				require.Error(t, r.Validate())
			})
		}
	}
	r := sourceRecipeFixture()
	r.Source.Requirements, r.Source.Entrypoint = "deps/requirements.txt", "app/main.py"
	require.NoError(t, r.Validate())
}

func TestSourceRuntimeOverridesAreRefusedByMembership(t *testing.T) {
	for name, override := range map[string]InstallRecipe{"empty": {}, "restated": sourceRecipeFixture(), "changed timeout": {Timeout: ptr("40m")}} {
		for _, tier := range []string{"defaults", "node"} {
			t.Run(tier+"/"+name, func(t *testing.T) {
				sc := sourceServiceFixture()
				patch := &InstallConfig{Runtimes: map[string]InstallRecipe{"engine": override}}
				if tier == "defaults" {
					sc.Defaults.Install = patch
				} else {
					sc.Nodes["worker"] = NodeSpec{Install: patch}
				}
				_, err := sc.ResolveInstall("worker", "engine")
				require.ErrorContains(t, err, "release-owned")
				require.Error(t, sc.ValidateInstall())
			})
		}
	}
	sc := sourceServiceFixture()
	sc.Install.Runtimes["engine"] = recipeFixture()
	sc.Defaults.Install = &InstallConfig{Runtimes: map[string]InstallRecipe{"engine": {Source: sourceRecipeFixture().Source}}}
	_, err := sc.ResolveInstall("worker", "engine")
	require.ErrorContains(t, err, "release-owned", "a pip override cannot introduce source authority")
}

func TestSourceExecutionContainment(t *testing.T) {
	for name, mutate := range map[string]func(*ServiceConfig){
		"missing runtime":  func(s *ServiceConfig) { s.Runtime = nil },
		"CLI":              func(s *ServiceConfig) { s.Runtime.Execution.Type = "cli" },
		"missing args":     func(s *ServiceConfig) { s.Runtime.Execution.Args = nil },
		"module":           func(s *ServiceConfig) { s.Runtime.Execution.Args = []string{"-m", "engine"} },
		"wrong script":     func(s *ServiceConfig) { s.Runtime.Execution.Args[0] = "other.py" },
		"data ancestor":    func(s *ServiceConfig) { s.Runtime.DataDir = "source/data" },
		"data case":        func(s *ServiceConfig) { s.Runtime.DataDir = "SOURCE" },
		"upstream version": func(s *ServiceConfig) { s.VersionSource = &VersionSource{Type: VersionSourcePyPI, Package: "engine"} },
	} {
		t.Run(name, func(t *testing.T) {
			sc := sourceServiceFixture()
			mutate(&sc)
			_, err := sc.ResolveInstall("worker", "engine")
			require.Error(t, err)
		})
	}
	for _, path := range []string{"", ".", "../source", "/source", "venv", "VENV/lib", "bin", ".candidate", ".previous/old", ".managed", "version", "install.json", "install.json.tmp", "python", "python/lib", "data", "data/code"} {
		t.Run(path, func(t *testing.T) {
			sc := sourceServiceFixture()
			sc.Runtime.Execution.WorkingDir = path
			_, err := sc.ResolveInstall("worker", "engine")
			require.Error(t, err)
		})
	}
	sc := sourceServiceFixture()
	sc.Runtime.Execution.WorkingDir = "payload/source"
	sc.Install.Runtimes["tool"] = sc.Install.Runtimes["engine"]
	sc.Features = map[string]Feature{"vision": {Runtime: "tool", Execution: &sc.Runtime.Execution}}
	_, err := sc.ResolveInstall("worker", "tool")
	require.NoError(t, err, "feature runtime uses its declared execution")
	sc.Features["vision"] = Feature{Runtime: "tool"}
	_, err = sc.ResolveInstall("worker", "tool")
	require.Error(t, err)
}

func TestSourceProviderSaveReloadAndStrictDecoding(t *testing.T) {
	sc := sourceServiceFixture()
	p, err := serviceConfigToProvider(sc.Name, sc)
	require.NoError(t, err)
	data, err := yaml.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, configschema.ValidateOnDemand(data))
	var decoded OnDemandProvider
	require.NoError(t, strictDecode(data, &decoded))
	decoded.Name = sc.Name // Provider names come from the directory, not YAML.
	require.NoError(t, decoded.Validate())
	apps := providersFixture(map[string]Provider{sc.Name: &decoded})
	dir := t.TempDir()
	require.NoError(t, apps.SaveToDir(dir))
	loaded, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	stored, ok := loaded.LookupApp(sc.Name)
	require.True(t, ok)
	resolved, err := stored.ResolveInstall("worker", sc.Name)
	require.NoError(t, err)
	assert.Equal(t, sc.Install.Runtimes[sc.Name].Source, resolved.Recipe.Source)
	assert.Equal(t, "release", resolved.Provenance["source.sha256"])
	assert.False(t, resolved.Overridden)
	resolved.Recipe.Source.SHA256 = strings.Repeat("b", 64)
	assert.Equal(t, strings.Repeat("a", 64), stored.Install.Runtimes[sc.Name].Source.SHA256)
	for _, field := range []string{"unexpected", "dependency_lock"} {
		var body map[string]any
		require.NoError(t, yaml.Unmarshal(data, &body))
		source := body["install"].(map[string]any)["runtimes"].(map[string]any)[sc.Name].(map[string]any)["source"].(map[string]any)
		source[field] = "lock.txt"
		bad, err := yaml.Marshal(body)
		require.NoError(t, err)
		require.Error(t, configschema.ValidateOnDemand(bad))
		require.Error(t, strictDecode(bad, &decoded))
	}
	_, err = MergeInstallPatch(nil, []byte(`{"runtimes":{"engine":{"source":{"unexpected":true}}}}`))
	require.Error(t, err)
}

func TestShippedPipRecipesRemainSourceFree(t *testing.T) {
	for _, name := range []string{"vllm", "mlx"} {
		data, err := templates.AppsFS.ReadFile(filepath.ToSlash(filepath.Join("files/providers/on-demand", name, "config.yaml")))
		require.NoError(t, err)
		require.NoError(t, configschema.ValidateOnDemand(data))
		var p OnDemandProvider
		require.NoError(t, strictDecode(data, &p))
		require.NoError(t, p.Validate())
		for _, r := range p.Install.Runtimes {
			assert.Nil(t, r.Source)
			require.NoError(t, r.Validate())
		}
	}
}
