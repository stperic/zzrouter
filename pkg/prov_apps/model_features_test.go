package prov_apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeFeatureProvisioning(t *testing.T) {
	for _, tc := range []struct {
		name, config   string
		optional, fail bool
		installs       int32
		want           string
	}{
		{"qualifies", `{"vision_config":{}}`, false, false, 1, ""},
		{"text default", `{}`, true, false, 0, ""},
		{"text explicit", `{}`, false, false, 0, "does not qualify"},
		{"install fails", `{"vision_config":{}}`, false, true, 1, "installer failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testAppsConfig()
			require.NoError(t, cfg.UpdateApp("vllm", func(svc *config.ServiceConfig) error {
				svc.Features = map[string]config.Feature{"vision": {Runtime: "mlx-vlm", When: "vision_config", Default: true, Execution: &config.ExecutionConfig{Type: "python", Command: "python3"}, WireEndpoints: []string{"chat_completions"}}}
				return nil
			}))
			m, err := NewProviderAppManager(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { _ = m.Stop(context.Background()) })
			fake := newFakeInstaller("mlx-vlm")
			fake.release()
			if tc.fail {
				fake.installErr.Store(errors.New("installer failed"))
			}
			m.installs.dispatcher.Register("mlx-vlm", fake)
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.config), 0600))
			var optional []string
			if tc.optional {
				optional = []string{"vision"}
			}
			err = m.EnsureModelFeatures(context.Background(), "vllm", dir, []string{"vision"}, optional, func(string) {})
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.installs, fake.installCalls.Load())
			if tc.installs == 1 && !tc.fail {
				require.NoError(t, m.EnsureModelFeatures(context.Background(), "vllm", dir, []string{"vision"}, optional, func(string) {}))
				assert.Equal(t, int32(1), fake.installCalls.Load())
			}
		})
	}
}
