package templates_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestOperationDefinitionsAndFlagsSurviveReconcile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	file := filepath.Join(dir, "on-demand", "llamacpp", "config.yaml")
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &body))
	ops := body["operations"].(map[string]any)
	original := ops["tokenize"].(map[string]any)
	original["description"] = "Operator selected tokenizer contract."
	additional := map[string]any{}
	for key, value := range original {
		additional[key] = value
	}
	additional["binding"] = map[string]any{"method": "GET", "path": "/inspect"}
	ops["inspect"] = additional
	body["schema"] = map[string]any{"future-option": map[string]any{"type": "int", "min": 0}}
	body["defaults"].(map[string]any)["parameters"].(map[string]any)["future-option"] = "3"
	modified, err := yaml.Marshal(body)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, modified, 0600))

	// The same descriptor under a new provider identity requires only another definition.
	newDir := filepath.Join(dir, "on-demand", "fixture-engine")
	require.NoError(t, os.MkdirAll(newDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(newDir, "config.yaml"), modified, 0600))
	for range 2 {
		_, err = templates.ReconcileManagedSpine(dir)
		require.NoError(t, err)
		cfg, err := config.LoadAppsConfig(dir)
		require.NoError(t, err)
		for _, provider := range []string{"llamacpp", "fixture-engine"} {
			op, ok := cfg.Operation(provider + ".inspect")
			require.True(t, ok)
			assert.Equal(t, "/inspect", op.Definition().Binding.Path)
			tokenize, ok := cfg.Operation(provider + ".tokenize")
			require.True(t, ok)
			assert.Equal(t, "Operator selected tokenizer contract.", tokenize.Definition().Description)
			view, ok := cfg.LookupApp(provider)
			require.True(t, ok)
			built, problems := schema.Merge(provider, nil, view)
			require.Empty(t, problems)
			require.Empty(t, built.Problems)
			require.Empty(t, schema.Check("future-option", "3", built.Parameters))
			require.NotEmpty(t, schema.Check("future-option", "-1", built.Parameters))
		}
	}
}

func TestOperationDefaultsReachExistingInstallAndPreserveOptOut(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "seed", true: "opt out"}[empty], func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, templates.InstallDefaults(dir))
			file := filepath.Join(dir, "on-demand", "llamacpp", "config.yaml")
			raw, err := os.ReadFile(file)
			require.NoError(t, err)
			var body map[string]any
			require.NoError(t, yaml.Unmarshal(raw, &body))
			delete(body, "operations")
			if empty {
				body["operations"] = map[string]any{}
			}
			delete(body["runtime"].(map[string]any), "listener")
			raw, err = yaml.Marshal(body)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(file, raw, 0600))
			_, err = templates.ReconcileManagedSpine(dir)
			require.NoError(t, err)
			cfg, err := config.LoadAppsConfig(dir)
			require.NoError(t, err)
			_, exists := cfg.Operation("llamacpp.tokenize")
			assert.Equal(t, !empty, exists)
			view, _ := cfg.LookupApp("llamacpp")
			assert.Equal(t, &config.ListenerConfig{HostParameter: "host", DefaultHost: "127.0.0.1"}, view.Runtime.Listener)
			require.NoError(t, cfg.SaveToDir(dir))
			_, err = templates.ReconcileManagedSpine(dir)
			require.NoError(t, err)
			reloaded, err := config.LoadAppsConfig(dir)
			require.NoError(t, err)
			_, exists = reloaded.Operation("llamacpp.tokenize")
			assert.Equal(t, !empty, exists)
		})
	}
}

func TestShippedTokenizerSchemasUseNativeShapes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	llama, ok := cfg.Operation("llamacpp.tokenize")
	require.True(t, ok)
	vllm, ok := cfg.Operation("vllm.tokenize")
	require.True(t, ok)
	require.NoError(t, llama.ValidateInput(map[string]any{"content": "test", "add_special": true}))
	require.Error(t, llama.ValidateInput(map[string]any{"prompt": "test"}))
	require.NoError(t, llama.ValidateOutput(map[string]any{"tokens": []any{1, 2}}))
	require.NoError(t, vllm.ValidateInput(map[string]any{"prompt": "test", "model": "served-name", "return_token_strs": true}))
	require.Error(t, vllm.ValidateInput(map[string]any{"content": "test"}))
	require.NoError(t, vllm.ValidateOutput(map[string]any{"tokens": []any{1, 2}, "count": 2, "max_model_len": 2048, "token_strs": nil}))
	require.Error(t, vllm.ValidateOutput(map[string]any{"tokens": []any{1, 2}}))
}
