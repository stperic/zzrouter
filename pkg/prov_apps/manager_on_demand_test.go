package prov_apps

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errTestStart = errors.New("test refuses before process launch")

func onDemandTestManager(t *testing.T, source func(context.Context, string) string) *ProviderAppManager {
	t.Helper()
	cfg := aliasTestConfig(t)
	second, ok := cfg.LookupApp("fake")
	require.True(t, ok)
	second.Name = "other"
	runtimeConfig := *second.Runtime
	second.Runtime = &runtimeConfig
	second.Runtime.PortRange = []int{8210, 8215}
	second.Runtime.BasePort = 8210
	require.NoError(t, cfg.AddApp("other", second))
	m, err := NewProviderAppManager(cfg, WithModelSource(source), WithParamLocalizer(func(_, _, _ string, _ map[string]string) (Localized, error) { return Localized{}, errTestStart }))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, m.Stop(context.Background())) })
	return m
}

func TestEnsureRunDifferentModelsStartConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var launches atomic.Int32
		release := make(chan struct{})
		m := onDemandTestManager(t, func(context.Context, string) string { launches.Add(1); <-release; return "" })
		var callers sync.WaitGroup
		for _, provider := range []string{"fake", "other"} {
			callers.Go(func() {
				_, err := m.EnsureRun(t.Context(), EnsureRequest{Model: provider, Provider: provider})
				assert.ErrorIs(t, err, errTestStart)
			})
		}
		synctest.Wait()
		assert.Equal(t, int32(2), launches.Load())
		close(release)
		callers.Wait()
	})
}

func TestEnsureRunDedupesSameModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var launches atomic.Int32
		release := make(chan struct{})
		m := onDemandTestManager(t, nil)
		m.localCatalog = func(context.Context, string) (bool, error) { launches.Add(1); <-release; return true, nil }
		var callers sync.WaitGroup
		for range 16 {
			callers.Go(func() {
				_, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "shared", Provider: "fake"})
				assert.ErrorIs(t, err, errTestStart)
			})
		}
		synctest.Wait()
		close(release)
		callers.Wait()
		assert.Equal(t, int32(1), launches.Load())
	})
}

func TestEnsureRunCancelledByStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled, release := make(chan struct{}), make(chan struct{})
		m := onDemandTestManager(t, func(ctx context.Context, _ string) string { <-ctx.Done(); close(cancelled); <-release; return "" })
		started := make(chan error, 1)
		go func() {
			_, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "shared", Provider: "fake"})
			started <- err
		}()
		synctest.Wait()
		stopped := make(chan error, 1)
		go func() { stopped <- m.Stop(context.Background()) }()
		synctest.Wait()
		select {
		case <-cancelled:
		default:
			t.Error("Stop must cancel the start")
		}
		select {
		case <-stopped:
			t.Error("Stop must drain the start")
		default:
		}
		close(release)
		require.NoError(t, <-stopped)
		require.Error(t, <-started)
		assert.Empty(t, m.instances.List())
		_, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "later", Provider: "fake"})
		assert.ErrorIs(t, err, ErrShutdown)
	})
}

func TestEnsureRunImmediateRefusals(t *testing.T) {
	m := onDemandTestManager(t, nil)
	_, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "m", Provider: "missing"})
	assert.ErrorIs(t, err, ErrProviderNotFound)
	m.validateModel = func(context.Context, string) error { return config.ErrModelNameConflict }
	_, err = m.EnsureRun(t.Context(), EnsureRequest{Model: "m", Provider: "fake"})
	assert.ErrorIs(t, err, config.ErrModelNameConflict)
}

func TestEnsureRunStopPreservesAuditDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		m := onDemandTestManager(t, func(ctx context.Context, _ string) string { <-ctx.Done(); <-release; return "" })
		m.Start(t.Context())
		started := make(chan error, 1)
		go func() {
			_, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "shared", Provider: "fake"})
			started <- err
		}()
		synctest.Wait()
		stopped := make(chan error, 1)
		go func() { stopped <- m.Stop(context.Background()) }()
		synctest.Wait()
		time.Sleep(2 * eventDrainWindow)
		m.emitEvent(Event{Type: EventInstanceStopped, Provider: "fake", Model: "shared"})
		synctest.Wait()
		assert.Empty(t, m.auditEvents)
		close(release)
		require.NoError(t, <-stopped)
		require.Error(t, <-started)
		time.Sleep(2 * eventDrainWindow)
	})
}
