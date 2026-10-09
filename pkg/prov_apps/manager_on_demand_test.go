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
	m, err := NewProviderAppManager(cfg, WithModelSource(source), WithParamLocalizer(func(_, _, _ string, _ map[string]string) (Localized, error) {
		return Localized{}, errors.New("test refuses before process launch")
	}))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, m.Stop(context.Background())) })
	return m
}

func TestStartOnDemandTwoProvidersOneName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var launches atomic.Int32
		m := onDemandTestManager(t, func(ctx context.Context, _ string) string {
			launches.Add(1)
			<-ctx.Done()
			return ""
		})
		for _, provider := range []string{"fake", "other"} {
			require.NoError(t, m.StartOnDemandAsync(LaunchRequest{Model: "shared", Provider: provider}))
		}
		synctest.Wait()
		assert.Equal(t, int32(2), launches.Load(), "both providers must enter the launch flow")
		require.NoError(t, m.Stop(context.Background()))
		assert.Empty(t, m.instances.ListAll())
	})
}

func TestStartOnDemandDedupesSameProvider(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var launches atomic.Int32
		release := make(chan struct{})
		m := onDemandTestManager(t, func(ctx context.Context, _ string) string {
			launches.Add(1)
			<-release
			return ""
		})
		var callers sync.WaitGroup
		for range 16 {
			callers.Go(func() {
				assert.NoError(t, m.StartOnDemandAsync(LaunchRequest{Model: "shared", Provider: "fake"}))
			})
		}
		callers.Wait()
		synctest.Wait()
		close(release)
		m.onDemandStarts.Wait()
		assert.Equal(t, int32(1), launches.Load())
		require.NoError(t, m.Stop(context.Background()))
	})
}

func TestStartOnDemandCancelledByStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled := make(chan struct{}, 1)
		release := make(chan struct{})
		defer close(release)
		m := onDemandTestManager(t, func(ctx context.Context, _ string) string {
			select {
			case <-ctx.Done():
				cancelled <- struct{}{}
				<-release
			case <-release:
			}
			return ""
		})
		require.NoError(t, m.StartOnDemandAsync(LaunchRequest{Model: "shared", Provider: "fake"}))
		synctest.Wait()
		stopped := make(chan struct{})
		var stopErr error
		go func() { stopErr = m.Stop(context.Background()); close(stopped) }()
		synctest.Wait()
		select {
		case <-cancelled:
		default:
			t.Error("Stop must cancel the in-flight start")
		}
		select {
		case <-stopped:
			t.Error("Stop must wait until the start goroutine exits")
		default:
		}
		release <- struct{}{}
		synctest.Wait()
		<-stopped
		require.NoError(t, stopErr)
		assert.Empty(t, m.instances.ListAll())
		assert.ErrorIs(t, m.StartOnDemandAsync(LaunchRequest{Model: "later", Provider: "fake"}), ErrShutdown)
	})
}

func TestStartOnDemandImmediateRefusals(t *testing.T) {
	m := onDemandTestManager(t, nil)
	assert.ErrorIs(t, m.StartOnDemandAsync(LaunchRequest{Model: "m", Provider: "missing"}), ErrProviderNotFound)
	m.validateModel = func(context.Context, string) error { return config.ErrModelNameConflict }
	assert.ErrorIs(t, m.StartOnDemandAsync(LaunchRequest{Model: "m", Provider: "fake"}), config.ErrModelNameConflict)
}

func TestStartOnDemandStopPreservesAuditDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		m := onDemandTestManager(t, func(ctx context.Context, _ string) string {
			<-ctx.Done()
			<-release
			return ""
		})
		m.Start(t.Context())
		require.NoError(t, m.StartOnDemandAsync(LaunchRequest{Model: "shared", Provider: "fake"}))
		synctest.Wait()
		stopped := make(chan error, 1)
		go func() { stopped <- m.Stop(context.Background()) }()
		synctest.Wait()
		time.Sleep(2 * eventDrainWindow)
		m.emitEvent(Event{Type: EventInstanceStopped, Provider: "fake", Model: "shared"})
		synctest.Wait()
		assert.Empty(t, m.auditEvents, "audit consumer must remain alive while Stop drains starts")
		close(release)
		synctest.Wait()
		require.NoError(t, <-stopped)
		time.Sleep(2 * eventDrainWindow)
		synctest.Wait()
	})
}
