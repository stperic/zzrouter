package templates

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestCloudRoutingDefaultsReachExistingInstalls(t *testing.T) {
	template := []byte("connection: {}\nruntime:\n  openai_path: /compatible\n  models_path: ''\n")
	for _, tc := range []struct{ name, runtime, wantPath, wantModels string }{
		{"missing", "endpoint: https://operator.test\n", "/compatible", ""},
		{"explicit", "endpoint: https://operator.test\n  openai_path: /operator\n  models_path: /catalog\n", "/operator", "/catalog"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := []byte("enabled: true\nruntime:\n  " + tc.runtime)
			got, _, err := mergeManagedSpine(live, template, nil)
			require.NoError(t, err)
			var doc map[string]any
			require.NoError(t, yaml.Unmarshal(got, &doc))
			runtime := doc["runtime"].(map[string]any)
			require.Equal(t, "https://operator.test", runtime["endpoint"])
			require.Equal(t, tc.wantPath, runtime["openai_path"])
			require.Equal(t, tc.wantModels, runtime["models_path"])
			again, changed, err := mergeManagedSpine(got, template, nil)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, got, again)
		})
	}
}
