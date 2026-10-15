package templates_test

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// templateKeys is the asset-typed chat template flag of each engine.
var templateKeys = map[string]string{
	"llamacpp": "chat-template-file",
	"vllm":     "chat-template",
	"mlx":      "chat-template",
}

func TestLlamacppPlacementReachesExistingInstall(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	file := filepath.Join(dir, "on-demand", "llamacpp", "config.yaml")
	live, err := os.ReadFile(file)
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(live, &cfg))
	runtime := cfg["runtime"].(map[string]any)
	execution := runtime["execution"].(map[string]any)
	args := execution["args"].([]any)
	require.Equal(t, []any{"--log-verbosity", "4"}, args[len(args)-2:])
	execution["args"] = args[:len(args)-2]
	cfg["defaults"].(map[string]any)["parameters"].(map[string]any)["ctx-size"] = "8192"
	stale, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, stale, 0600))
	schemaFile := filepath.Join(dir, "on-demand", "llamacpp", "schema.yaml")
	raw, err := os.ReadFile(schemaFile)
	require.NoError(t, err)
	declared, err := schema.LoadYAMLSchema(raw)
	require.NoError(t, err)
	delete(declared.Parameters, "n-gpu-layers")
	stale, err = yaml.Marshal(declared)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(schemaFile, stale, 0600))
	changed, err := templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	assert.Contains(t, changed, "on-demand/llamacpp/config.yaml")
	assert.Contains(t, changed, "on-demand/llamacpp/schema.yaml")
	apps, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	svc, ok := apps.LookupApp("llamacpp")
	require.True(t, ok)
	assert.Equal(t, "8192", svc.Defaults.Parameters["ctx-size"])
	assert.Equal(t, []string{"--log-verbosity", "4"}, svc.Runtime.Execution.Args[len(svc.Runtime.Execution.Args)-2:])
	schemas, errs := schema.LoadFromFS(os.DirFS(dir), ".")
	require.Empty(t, errs)
	assert.Equal(t, `offloaded (\d+)/(\d+) layers to GPU`, schemas["llamacpp"].Parameters["n-gpu-layers"].ServedLog.String())
}

func TestVLLMToolCallingSchemaReachesExistingInstall(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	file := filepath.Join(dir, "on-demand", "vllm", "schema.yaml")
	live, err := os.ReadFile(file)
	require.NoError(t, err)
	declared, err := schema.LoadYAMLSchema(live)
	require.NoError(t, err)
	for _, key := range []string{"enable-auto-tool-choice", "tool-call-parser", "reasoning-parser"} {
		delete(declared.Parameters, key)
	}
	old, err := yaml.Marshal(declared)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, old, 0o600))
	changed, err := templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	require.Contains(t, changed, "on-demand/vllm/schema.yaml")
	updated, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, live, updated)
	declared, err = schema.LoadYAMLSchema(updated)
	require.NoError(t, err)
	merged, errs := schema.Merge("vllm", declared)
	require.Empty(t, errs)
	for key, kind := range map[string]schema.ParamKind{"enable-auto-tool-choice": schema.ParamBool, "tool-call-parser": schema.ParamString, "reasoning-parser": schema.ParamString} {
		require.Equal(t, kind, merged.Parameters[key].Kind)
		require.Empty(t, merged.Parameters[key].Enum)
		_, inSpine := schema.Lookup("vllm", key)
		require.False(t, inSpine, "missing-feature flags belong in YAML")
	}
}

// mlx_lm.server takes the template's text, not a path.
func TestMLXTakesTheTemplateByContent(t *testing.T) {
	schemas, errs := schema.LoadFromFS(templates.AppsFS, "files/providers")
	require.Empty(t, errs)
	assert.Equal(t, schema.AssetPassContent, schemas["mlx"].Parameters["chat-template"].Pass)
	assert.Empty(t, schemas["llamacpp"].Parameters["chat-template-file"].Pass)
	assert.Empty(t, schemas["vllm"].Parameters["chat-template"].Pass)
}

// model_defaults is retired: an install that still carries the block has it
// stripped before the strict load, which would otherwise refuse the file,
// and the operator's own model entry survives.
func TestReconcileStripsRetiredModelDefaults(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	for provider, key := range templateKeys {
		p := filepath.Join(dir, "on-demand", provider, "config.yaml")
		raw, err := os.ReadFile(p)
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(raw, &doc))
		doc["model_defaults"] = map[string]any{"qwen3.8-*": map[string]any{"parameters": map[string]any{key: "old.jinja"}}}
		doc["models"] = map[string]any{"Qwen3.8-27B-Q8_0": map[string]any{"parameters": map[string]any{"threads": "8"}}}
		old, err := yaml.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, old, 0o600))
	}

	changed, err := templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	cfg, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	for provider, key := range templateKeys {
		assert.Contains(t, changed, path.Join("on-demand", provider, "config.yaml"))
		sc, _ := cfg.LookupApp(provider)
		got := sc.Resolve("", "Qwen3.8-27B-Q8_0").Parameters
		assert.NotContains(t, got, key, "%s: the retired block is gone", provider)
		assert.Equal(t, "8", got["threads"].Value, "%s: the operator's model entry survives", provider)
	}
}
