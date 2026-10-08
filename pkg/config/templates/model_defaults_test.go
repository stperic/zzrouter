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

// templateKeys is the asset-typed chat template flag of each engine the
// Qwen3.8 default ships for.
var templateKeys = map[string]string{
	"llamacpp": "chat-template-file",
	"vllm":     "chat-template",
	"mlx":      "chat-template",
}

const qwenTemplate = "qwen3.8-system-anywhere.jinja"

// The motivating failure: an on-demand load resolves from stored config
// alone, so a model that needs a template must get it from the tree, not
// from a launch-time flag. Every name the model is known by, on every
// engine that launches it, has to resolve to the shipped template.
func TestShippedQwen38DefaultResolvesOnEveryEngine(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	schemas, errs := schema.LoadFromFS(templates.AppsFS, "files/providers")
	require.Empty(t, errs)

	names := []string{
		"Qwen3.8-27B-Q8_0",
		"unsloth/Qwen3.8-27B-GGUF#Qwen3.8-27B-Q8_0.gguf",
		"Qwen/Qwen3.8-27B",
		"mlx-community/Qwen3.8-27B-4bit",
	}
	for provider, key := range templateKeys {
		sc, ok := cfg.LookupApp(provider)
		require.True(t, ok, provider)
		assert.Equal(t, schema.ParamAsset, schemas[provider].Parameters[key].Kind,
			"%s.%s must be asset-typed or the launch passes a bare name", provider, key)
		for _, name := range names {
			got := sc.Resolve("worker-1", name).Parameters[key]
			assert.Equal(t, qwenTemplate, got.Value, "%s %s", provider, name)
			assert.Equal(t, config.TierModelDefault, got.Tier, "%s %s", provider, name)
		}
		assert.NotContains(t, sc.Resolve("worker-1", "Qwen/Qwen3-8B").Parameters, key,
			"%s: another family keeps its own template", provider)
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

// Every model_defaults value of an asset-typed key must name an asset the
// release ships for that same provider, and one template shipped to
// several engines must be one template: a fix to one copy reaches all.
func TestShippedModelDefaultsNameShippedAssets(t *testing.T) {
	schemas, errs := schema.LoadFromFS(templates.AppsFS, "files/providers")
	require.Empty(t, errs)
	copies := map[string][]byte{}
	checked := 0

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
				checked++
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
	assert.GreaterOrEqual(t, checked, len(templateKeys))
}

// An install that predates model_defaults gains it on the next start,
// and an operator's own model entry for the same family survives.
func TestReconcileGivesAnExistingInstallTheModelDefaults(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	for provider := range templateKeys {
		p := filepath.Join(dir, "on-demand", provider, "config.yaml")
		raw, err := os.ReadFile(p) //nolint:gosec // test temp dir
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(raw, &doc))
		delete(doc, "model_defaults")
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
		assert.Equal(t, qwenTemplate, got[key].Value, provider)
		assert.Equal(t, "8", got["threads"].Value, "%s: the operator's model entry survives", provider)
	}
}
