package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stperic/zzrouter/internal/testproc"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPinnedRestoreServerStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process fixture")
	}
	s := createTestNodeWithDefaults(t)
	require.NoError(t, s.providers.appMgr.Stop(t.Context()))
	engine := filepath.Join(t.TempDir(), "engine")
	require.NoError(t, os.WriteFile(engine, []byte(testproc.Script("trap 'kill $! 2>/dev/null; exit 0' TERM INT\nprintf 'READY\\n'\nwhile kill -0 $test_owner; do /bin/sleep 1 & wait $!; done\n")), 0o700))
	apps := &config.AppsConfig{}
	require.NoError(t, apps.AddApp("generic", config.ServiceConfig{
		Enabled: new(true), Name: "generic", Mode: "on-demand", Protocol: config.ProtocolOpenAI,
		Runtime: &config.AppRuntimeConfig{
			PortRange: []int{17870, 17879}, BasePort: 17870,
			Execution:   config.ExecutionConfig{Type: "cli", Command: engine},
			HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{Timeout: "10s", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}}}},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	data, err := json.Marshal(map[string]any{"version": 1, "runs": []prov_apps.EnsureRequest{{Provider: "generic", Model: "startup", Parameters: map[string]string{"keep-alive": "-1"}}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(config.Paths().GetDataDir(), "pinned_runs.json"), data, 0o600))
	attempts := make(chan prov_apps.EnsureRequest, 1)
	s.providers.appMgr, err = prov_apps.NewProviderAppManager(apps, prov_apps.WithStartObserver(func(ctx context.Context, req prov_apps.EnsureRequest) (context.Context, func(prov_apps.EnsureResult, error)) {
		attempts <- req
		return ctx, func(prov_apps.EnsureResult, error) {}
	}))
	require.NoError(t, err)
	t.Cleanup(func() {
		s.shutdownCancel()
		assert.NoError(t, s.stopCommonSubsystems(context.Background()))
	})
	require.NoError(t, s.startCommonSubsystems())
	select {
	case req := <-attempts:
		assert.Equal(t, prov_apps.OriginRestore, req.Origin)
	case <-time.After(10 * time.Second):
		t.Fatal("server startup did not restore its pin")
	}
	require.Eventually(t, func() bool {
		runs := s.providers.appMgr.ListInstances("")
		return len(runs) == 1 && runs[0].Status == instance.StatusRunning && runs[0].Pinned
	}, 10*time.Second, time.Millisecond)
}
