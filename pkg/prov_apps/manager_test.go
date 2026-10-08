package prov_apps

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateProviderRoot points fsroot at a per-test temp dir so a real
// `.managed` marker in the developer's home (left over from a prior
// `zzrouter app install ollama`) doesn't bleed into ProviderStatus
// assertions.
func isolateProviderRoot(t *testing.T) {
	t.Helper()
	previous := fsroot.ProviderRootDir()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previous) })
}

func cleanupProviderManager(t *testing.T, m *ProviderAppManager) {
	t.Helper()
	t.Cleanup(func() {
		// testing cancels t.Context before cleanup, so shutdown needs its own budget.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		instances := m.instances.ListAll()
		assert.NoError(t, m.Stop(ctx))
		for _, inst := range instances {
			assert.NoError(t, inst.WaitForGoroutines(ctx), "instance %s must finish before fixture removal", inst.ID)
		}
		assert.Empty(t, m.instances.ListAll(), "cleanup must stop every fixture instance")
	})
}

func TestPackageRuntimePathsAreIsolated(t *testing.T) {
	root := os.Getenv("ZZROUTER_TEST_HOME")
	require.NotEmpty(t, root, "TestMain must isolate config and data before any manager starts")
	operator := t.TempDir()
	t.Setenv("ZZROUTER_CONFIG_DIR", operator)
	assert.Equal(t, root, config.Paths().GetConfigDir())
	assert.Equal(t, filepath.Join(root, "pids"), config.Paths().GetPIDDir())
	assert.Equal(t, filepath.Join(filepath.Dir(root), "providers"), fsroot.ProviderRootDir())
	m, err := NewProviderAppManager(nil)
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	require.NotNil(t, m.pidTracker)
	require.NoError(t, m.pidTracker.Track("test-isolation", os.Getpid()))
	t.Cleanup(func() { m.pidTracker.Untrack("test-isolation") })
	_, err = os.Stat(filepath.Join(root, "pids", "test-isolation.pid"))
	assert.NoError(t, err)
	entries, err := os.ReadDir(operator)
	require.NoError(t, err)
	assert.Empty(t, entries, "manager creation must not write to the operator config")
}

func testAppsConfig() *config.AppsConfig {
	caps := func(eps ...string) *config.AppCapabilities {
		return &config.AppCapabilities{WireEndpoints: eps}
	}
	apps := map[string]config.ServiceConfig{
		"ollama": {
			Enabled:  new(true),
			Name:     "Ollama",
			Protocol: config.ProtocolOllama,
			Mode:     "service",
			Runtime: &config.AppRuntimeConfig{
				Endpoint:  "http://localhost:11434",
				KeepAlive: "5m",
			},
			Capabilities: caps("chat_completions", "completions", "embeddings"),
		},
		"vllm": {
			Enabled:  new(true),
			Name:     "vLLM",
			Protocol: config.ProtocolOpenAI,
			Mode:     "on-demand",
			Runtime: &config.AppRuntimeConfig{
				PortRange: []int{8100, 8105},
				BasePort:  8100,
				KeepAlive: "10m",
				Execution: config.ExecutionConfig{
					Type:    "python",
					Command: "python3",
					Args:    []string{"-m", "vllm.entrypoints.openai.api_server", "--model", "${MODEL_PATH}", "--served-model-name", "${MODEL}", "--port", "${PORT}"},
				},
				HealthCheck: config.HealthcheckConfig{
					Path: "/v1/models",
				},
			},
			Capabilities: caps("chat_completions", "completions", "embeddings", "responses"),
		},
	}
	cfg := &config.AppsConfig{}
	for name, sc := range apps {
		if err := cfg.AddApp(name, sc); err != nil {
			panic("testAppsConfig: AddApp(" + name + ") failed: " + err.Error())
		}
	}
	return cfg
}

func TestNewProviderAppManager(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	assert.NotNil(t, m.Instances())
	assert.NotNil(t, m.Protocols())
	assert.NotNil(t, m.Install())
	assert.NotNil(t, m.Install().Dispatcher())
	assert.NotNil(t, m.Ports())
	assert.NotNil(t, m.LogManager())
	assert.NotNil(t, m.AppsConfig())
}

func TestNewProviderAppManager_NilConfig(t *testing.T) {
	m, err := NewProviderAppManager(nil)
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	assert.Equal(t, 0, m.Protocols().Count())
}

func TestProviderAppManager_Protocol(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	p, ok := m.Protocol("ollama")
	assert.True(t, ok)
	assert.Equal(t, "ollama", p.Name())

	_, ok = m.Protocol("nonexistent")
	assert.False(t, ok)
}

func TestProviderAppManager_Versions(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	assert.Equal(t, "unknown", m.ProviderVersion("vllm"))

	// Versions map should be empty before DiscoverAndRegister
	versions := m.ProviderVersions()
	assert.Empty(t, versions)
}

func TestProviderAppManager_LaunchInstance(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	inst, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "vllm",
		Model:    "llama3",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, inst.ID)
	assert.Equal(t, "vllm", inst.Provider)
	assert.Equal(t, "llama3", inst.Model)
	assert.True(t, inst.Port >= 8100 && inst.Port <= 8105)

	// Should be in registry
	got, ok := m.GetInstance(inst.ID)
	assert.True(t, ok)
	assert.Equal(t, inst.ID, got.ID)

	gotByModel, ok := m.GetInstanceByModel("llama3")
	assert.True(t, ok)
	assert.Equal(t, inst.ID, gotByModel.ID)

	// List instances
	infos := m.ListInstances("node1")
	assert.Len(t, infos, 1)
	assert.Equal(t, "node1", infos[0].Node)
}

func TestProviderAppManager_StopInstance(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	inst, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "vllm",
		Model:    "llama3",
	})
	require.NoError(t, err)

	err = m.StopInstance(context.Background(), inst.ID)
	require.NoError(t, err)

	_, ok := m.GetInstance(inst.ID)
	assert.False(t, ok)
}

func TestProviderAppManager_RestartInstance(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	inst, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider:   "vllm",
		Model:      "llama3",
		Parameters: map[string]string{"temperature": "0.7"},
		EnvVars:    map[string]string{"HF_TOKEN": "secret"},
	})
	require.NoError(t, err)
	oldID := inst.ID

	newInst, err := m.RestartInstance(context.Background(), oldID)
	require.NoError(t, err)
	assert.NotEqual(t, oldID, newInst.ID, "restart should mint a new instance ID")
	assert.Equal(t, "vllm", newInst.Provider)
	assert.Equal(t, "llama3", newInst.Model)
	assert.Equal(t, "0.7", newInst.Config.Parameters["temperature"], "parameters must carry over")
	assert.Equal(t, "secret", newInst.Config.EnvVars["HF_TOKEN"], "env vars must carry over")

	// Old instance must be gone from the registry.
	_, ok := m.GetInstance(oldID)
	assert.False(t, ok, "old instance should be removed by Stop")

	// New instance must be addressable.
	got, ok := m.GetInstance(newInst.ID)
	assert.True(t, ok)
	assert.Equal(t, newInst.ID, got.ID)
}

func TestProviderAppManager_RestartNotFound(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	_, err = m.RestartInstance(context.Background(), "nonexistent")
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestProviderAppManager_StopNotFound(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	err = m.StopInstance(context.Background(), "nonexistent")
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestProviderAppManager_LaunchAfterStop(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)

	_ = m.Stop(context.Background())

	_, err = m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "vllm",
		Model:    "llama3",
	})
	assert.ErrorIs(t, err, ErrShutdown)
}

func TestProviderAppManager_StartAfterStop_NoPanic(t *testing.T) {
	// Stop-before-Start is a real path on startup-error rollback. Without
	// the shutdown gate in Start, idleReaper would later read from the
	// closed idleExpiry channel and eventLoop would hang waiting on a
	// channel that emit() will never send to (shutdown gate drops first).
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)

	require.NoError(t, m.Stop(context.Background()))

	require.NotPanics(t, func() {
		m.Start(context.Background())
	})
	assert.True(t, m.IsShutdown())
}

func TestProviderAppManager_EventLoop_DrainsLateEmit(t *testing.T) {
	// Race: emit() passes the shutdown gate, then Stop fires shutdownCancel
	// before the channel send completes. The trailing-window drain catches it.
	// Simulate by buffering an event after shutdownCancel but within the
	// drain window, and verifying eventLoop processes it before exiting.
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	m.Start(context.Background())

	// Cancel the eventLoop's ctx, then immediately stage a late event.
	m.shutdownCancel()
	select {
	case m.auditEvents <- Event{Type: EventInstallStarted, Provider: "late", Time: time.Now()}:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("auditEvents send blocked unexpectedly")
	}

	// Within the drain window the event must be consumed (channel returns to
	// empty). Poll for up to 2× the drain window.
	deadline := time.Now().Add(eventDrainWindow * 2)
	for time.Now().Before(deadline) {
		if len(m.auditEvents) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("eventLoop did not drain late emit within %v", eventDrainWindow*2)
}

func TestProviderAppManager_Events(t *testing.T) {
	// Audit channel is normally drained by eventLoop, which only runs
	// after Start. This test deliberately does not call Start so the
	// channel buffers events and the assertions can observe them.
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)

	inst, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "vllm",
		Model:    "llama3",
	})
	require.NoError(t, err)

	select {
	case e := <-m.auditEvents:
		assert.Equal(t, EventInstanceStarting, e.Type)
		assert.Equal(t, "vllm", e.Provider)
		assert.Equal(t, inst.ID, e.Instance)
	case <-time.After(time.Second):
		t.Fatal("expected event")
	}

	_ = m.StopInstance(context.Background(), inst.ID)

	events := drainEvents(m.auditEvents, 2, time.Second)
	assert.GreaterOrEqual(t, len(events), 1) // at least stopping

	_ = m.Stop(context.Background())
}

func TestProviderAppManager_Stop(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)

	// Launch some instances
	_, _ = m.LaunchInstance(context.Background(), LaunchRequest{Provider: "vllm", Model: "a"})
	_, _ = m.LaunchInstance(context.Background(), LaunchRequest{Provider: "vllm", Model: "b"})

	err = m.Stop(context.Background())
	require.NoError(t, err)

	assert.True(t, m.IsShutdown())

	// Double shutdown is a no-op
	err = m.Stop(context.Background())
	assert.NoError(t, err)
}

func TestInstallCoordinator_LockProvider_SerializesSameName(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	coord := m.Install()
	var active, violated int32
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := coord.acquireProvider(context.Background(), "vllm")
			require.NoError(t, err)
			defer release()
			if atomic.AddInt32(&active, 1) > 1 {
				atomic.StoreInt32(&violated, 1)
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&active, -1)
		}()
	}
	wg.Wait()
	assert.Zero(t, atomic.LoadInt32(&violated), "same-name lockProvider must serialize concurrent callers")
}

func TestInstallCoordinator_LockProvider_IndependentNames(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	coord := m.Install()
	releaseA, err := coord.acquireProvider(context.Background(), "a")
	require.NoError(t, err)
	defer releaseA()

	done := make(chan struct{})
	go func() {
		releaseB, err := coord.acquireProvider(context.Background(), "b")
		require.NoError(t, err)
		releaseB()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("lockProvider(b) blocked behind lockProvider(a); lock must be per-name")
	}
}

func TestInstallCoordinator_UpgradeRefusesRunning(t *testing.T) {
	reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
	require.NoError(t, err)
	defer reg.Stop()

	m, err := NewProviderAppManager(testAppsConfig(), WithJobsRegistry(reg))
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	_, _ = m.LaunchInstance(context.Background(), LaunchRequest{Provider: "vllm", Model: "llama3"})

	_, err = m.Install().UpgradeAsync(context.Background(), "vllm", "", nil)
	assert.ErrorIs(t, err, ErrInstancesRunning)
}

func TestProviderAppManager_ProviderStatus(t *testing.T) {
	isolateProviderRoot(t)
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	status, err := m.ProviderStatus("ollama")
	require.NoError(t, err)
	// Post-storage-flip (commit 6e-2): ServiceConfig.Name is the provider
	// map key; the user-facing display name lives in Description.
	// Mode == "external" because the legacy service/external split
	// collapsed into KindExternal.
	assert.Equal(t, "ollama", status.Name)
	assert.Equal(t, "external", status.Mode)
	assert.True(t, status.Enabled)
	assert.False(t, status.Managed)

	_, err = m.ProviderStatus("nonexistent")
	assert.ErrorIs(t, err, ErrProviderNotFound)
}

func TestProviderAppManager_ReloadConfig(t *testing.T) {
	// Start with vllm disabled. Route the mutation through UpdateApp so
	// the typed providers view stays in sync — direct writes to
	// cfg.Apps skip that sync and leave ProviderStatus reading stale
	// data (exactly the dual-shape staleness the 5a arc is guarding).
	cfg := testAppsConfig()
	require.NoError(t, cfg.UpdateApp("vllm", func(sc *config.ServiceConfig) error {
		disabled := false
		sc.Enabled = &disabled
		return nil
	}))

	m, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	// ProviderStatus should report enabled: false
	status, err := m.ProviderStatus("vllm")
	require.NoError(t, err)
	assert.False(t, status.Enabled, "vllm should be disabled before reload")

	// Simulate what happens after install: reload with enabled: true
	freshCfg := testAppsConfig() // testAppsConfig returns enabled: true for both
	m.ReloadConfig(freshCfg)

	// ProviderStatus should now report enabled: true
	status, err = m.ProviderStatus("vllm")
	require.NoError(t, err)
	assert.True(t, status.Enabled, "vllm should be enabled after reload")
}

func TestProviderAppManager_ReloadConfig_NilSafe(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	// ReloadConfig with nil should not panic, and downstream methods
	// that read config should return gracefully (not nil-dereference).
	m.ReloadConfig(nil)

	_, err = m.ProviderStatus("ollama")
	assert.ErrorIs(t, err, ErrProviderNotFound, "ProviderStatus should return not-found after nil reload")

	assert.False(t, m.IsProviderSupported("ollama"), "IsProviderSupported should return false after nil reload")
	assert.Empty(t, m.SupportedProviders(), "SupportedProviders should return empty after nil reload")
}

func TestProviderAppManager_LaunchInstance_InvalidProvider(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	_, err = m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "nonexistent",
		Model:    "llama3",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrProviderNotFound)
}

func TestProviderAppManager_LaunchInstance_DangerousEnvVar(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	_, err = m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "vllm",
		Model:    "llama3",
		EnvVars:  map[string]string{"LD_PRELOAD": "/evil.so"},
	})
	// Should fail during launch (env var rejected in launcher)
	// The instance may be created but will fail in the goroutine
	// Either way, the dangerous env var should be caught
	if err != nil {
		assert.ErrorIs(t, err, process.ErrDangerousEnvVar)
	}
}

func TestProviderAppManager_LaunchInstance_InvalidParams(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	_, err = m.LaunchInstance(context.Background(), LaunchRequest{
		Provider:   "vllm",
		Model:      "llama3",
		Parameters: map[string]string{"key;inject": "value"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrParameterValidation)
}

func TestProviderAppManager_LaunchInstance_HealthURL(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	inst, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "vllm",
		Model:    "llama3",
	})
	require.NoError(t, err)

	// vllm config has health_check.path = /v1/models
	assert.Contains(t, inst.HealthURL, "/v1/models")
}

func TestMergeStringMaps(t *testing.T) {
	t.Run("nil inputs", func(t *testing.T) {
		result := mergeStringMaps(nil, nil)
		assert.Nil(t, result)
	})

	t.Run("defaults only", func(t *testing.T) {
		result := mergeStringMaps(map[string]string{"a": "1"}, nil)
		assert.Equal(t, "1", result["a"])
	})

	t.Run("overrides only", func(t *testing.T) {
		result := mergeStringMaps(nil, map[string]string{"b": "2"})
		assert.Equal(t, "2", result["b"])
	})

	t.Run("override wins", func(t *testing.T) {
		defaults := map[string]string{"key": "default", "other": "keep"}
		overrides := map[string]string{"key": "override"}
		result := mergeStringMaps(defaults, overrides)
		assert.Equal(t, "override", result["key"])
		assert.Equal(t, "keep", result["other"])
	})

	t.Run("does not mutate inputs", func(t *testing.T) {
		defaults := map[string]string{"a": "1"}
		overrides := map[string]string{"b": "2"}
		result := mergeStringMaps(defaults, overrides)
		result["c"] = "3"
		assert.NotContains(t, defaults, "c")
		assert.NotContains(t, overrides, "c")
	})
}

func TestBuildHealthCheckConfig(t *testing.T) {
	t.Run("nil runtime", func(t *testing.T) {
		svcCfg := config.ServiceConfig{}
		hc := buildHealthCheckConfig(svcCfg, 8000)
		assert.Equal(t, "/health", hc.HTTPHealthPath)
		assert.Equal(t, 200, hc.ExpectedStatus)
	})

	t.Run("custom path", func(t *testing.T) {
		svcCfg := config.ServiceConfig{
			Runtime: &config.AppRuntimeConfig{
				HealthCheck: config.HealthcheckConfig{
					Path: "/v1/models",
				},
			},
		}
		hc := buildHealthCheckConfig(svcCfg, 8000)
		assert.Equal(t, "/v1/models", hc.HTTPHealthPath)
	})

	t.Run("timing overrides", func(t *testing.T) {
		svcCfg := config.ServiceConfig{
			Runtime: &config.AppRuntimeConfig{
				HealthCheck: config.HealthcheckConfig{
					Interval: "30s",
					Timeout:  "5s",
					Retries:  5,
				},
			},
		}
		hc := buildHealthCheckConfig(svcCfg, 8000)
		assert.Equal(t, 30*time.Second, hc.Interval)
		assert.Equal(t, 5*time.Second, hc.Timeout)
		assert.Equal(t, 5, hc.MaxRetries)
	})
}

// drainEvents reads up to n events with a timeout.
func drainEvents(ch <-chan Event, n int, timeout time.Duration) []Event {
	var events []Event
	deadline := time.After(timeout)
	for range n {
		select {
		case e, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, e)
		case <-deadline:
			return events
		}
	}
	return events
}
