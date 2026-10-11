package instance

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdleStopSerializesWithLease(t *testing.T) {
	for range 100 {
		inst := NewInstance("test", "test", "test", 0, -1, 10)
		inst.SetStatus(StatusRunning)
		start := make(chan struct{})
		claimed := make(chan bool, 1)
		released := make(chan func(), 1)
		activity := inst.GetLastActivity()
		acquired := make(chan *Lease, 1)
		go func() {
			<-start
			release, ok := ClaimIdleStops([]IdleStopCandidate{{Instance: inst, Activity: activity}})
			claimed <- ok
			released <- release
		}()
		go func() {
			<-start
			lease, _ := inst.Acquire(t.Context(), nil)
			acquired <- lease
		}()
		close(start)
		stopping, lease := <-claimed, <-acquired
		assert.False(t, stopping && lease != nil, "idle-stop and a lease cannot both win")
		assert.True(t, stopping || lease != nil)
		if lease != nil {
			lease.Release()
		}
		if release := <-released; release != nil {
			release()
		}
		inst.Cancel()
	}
}

func TestIdleStopReturnsRefusedLeaseSlot(t *testing.T) {
	inst := NewInstance("test", "test", "test", 0, -1, 10)
	inst.SetStatus(StatusRunning)
	inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 1, QueueTimeout: time.Second})
	release, claimed := ClaimIdleStops([]IdleStopCandidate{{Instance: inst, Activity: inst.GetLastActivity()}})
	require.True(t, claimed)
	defer release()
	lease, err := inst.Acquire(t.Context(), nil)
	// A refusal, not a cancellation: the fallback chain reads context.Canceled as the client gone.
	var admission *AdmissionError
	require.ErrorAs(t, err, &admission)
	require.ErrorIs(t, err, ErrStopping)
	assert.NotErrorIs(t, err, context.Canceled)
	assert.Nil(t, lease)
	assert.Empty(t, inst.concurrencySem, "rejected acquisition must return its slot")
	assert.Zero(t, inst.GetActiveRequests())
	inst.Cancel()
}

func TestIdleStopRejectsRefreshedActivity(t *testing.T) {
	inst := NewInstance("test", "test", "test", 0, 0, 10)
	inst.SetStatus(StatusRunning)
	activity := inst.GetLastActivity()
	lease, err := inst.Acquire(t.Context(), nil)
	require.NoError(t, err)
	lease.Release()
	release, claimed := ClaimIdleStops([]IdleStopCandidate{{Instance: inst, Activity: activity}})
	assert.False(t, claimed, "a stale expiry observation cannot stop fresh activity")
	assert.Nil(t, release)
	assert.True(t, inst.Evictable())
	inst.Cancel()
}

func TestIdleStopClaimsAllOrNothing(t *testing.T) {
	first := NewInstance("first", "test", "first", 0, -1, 10)
	second := NewInstance("second", "test", "second", 0, -1, 10)
	first.SetStatus(StatusRunning)
	second.SetStatus(StatusRunning)
	claims := []IdleStopCandidate{{Instance: first, Activity: first.GetLastActivity()}, {Instance: second, Activity: second.GetLastActivity()}}
	lease, err := second.Acquire(t.Context(), nil)
	require.NoError(t, err)
	defer lease.Release()
	release, claimed := ClaimIdleStops(claims)
	assert.False(t, claimed)
	assert.Nil(t, release)
	assert.True(t, first.Evictable(), "a failed set claim must leave other candidates available")
	assert.False(t, first.idleStopClaimed)
	first.Cancel()
	second.Cancel()
}

func TestIdleStopReleaseAllowsLease(t *testing.T) {
	inst := NewInstance("test", "test", "test", 0, -1, 10)
	inst.SetStatus(StatusRunning)
	release, claimed := ClaimIdleStops([]IdleStopCandidate{{Instance: inst, Activity: inst.GetLastActivity()}})
	require.True(t, claimed)
	_, err := inst.Acquire(t.Context(), nil)
	assert.ErrorIs(t, err, ErrStopping)
	release()
	lease, err := inst.Acquire(t.Context(), nil)
	require.NoError(t, err)
	release()
	assert.Equal(t, int64(1), inst.GetActiveRequests(), "release is idempotent")
	lease.Release()
	inst.Cancel()
}

func TestUnhealthyIdleRunIsEvictable(t *testing.T) {
	inst := NewInstance("test", "test", "test", 0, -1, 10)
	inst.SetStatus(StatusUnhealthy)
	assert.True(t, inst.Evictable(), "the reaper must still stop an idle unhealthy run")
	inst.SetStatus(StatusStarting)
	assert.False(t, inst.Evictable())
	inst.Cancel()
}
