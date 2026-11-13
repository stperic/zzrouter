package prov_apps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stretchr/testify/require"
)

func reloadFixtureCloud(t *testing.T, cfg *config.AppsConfig) {
	t.Helper()
	require.NoError(t, cfg.AddApp("cloud-fixture", config.ServiceConfig{Mode: "cloud", Enabled: new(false), Runtime: &config.AppRuntimeConfig{Endpoint: "https://cloud.example.test", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "synthetic"}}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
}

func reloadFixtureStore(t *testing.T, cfg *config.AppsConfig) (*config.AppsConfigStore, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, cfg.SaveToDir(dir))
	store, err := config.NewAppsConfigStore(dir)
	require.NoError(t, err)
	return store, dir
}

func reloadFixtureFile(t *testing.T, dir, kind, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, kind, name, "config.yaml"))
	require.NoError(t, err)
	return body
}

func TestUnavailableConfigPreservesServedRun(t *testing.T) {
	r := newServedRig(t)
	cfg := r.config.Load()
	reloadFixtureCloud(t, cfg)
	store, dir := reloadFixtureStore(t, cfg)
	store.OnChange("providerAppManager", func(cfg *config.AppsConfig) config.ReloadDisposition {
		r.config.Store(cfg)
		return r.m.ReloadConfig(cfg)
	})
	r.config.Store(store.Config())
	r.m.ReloadConfig(store.Config())
	run := r.running(t, EnsureRequest{Provider: "ollama", Model: servedModel})
	require.True(t, r.daemon.isLoaded(run.Model))
	oldCloud := reloadFixtureFile(t, dir, "cloud", "cloud-fixture")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cloud", "cloud-fixture", "config.yaml"), []byte("unknown_field: true\n"), 0600))
	body := append(reloadFixtureFile(t, dir, "external", "ollama"), []byte("\n# valid unrelated pushed bytes\n")...)
	changed, err := store.WriteProviderFiles("ollama", config.KindExternal, config.ProviderFiles{Config: body})
	require.Error(t, err)
	require.True(t, changed)
	require.Nil(t, store.Config())
	require.Nil(t, r.m.AppsConfig())
	require.True(t, r.m.instances.Has(run.ID), "unavailable config must retain the run record")
	require.True(t, r.daemon.isLoaded(run.Model), "external daemon did not unload the model")
	require.Zero(t, r.daemon.count(true), "nil config sent no daemon unload")
	require.NotContains(t, runLog(t, run), "disabled or removed")
	_, err = r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "ollama", Model: servedModel})
	require.Error(t, err, "new work remains fail-closed")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cloud", "cloud-fixture", "config.yaml"), oldCloud, 0600))
	_, err = store.Reload()
	require.NoError(t, err)
	replacement := r.running(t, EnsureRequest{Provider: "ollama", Model: servedModel})
	require.Equal(t, run.ID, replacement.ID, "repair preserves the serving identity")
	r.config.Store(nil)
	require.Equal(t, config.DispositionRejected, r.m.ReloadConfig(nil))
	require.Equal(t, config.DispositionRejected, r.m.ReloadConfig(nil))
	empty := &config.AppsConfig{}
	r.config.Store(empty)
	r.m.ReloadConfig(empty)
	require.False(t, r.m.instances.Has(run.ID), "validated removal still retires the run")
}

func TestVariantMovePreservesUnrelatedManagedDaemon(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	cfg := m.AppsConfig()
	onDemand, _ := cfg.LookupApp("vllm")
	sourceRuntime := *onDemand.Runtime
	sourceRuntime.PortRange, sourceRuntime.BasePort = []int{8120, 8125}, 8120
	onDemand.Runtime = &sourceRuntime
	onDemand.Models = map[string]config.ModelSpec{"moving-alias": {From: "base-model"}}
	require.NoError(t, cfg.AddApp("source-fixture", onDemand))
	destinationRuntime := *onDemand.Runtime
	destinationRuntime.PortRange, destinationRuntime.BasePort = []int{8140, 8145}, 8140
	onDemand.Runtime = &destinationRuntime
	onDemand.Models = nil
	require.NoError(t, cfg.AddApp("destination-fixture", onDemand))
	store, dir := reloadFixtureStore(t, cfg)
	store.OnChange("providerAppManager", m.ReloadConfig)
	m.ReloadConfig(store.Config())
	m.Start(context.Background())
	require.Eventually(t, func() bool { return serviceStatus(t, m).Running }, 5*time.Second, 10*time.Millisecond)
	firstPID := serviceStatus(t, m).PID
	require.Positive(t, firstPID)
	// Prepare one entirely valid coordinator tree, then deliver its two files
	// separately as provider sync does. Destination first creates a transient
	// duplicate, although both old and final complete trees validate.
	desired, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	require.NoError(t, desired.UpdateApp("source-fixture", func(s *config.ServiceConfig) error { s.Models = nil; return nil }))
	require.NoError(t, desired.UpdateApp("destination-fixture", func(s *config.ServiceConfig) error {
		s.Models = map[string]config.ModelSpec{"moving-alias": {From: "base-model"}}
		return nil
	}))
	desiredDir := t.TempDir()
	require.NoError(t, desired.SaveToDir(desiredDir))
	_, err = config.LoadAppsConfig(desiredDir)
	require.NoError(t, err, "final coordinator tree is valid")
	_, err = store.WriteProviderFiles("destination-fixture", config.KindOnDemand, config.ProviderFiles{Config: reloadFixtureFile(t, desiredDir, "on-demand", "destination-fixture")})
	require.ErrorIs(t, err, config.ErrInvalidVariant)
	require.Nil(t, store.Config(), "refuse new work until the complete valid snapshot arrives")
	require.Nil(t, m.AppsConfig(), "manager reads also fail closed")
	_, err = store.SetProviderEnabled("generic-daemon", "", false)
	require.Error(t, err, "mutations refuse an unavailable tree")
	_, err = m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic-daemon", Model: "new-model"})
	require.Error(t, err, "new work refuses an unavailable tree")
	state := m.serviceState("generic-daemon")
	state.mu.Lock()
	state.unreconciled = true
	state.mu.Unlock()
	m.reconcileServices(true)
	reconcileErr := m.reconcileService(t.Context(), "generic-daemon")
	state.mu.Lock()
	running, pid := state.done != nil, state.status.PID
	state.mu.Unlock()
	require.True(t, running, "incomplete sync must not stop the unrelated daemon")
	require.Equal(t, firstPID, pid)
	require.True(t, process.ProcessExists(int32(firstPID)), "the same daemon remains physically alive")
	require.ErrorIs(t, reconcileErr, ErrConfigUnavailable)
	_, err = store.WriteProviderFiles("source-fixture", config.KindOnDemand, config.ProviderFiles{Config: reloadFixtureFile(t, desiredDir, "on-demand", "source-fixture")})
	require.NoError(t, err)
	require.NotNil(t, store.Config())
	require.Eventually(t, func() bool { return serviceStatus(t, m).Running }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, firstPID, serviceStatus(t, m).PID, "repair must preserve the daemon process")
	_, err = store.SetProviderEnabled("generic-daemon", "", false)
	require.NoError(t, err)
	require.False(t, process.ProcessExists(int32(firstPID)), "validated disable still stops the managed daemon")
}
