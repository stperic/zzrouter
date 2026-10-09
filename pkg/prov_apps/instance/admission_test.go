package instance

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

type leaseResult struct {
	lease *Lease
	err   error
}

func queuedTestAcquire(t *testing.T, inst *Instance, ctx context.Context) <-chan leaseResult {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	result := make(chan leaseResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		lease, err := inst.Acquire(ctx, nil)
		result <- leaseResult{lease, err}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		select {
		case got := <-result:
			if got.lease != nil {
				got.lease.Release()
			}
		default:
		}
	})
	return result
}

func waiterCount(inst *Instance) int {
	inst.queueMu.Lock()
	defer inst.queueMu.Unlock()
	return inst.waiters
}

func TestLease_QueueCapRejectsAtDoor(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 0)
	inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 1, QueueTimeout: time.Second})
	held := acquireTestLease(t, inst)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parked := queuedTestAcquire(t, inst, ctx)
	require.Eventually(t, func() bool { return waiterCount(inst) == 1 }, time.Second, time.Millisecond)

	start := utils.Now()
	refused := queuedTestAcquire(t, inst, ctx)
	select {
	case got := <-refused:
		require.ErrorIs(t, got.err, ErrQueueFull)
		require.Nil(t, got.lease)
		var admission *AdmissionError
		require.ErrorAs(t, got.err, &admission)
		assert.Equal(t, AdmissionRetryAfter, admission.RetryAfter)
		assert.Less(t, time.Since(start), 200*time.Millisecond)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("queue-full Acquire waited instead of refusing at the door")
	}
	assert.Equal(t, 1, waiterCount(inst))
	held.Release()
	got := <-parked
	require.NoError(t, got.err)
	got.lease.Release()
	assert.Zero(t, waiterCount(inst))
}

func TestLease_WaiterCountReturnsOnEveryExit(t *testing.T) {
	for _, exit := range []string{"success", "timeout", "cancel", "caller_deadline"} {
		t.Run(exit, func(t *testing.T) {
			wait := constants.QueueUntilCallerDeadline
			if exit == "timeout" {
				wait = 50 * time.Millisecond
			}
			inst := NewInstance("id", "p", "m", 0, 0, 0)
			inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 1, QueueTimeout: wait})
			held := acquireTestLease(t, inst)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if exit == "caller_deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
			}
			defer cancel()
			result := queuedTestAcquire(t, inst, ctx)
			require.Eventually(t, func() bool { return waiterCount(inst) == 1 }, time.Second, time.Millisecond)
			switch exit {
			case "success":
				held.Release()
			case "cancel":
				cancel()
			}
			got := <-result
			switch exit {
			case "success":
				require.NoError(t, got.err)
				got.lease.Release()
			case "timeout":
				require.ErrorIs(t, got.err, ErrQueueWaitExpired)
			case "cancel":
				require.Equal(t, context.Canceled, got.err)
			case "caller_deadline":
				require.Equal(t, context.DeadlineExceeded, got.err)
			}
			assert.Zero(t, waiterCount(inst))
		})
	}
}

func TestLease_WaitExpires(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 0)
	inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 1, QueueTimeout: 20 * time.Millisecond})
	acquireTestLease(t, inst)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := utils.Now()
	lease, err := inst.Acquire(ctx, nil)
	require.ErrorIs(t, err, ErrQueueWaitExpired)
	assert.Nil(t, lease)
	assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
	var admission *AdmissionError
	require.ErrorAs(t, err, &admission)
	assert.Equal(t, AdmissionRetryAfter, admission.RetryAfter)
	assert.Zero(t, waiterCount(inst))
	assert.EqualValues(t, 1, inst.GetActiveRequests())
}

func TestLease_NoQueueStillUsesFreeSlot(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 0)
	inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, QueueTimeout: time.Second})
	acquireTestLease(t, inst)
	lease, err := inst.Acquire(context.Background(), nil)
	assert.Nil(t, lease)
	assert.ErrorIs(t, err, ErrQueueFull)
	assert.Zero(t, waiterCount(inst))
}
