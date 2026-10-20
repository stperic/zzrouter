package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestOperatorSchemaConfigRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		body string
	}{
		{KindOnDemand, "run_scope: node\nenabled: false\nruntime: {port_range: [8188, 8188], base_port: 8188, execution: {type: cli, command: engine}}\n"},
		{KindExternal, "protocol: ollama\nenabled: false\nruntime: {endpoint: 'http://localhost:11434'}\n"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			const block = "schema:\n  future: {type: int, min: 1, max: 9, enum: ['2'], description: operator, auto: auto}\n  literal_flag: {type: bool, negatable: true}\n  template: {type: asset, pass: content}\n  bad: {type: invented}\n"
			p, err := DecodeProviderBytes(tc.kind, "eng", []byte(tc.body+block))
			require.NoError(t, err)
			require.NoError(t, p.Validate(), "semantic declarations never fail provider load")
			view := providerToServiceConfig(p)
			require.Len(t, view.Schema, 4)
			assert.Equal(t, "invented", *view.Schema["bad"].Type)
			restored, err := serviceConfigToProvider("eng", view)
			require.NoError(t, err)
			copy, err := cloneProvider(restored)
			require.NoError(t, err)
			assert.Equal(t, restored, copy)
			copied := providerToServiceConfig(copy)
			*copied.Schema["future"].Min = 3
			(*copied.Schema["future"].Enum)[0] = "4"
			assert.Equal(t, 1.0, *view.Schema["future"].Min)
			assert.Equal(t, []string{"2"}, *view.Schema["future"].Enum)
			root := t.TempDir()
			dir := filepath.Join(root, kindDirKeyFor(tc.kind), "eng")
			require.NoError(t, os.MkdirAll(dir, 0700))
			data, err := yaml.Marshal(p)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0600))
			cfg, err := LoadAppsConfig(root)
			require.NoError(t, err)
			loaded, ok := cfg.LookupApp("eng")
			require.True(t, ok)
			assert.Equal(t, view.Schema, loaded.Schema)
		})
	}
}

func TestOperatorSchemaUnknownFieldFailsLoad(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "on-demand", "eng")
	require.NoError(t, os.MkdirAll(dir, 0700))
	body := []byte("run_scope: node\nruntime: {port_range: [8188, 8188], base_port: 8188, execution: {type: cli, command: engine}}\nschema:\n  future: {type: string, served: injected}\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), body, 0600))
	_, err := LoadAppsConfig(root)
	require.Error(t, err)
	_, err = DecodeProviderBytes(KindOnDemand, "eng", body)
	require.Error(t, err)
}
