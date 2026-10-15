package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestNodeObservabilityModeValidation(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		mode                      ClusterMode
		enabled, tracing, metrics bool
		endpoint                  string
		rejected                  bool
	}{
		{name: "worker tracing", mode: ClusterModeWorker, enabled: true, tracing: true, rejected: true},
		{name: "worker dormant tracing", mode: ClusterModeWorker, tracing: true, rejected: true},
		{name: "worker export", mode: ClusterModeWorker, enabled: true, metrics: true, endpoint: "http://collector.invalid/private-marker", rejected: true},
		{name: "worker dormant endpoint", mode: ClusterModeWorker, endpoint: "collector.invalid:4317", rejected: true},
		{name: "worker local metrics", mode: ClusterModeWorker, enabled: true, metrics: true},
		{name: "worker all disabled", mode: ClusterModeWorker},
		{name: "coordinator", mode: ClusterModeCoordinator, enabled: true, tracing: true, metrics: true, endpoint: "http://collector.invalid"},
		{name: "standalone", mode: ClusterModeStandalone, enabled: true, tracing: true, endpoint: "collector.invalid:4317"},
		{name: "cluster disabled", mode: ClusterModeDisabled, enabled: true, tracing: true, endpoint: "collector.invalid:4317"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NodeConfig{Node: ServeConfig{Port: 9090}, Cluster: ClusterConfig{Mode: tc.mode}, Observability: ObservabilityConfig{Enabled: tc.enabled, Tracing: TracingConfig{Enabled: tc.tracing}, Metrics: MetricsConfig{Enabled: tc.metrics}, OTLP: OTLPConfig{Endpoint: tc.endpoint}}}
			err := cfg.Validate()
			if tc.rejected {
				require.ErrorIs(t, err, ErrInvalidConfig)
				require.ErrorContains(t, err, "supported only on the coordinator")
				assert.NotContains(t, err.Error(), "private-marker")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func writeObservabilityConfig(t *testing.T, dir string, cfg *NodeConfig) []byte {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), data, 0600))
	return data
}

func TestWorkerObservabilityLoadAndSaveRejected(t *testing.T) {
	cm := NewConfigManager("zzrouter")
	for _, tracing := range []bool{false, true} {
		t.Run(map[bool]string{false: "endpoint", true: "tracing"}[tracing], func(t *testing.T) {
			invalid := &NodeConfig{Node: ServeConfig{Port: 9090}, Cluster: ClusterConfig{Mode: ClusterModeWorker}, Observability: ObservabilityConfig{Tracing: TracingConfig{Enabled: tracing}}}
			if !tracing {
				invalid.Observability.OTLP.Endpoint = "http://collector.invalid"
			}
			dir := t.TempDir()
			before := writeObservabilityConfig(t, dir, invalid)
			loaded, err := cm.LoadNodeConfigFromDir(dir)
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.Nil(t, loaded)
			loaded, err = readNodeConfigFile(filepath.Join(dir, "node.yaml"))
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.Nil(t, loaded)
			require.ErrorIs(t, cm.SaveNodeConfigToDir(invalid, dir), ErrInvalidConfig)
			after, err := os.ReadFile(filepath.Join(dir, "node.yaml"))
			require.NoError(t, err)
			assert.Equal(t, before, after)
			absent := filepath.Join(t.TempDir(), "new-config")
			require.ErrorIs(t, cm.SaveNodeConfigToDir(invalid, absent), ErrInvalidConfig)
			_, err = os.Stat(absent)
			require.True(t, os.IsNotExist(err), "refused save must not create a directory")
		})
	}
}

func TestWorkerObservabilityReloadRetainsAppliedStartupState(t *testing.T) {
	for _, startTracing := range []bool{false, true} {
		for _, twoSteps := range []bool{false, true} {
			name := map[bool]string{false: "export", true: "tracing"}[startTracing] + map[bool]string{false: " atomic", true: " two step"}[twoSteps]
			t.Run(name, func(t *testing.T) {
				initial := &NodeConfig{Node: ServeConfig{Port: 9090}, Cluster: ClusterConfig{Mode: ClusterModeCoordinator}, Observability: ObservabilityConfig{Enabled: true, Tracing: TracingConfig{Enabled: startTracing}}}
				if !startTracing {
					initial.Observability.OTLP.Endpoint = "http://collector.invalid"
					initial.Observability.Metrics.Enabled = true
				}
				store := NewNodeConfigStoreFromConfig(initial)
				notifications := 0
				store.OnChange("test", func(*NodeConfig) ReloadDisposition { notifications++; return DispositionApplied })
				next := *initial
				next.Observability = ObservabilityConfig{Enabled: true, Metrics: MetricsConfig{Enabled: true}}
				// Use the store's normal loader path, without any real user configuration.
				t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
				store.dir = store.cm.GetNodeConfigDir()
				require.NoError(t, os.MkdirAll(store.dir, 0700))
				if twoSteps {
					writeObservabilityConfig(t, store.dir, &next)
					_, err := store.Reload()
					require.NoError(t, err)
					require.Equal(t, 1, notifications)
				}
				before := store.Config()
				next.Cluster.Mode = ClusterModeWorker
				diskBefore := writeObservabilityConfig(t, store.dir, &next)
				_, err := store.Reload()
				require.ErrorIs(t, err, ErrInvalidConfig)
				require.ErrorContains(t, err, "restart")
				require.Same(t, before, store.Config())
				assert.Equal(t, ClusterModeCoordinator, store.ClusterMode())
				assert.Equal(t, map[bool]int{false: 0, true: 1}[twoSteps], notifications)
				_, err = store.SetClusterJoined()
				require.ErrorIs(t, err, ErrInvalidConfig)
				assert.Equal(t, ClusterModeCoordinator, store.ClusterMode())
				assert.Equal(t, map[bool]int{false: 0, true: 1}[twoSteps], notifications)
				diskAfter, err := os.ReadFile(filepath.Join(store.dir, "node.yaml"))
				require.NoError(t, err)
				assert.Equal(t, diskBefore, diskAfter)
			})
		}
	}
}

func TestWorkerTransitionUsesStartupObservabilityInsteadOfPendingReload(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	initial := &NodeConfig{Node: ServeConfig{Port: 9090}, Cluster: ClusterConfig{Mode: ClusterModeCoordinator},
		Observability: ObservabilityConfig{Enabled: true, Metrics: MetricsConfig{Enabled: true}}}
	store := NewNodeConfigStoreFromConfig(initial)
	require.NoError(t, os.MkdirAll(store.dir, 0700))
	next := *initial
	next.Observability.Tracing.Enabled = true
	writeObservabilityConfig(t, store.dir, &next)
	_, err := store.Reload()
	require.NoError(t, err)
	// Tracing requested by a reload has not been applied to the running node.
	next.Observability.Tracing.Enabled = false
	writeObservabilityConfig(t, store.dir, &next)
	_, err = store.Reload()
	require.NoError(t, err)
	_, err = store.SetClusterJoined()
	require.NoError(t, err)
	assert.Equal(t, ClusterModeWorker, store.ClusterMode())
}
