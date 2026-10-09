package instance

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stperic/zzrouter/pkg/utils"
)

func acquireTestLease(t *testing.T, inst *Instance) *Lease {
	t.Helper()
	lease, err := inst.Acquire(context.Background(), nil)
	require.NoError(t, err)
	require.Same(t, inst, lease.Instance())
	t.Cleanup(lease.Release)
	return lease
}

func TestLease_ReleaseExactlyOnce(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 0)
	inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: 0})
	lease, err := inst.Acquire(context.Background(), nil)
	require.NoError(t, err)
	require.Same(t, inst, lease.Instance())
	lease.Release()

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(lease.Release)
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("repeated Release blocked")
	}
	require.Equal(t, int64(0), inst.GetActiveRequests())

	next, err := inst.Acquire(context.Background(), nil)
	require.NoError(t, err, "Release must free the slot")
	defer next.Release()
	blocked, err := inst.Acquire(context.Background(), nil)
	assert.ErrorIs(t, err, ErrQueueFull, "Release must free exactly one slot")
	assert.Nil(t, blocked)
	assert.Equal(t, int64(1), inst.GetActiveRequests())
}

func TestLease_ConcurrentAcquireNeverExceedsCap(t *testing.T) {
	const limit = 3
	inst := NewInstance("id", "p", "m", 0, 0, 0)
	inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: limit, MaxQueuedRequests: 100, QueueTimeout: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var held, maxHeld, failures atomic.Int64
	start := make(chan struct{})
	release := make(chan struct{})
	acquired := make(chan struct{}, 96)
	var wg sync.WaitGroup
	for range 96 {
		wg.Go(func() {
			<-start
			for range 8 {
				lease, err := inst.Acquire(ctx, nil)
				if err != nil {
					failures.Add(1)
					return
				}
				n := held.Add(1)
				for max := maxHeld.Load(); n > max; max = maxHeld.Load() {
					if maxHeld.CompareAndSwap(max, n) {
						break
					}
				}
				select {
				case acquired <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-ctx.Done():
				}
				time.Sleep(time.Millisecond)
				held.Add(-1)
				lease.Release()
			}
		})
	}
	close(start)
	for range limit {
		select {
		case <-acquired:
		case <-ctx.Done():
			t.Error("initial leases did not fill capacity")
		}
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int64(0), failures.Load())
	assert.Equal(t, int64(limit), maxHeld.Load(), "held leases must fill but never exceed capacity")
	assert.Equal(t, int64(0), held.Load())
	assert.Equal(t, int64(0), inst.GetActiveRequests())
}

func TestLease_ReleaseRestampsActivity(t *testing.T) {
	for _, duration := range []time.Duration{time.Minute, 0, -1} {
		t.Run(duration.String(), func(t *testing.T) {
			acquireAt := utils.Now()
			restore := utils.SetClock(utils.FixedClock(acquireAt))
			t.Cleanup(restore)
			inst := NewInstance("id", "p", "m", 0, time.Hour, 0)
			t.Cleanup(inst.StopKeepAliveTimer)
			hints := &keepalive.Override{Duration: &duration}
			lease, err := inst.Acquire(context.Background(), hints)
			require.NoError(t, err)
			t.Cleanup(lease.Release)
			require.Equal(t, acquireAt, inst.GetLastActivity())
			require.Equal(t, duration, inst.GetKeepAlive())

			otherDuration := 2 * time.Hour
			inst.StampActivityWithHints(&keepalive.Override{Duration: &otherDuration})
			releaseAt := acquireAt.Add(time.Second)
			restoreReleaseClock := utils.SetClock(utils.FixedClock(releaseAt))
			t.Cleanup(restoreReleaseClock)
			lease.Release()
			assert.True(t, inst.GetLastActivity().After(acquireAt))
			assert.Equal(t, releaseAt, inst.GetLastActivity())
			assert.Equal(t, releaseAt, inst.GetLastUsedAt())
			assert.Equal(t, otherDuration, inst.GetKeepAlive(), "a later request's keep_alive survives this release")
			assert.Equal(t, int64(0), inst.GetActiveRequests())
		})
	}
}

func TestLease_UnlimitedStillCounts(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 0)
	require.Equal(t, 0, inst.ConcurrencyLimit())
	first := acquireTestLease(t, inst)
	require.Equal(t, int64(1), inst.GetActiveRequests())
	require.False(t, inst.GetLastActivity().IsZero())
	second := acquireTestLease(t, inst)
	require.Equal(t, int64(2), inst.GetActiveRequests())
	first.Release()
	require.Equal(t, int64(1), inst.GetActiveRequests())
	second.Release()
	assert.Equal(t, int64(0), inst.GetActiveRequests())
}

func TestLease_ReleaseRearmsExpiredTimer(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 20*time.Millisecond, 0)
	inst.IdleExpiry = make(chan string, 1)
	t.Cleanup(inst.StopKeepAliveTimer)
	lease := acquireTestLease(t, inst)
	select {
	case <-inst.IdleExpiry:
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not arm expiry")
	}
	require.Equal(t, int64(1), inst.GetActiveRequests())
	lease.Release()
	select {
	case id := <-inst.IdleExpiry:
		assert.Equal(t, inst.ID, id)
	case <-time.After(2 * time.Second):
		t.Fatal("Release did not re-arm expiry after a long request")
	}
}
