package templates_test

import (
	"bytes"
	"io/fs"
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

// Every model_defaults value of an asset-typed key must name an asset the
// release ships for that same provider, and one template shipped to
// several engines must be one template: a fix to one copy reaches all.
func TestShippedModelDefaultsNameShippedAssets(t *testing.T) {
	schemas, errs := schema.LoadFromFS(templates.AppsFS, "files/providers")
	require.Empty(t, errs)
	copies := map[string][]byte{}

	err := fs.WalkDir(templates.AppsFS, "files/providers", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Base(p) != "config.yaml" {
			return err
		}
		raw, err := fs.ReadFile(templates.AppsFS, p)
		require.NoError(t, err)
		var doc struct {
			ModelDefaults map[string]config.ModelSpec `yaml:"model_defaults"`
		}
		require.NoError(t, yaml.Unmarshal(raw, &doc), p)
		provider := path.Base(path.Dir(p))
		for pattern, spec := range doc.ModelDefaults {
			for key, value := range spec.Parameters {
				if schemas[provider].Parameters[key].Kind != schema.ParamAsset {
					continue
				}
				data, err := fs.ReadFile(templates.AppsFS, path.Join(path.Dir(p), "assets", value))
				require.NoError(t, err, "%s model_defaults.%s.%s names %q, which %s does not ship", provider, pattern, key, value, provider)
				if prev, ok := copies[value]; ok {
					assert.True(t, bytes.Equal(prev, data), "the shipped copies of %s differ", value)
				}
				copies[value] = data
			}
		}
		return nil
	})
	require.NoError(t, err)
}

// A release that ships no model_defaults takes back the block an earlier
// release wrote, and an operator's own model entry survives.
func TestReconcileRemovesModelDefaultsTheReleaseNoLongerShips(t *testing.T) {
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
		assert.NotContains(t, got, key, "%s: the release's old default is gone", provider)
		assert.Equal(t, "8", got["threads"].Value, "%s: the operator's model entry survives", provider)
	}
}
