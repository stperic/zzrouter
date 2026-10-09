package instance

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/constants"
)

func TestAcquire_QueuePolicy(t *testing.T) {
	t.Run("zero timeout rejects at the door", func(t *testing.T) {
		i := NewInstance("id", "mlx", "m", 0, 0, 0)
		i.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: 0})
		acquireTestLease(t, i)
		lease, err := i.Acquire(context.Background(), nil)
		assert.ErrorIs(t, err, ErrQueueFull)
		assert.Nil(t, lease)
	})

	t.Run("a bounded wait takes the slot", func(t *testing.T) {
		i := NewInstance("id", "mlx", "m", 0, 0, 0)
		i.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: constants.DefaultQueueWait})
		first := acquireTestLease(t, i)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		acquired := make(chan *Lease, 1)
		go func() {
			lease, err := i.Acquire(ctx, nil)
			assert.NoError(t, err)
			acquired <- lease
		}()

		select {
		case lease := <-acquired:
			if lease != nil {
				lease.Release()
			}
			t.Fatal("second acquire returned early; it must queue")
		case <-time.After(50 * time.Millisecond):
		}

		first.Release()
		lease := <-acquired
		require.NotNil(t, lease, "queued acquire must wake once the slot frees")
		lease.Release()
	})

	t.Run("the caller's deadline is the bound", func(t *testing.T) {
		i := NewInstance("id", "mlx", "m", 0, 0, 0)
		i.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: constants.QueueUntilCallerDeadline})
		acquireTestLease(t, i)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		lease, err := i.Acquire(ctx, nil)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Nil(t, lease)
	})
}
