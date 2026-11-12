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
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func holdFirstReload(t *testing.T, store *config.AppsConfigStore, apply func(*config.AppsConfig) config.ReloadDisposition) (<-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	store.OnChange("manager", func(cfg *config.AppsConfig) config.ReloadDisposition {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		assert.Equal(t, cfg, store.Config(), "the callback can read its current store snapshot")
		return apply(cfg)
	})
	return entered, unblock
}

func waitConfigPublication(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("config publication did not reach the expected barrier")
	}
}

func assertConfigPublicationWaiting(t *testing.T, done <-chan struct{}) {
	t.Helper()
	deadline := time.NewTimer(100 * time.Millisecond)
	defer deadline.Stop()
	select {
	case <-done:
		t.Error("new publication overtook a paused older listener")
	case <-deadline.C:
	}
}

func TestConfigPublicationOrdersUnavailableRecovery(t *testing.T) {
	r := newServedRig(t)
	cfg := r.config.Load()
	reloadFixtureCloud(t, cfg)
	store, dir := reloadFixtureStore(t, cfg)
	r.m.ReloadConfig(store.Config())
	entered, unblock := holdFirstReload(t, store, r.m.ReloadConfig)
	var wg sync.WaitGroup
	defer func() { unblock(); wg.Wait() }()
	cloudPath := filepath.Join(dir, "cloud", "cloud-fixture", "config.yaml")
	oldCloud := reloadFixtureFile(t, dir, "cloud", "cloud-fixture")
	require.NoError(t, os.WriteFile(cloudPath, []byte("unknown_field: true\n"), 0600))
	body := append(reloadFixtureFile(t, dir, "external", "ollama"), []byte("\n# pushed bytes\n")...)
	firstDone := make(chan struct{})
	wg.Go(func() {
		defer close(firstDone)
		_, err := store.WriteProviderFiles("ollama", config.KindExternal, config.ProviderFiles{Config: body})
		assert.Error(t, err)
	})
	waitConfigPublication(t, entered)
	require.Nil(t, store.Config())
	require.NoError(t, os.WriteFile(cloudPath, oldCloud, 0600))
	started, recovered := make(chan struct{}), make(chan struct{})
	wg.Go(func() {
		defer close(recovered)
		close(started)
		_, err := store.Reload()
		assert.NoError(t, err)
	})
	waitConfigPublication(t, started)
	assertConfigPublicationWaiting(t, recovered)
	unblock()
	waitConfigPublication(t, firstDone)
	waitConfigPublication(t, recovered)
	require.NotNil(t, store.Config())
	require.Same(t, store.Config(), r.m.AppsConfig(), "older unavailable state cannot overwrite recovery")
}

func TestConfigPublicationOrdersServiceDisableAndEnable(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	store, _ := reloadFixtureStore(t, m.AppsConfig())
	m.ReloadConfig(store.Config())
	m.Start(context.Background())
	require.Eventually(t, func() bool { return serviceStatus(t, m).Running }, 5*time.Second, 10*time.Millisecond)
	firstPID := serviceStatus(t, m).PID
	entered, unblock := holdFirstReload(t, store, m.ReloadConfig)
	var wg sync.WaitGroup
	defer func() { unblock(); wg.Wait() }()
	disabled := make(chan struct{})
	wg.Go(func() {
		defer close(disabled)
		_, err := store.SetProviderEnabled("generic-daemon", "", false)
		assert.NoError(t, err)
	})
	waitConfigPublication(t, entered)
	started, enabled := make(chan struct{}), make(chan struct{})
	wg.Go(func() {
		defer close(enabled)
		close(started)
		_, err := store.SetProviderEnabled("generic-daemon", "", true)
		assert.NoError(t, err)
	})
	waitConfigPublication(t, started)
	assertConfigPublicationWaiting(t, enabled)
	unblock()
	waitConfigPublication(t, disabled)
	waitConfigPublication(t, enabled)
	require.Same(t, store.Config(), m.AppsConfig(), "older disabled state cannot overwrite the newer enable")
	require.Eventually(t, func() bool { return serviceStatus(t, m).Running }, 5*time.Second, 10*time.Millisecond)
	status := serviceStatus(t, m)
	require.True(t, status.Desired)
	require.NotEqual(t, firstPID, status.PID, "ordered disable and enable must finish both lifecycle effects")
	require.True(t, process.ProcessExists(int32(status.PID)), "the final enabled daemon must physically run")
}
