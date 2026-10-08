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
