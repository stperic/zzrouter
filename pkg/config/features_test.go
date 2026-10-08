package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFeatureValidation(t *testing.T) {
	cases := []struct {
		name    string
		feature Feature
		want    string
	}{
		{"builtin", Feature{}, ""},
		{"runtime missing execution", Feature{Runtime: "mlx-vlm"}, "requires a cli or python"},
		{"dangling execution", Feature{Execution: &ExecutionConfig{Type: "python", Command: "python3"}}, "requires a runtime"},
		{"runtime bad endpoints", Feature{Runtime: "mlx-vlm", Execution: &ExecutionConfig{Type: "python", Command: "python3"}, WireEndpoints: []string{"bogus"}}, "unknown value"},
		{"runtime unknown predicate", Feature{Runtime: "mlx-vlm", When: "bogus", Execution: &ExecutionConfig{Type: "python", Command: "python3"}}, "unknown predicate"},
		{"files", Feature{Default: true, Files: []string{"mmproj-*.gguf"}, Flag: "mmproj"}, ""},
		{"runtime", Feature{Default: true, Runtime: "mlx-vlm", When: "vision_config", Execution: &ExecutionConfig{Type: "python", Command: "python3"}, WireEndpoints: []string{"chat_completions"}}, ""},
		{"exclusive", Feature{Files: []string{"x"}, Flag: "mmproj", Runtime: "mlx-vlm"}, "mutually exclusive"},
		{"missing flag", Feature{Files: []string{"x"}}, "require a flag"},
		{"dangling flag", Feature{Flag: "mmproj"}, "requires files"},
		{"dangling when", Feature{When: "vision_config"}, "requires a runtime"},
		{"builtin default", Feature{Default: true}, "default requires"},
		{"bad glob", Feature{Files: []string{"["}, Flag: "mmproj"}, "invalid file glob"},
	}
	for _, kind := range []string{"llamacpp", "ollama", "openai"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, templates.InstallDefaults(dir))
			cfg, err := LoadAppsConfig(dir)
			require.NoError(t, err)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					sc, _ := cfg.LookupApp(kind)
					sc.Features = map[string]Feature{"vision": tc.feature}
					p, err := serviceConfigToProvider(kind, sc)
					require.NoError(t, err)
					err = p.Validate()
					if tc.want == "" {
						require.NoError(t, err)
					} else {
						require.ErrorContains(t, err, tc.want)
						assert.ErrorContains(t, err, "vision")
					}
				})
			}
			sc, _ := cfg.LookupApp(kind)
			sc.Features = map[string]Feature{"typo": {}}
			p, err := serviceConfigToProvider(kind, sc)
			require.NoError(t, err)
			require.ErrorContains(t, p.Validate(), `feature "typo"`)
		})
	}
}

func TestFeatureTemplatesAndUpdateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	expected := map[string]map[string]Feature{
		"llamacpp": {"vision": {Default: true, Files: []string{"mmproj-F16.gguf", "mmproj-BF16.gguf", "mmproj-*.gguf"}, Flag: "mmproj"}},
		"mlx":      {"vision": {Default: true, Runtime: "mlx-vlm", When: "vision_config", Execution: &ExecutionConfig{Type: "python", Command: "python3", Args: []string{"-m", "mlx_vlm.server", "--host", "127.0.0.1", "--port", "${PORT}", "--model", "${MODEL_PATH}"}, WireModel: WireModelPath}, WireEndpoints: []string{"chat_completions", "responses", "messages"}, ExcludeParameters: []string{"chat-template"}}},
		"vllm":     {"vision": {}},
		"ollama":   {"vision": {}},
	}
	for name, p := range cfg.providers {
		require.NoError(t, p.Validate(), name)
		sc, _ := cfg.LookupApp(name)
		assert.Equal(t, expected[name], sc.Features, name)
	}
	// A cloud declaration exercises the third conversion arm as well.
	require.NoError(t, cfg.UpdateApp("openai", func(sc *ServiceConfig) error { sc.Features = map[string]Feature{"vision": {}}; return nil }))
	expected["openai"] = map[string]Feature{"vision": {}}
	for name, want := range expected {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, cfg.UpdateApp(name, func(sc *ServiceConfig) error { sc.Description = "updated"; return nil }))
			assert.Equal(t, want, providerToServiceConfig(cfg.Find(name)).Features)
		})
	}
	require.NoError(t, cfg.SaveToDir(dir))
	fresh, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	for name, want := range expected {
		sc, _ := fresh.LookupApp(name)
		assert.Equal(t, want, sc.Features, name)
	}
}

func TestFeatureStrictDecode(t *testing.T) {
	var p OnDemandProvider
	require.ErrorContains(t, strictDecode([]byte("features:\n  vision:\n    typo: true\n"), &p), "typo")
	require.NoError(t, strictDecode([]byte("features:\n  vision: {}\n"), &p))
	assert.Equal(t, map[string]Feature{"vision": {}}, p.Features)
}

func TestFeatureEmptyDeclarationSurvivesReconcile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	for _, name := range []string{"llamacpp", "ollama", "openai"} {
		require.NoError(t, cfg.UpdateApp(name, func(sc *ServiceConfig) error { sc.Features = map[string]Feature{}; return nil }))
		require.NoError(t, cfg.UpdateApp(name, func(sc *ServiceConfig) error { sc.Description = "updated"; return nil }))
		sc, _ := cfg.LookupApp(name)
		require.NotNil(t, sc.Features, name)
		assert.Empty(t, sc.Features, name)
	}
	require.NoError(t, cfg.SaveToDir(dir))
	_, err = templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	fresh, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	for _, name := range []string{"llamacpp", "ollama", "openai"} {
		sc, _ := fresh.LookupApp(name)
		require.NotNil(t, sc.Features, name)
		assert.Empty(t, sc.Features, name)
	}
	sc, _ := fresh.LookupApp("groq")
	assert.Nil(t, sc.Features, "absent cloud declarations stay absent")
}

func TestFeatureRuntimeUpgradeFromStep2(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	p := filepath.Join(dir, "on-demand", "mlx", "config.yaml")
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(data, &doc))
	doc["features"] = map[string]any{"vision": map[string]any{"default": false, "when": "vision_config", "runtime": "mlx-vlm"}}
	old, err := yaml.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, old, 0600))
	_, err = templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	cfg, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	svc, _ := cfg.LookupApp("mlx")
	f := svc.Features["vision"]
	assert.False(t, f.Default, "operator policy survives")
	require.NotNil(t, f.Execution)
	assert.Contains(t, f.Execution.Args, "mlx_vlm.server")
	assert.Equal(t, []string{"chat_completions", "responses", "messages"}, f.WireEndpoints)
	require.NoError(t, cfg.SaveToDir(dir))
	_, err = LoadAppsConfig(dir)
	require.NoError(t, err)
}
