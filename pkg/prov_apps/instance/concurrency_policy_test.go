package instance

import (
	"context"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
)

func TestAcquireConcurrency_QueuePolicy(t *testing.T) {
	t.Run("zero timeout rejects at the door", func(t *testing.T) {
		i := NewInstance("id", "mlx", "m", 0, 0, 0)
		i.InitConcurrencyLimit(1, 0)
		if err := i.AcquireConcurrency(context.Background()); err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		if err := i.AcquireConcurrency(context.Background()); err != ErrAtCapacity {
			t.Errorf("second acquire = %v, want ErrAtCapacity", err)
		}
	})

	t.Run("the default waits for the slot and takes it", func(t *testing.T) {
		i := NewInstance("id", "mlx", "m", 0, 0, 0)
		i.InitConcurrencyLimit(1, constants.QueueUntilCallerDeadline)
		if err := i.AcquireConcurrency(context.Background()); err != nil {
			t.Fatalf("first acquire: %v", err)
		}

		acquired := make(chan error, 1)
		go func() { acquired <- i.AcquireConcurrency(context.Background()) }()

		select {
		case err := <-acquired:
			t.Fatalf("second acquire returned early with %v; it must queue", err)
		case <-time.After(50 * time.Millisecond):
		}

		i.ReleaseConcurrency()
		select {
		case err := <-acquired:
			if err != nil {
				t.Errorf("queued acquire = %v, want nil once the slot frees", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("queued acquire never woke after the slot freed")
		}
	})

	t.Run("the caller's deadline is the bound", func(t *testing.T) {
		i := NewInstance("id", "mlx", "m", 0, 0, 0)
		i.InitConcurrencyLimit(1, constants.QueueUntilCallerDeadline)
		if err := i.AcquireConcurrency(context.Background()); err != nil {
			t.Fatalf("first acquire: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := i.AcquireConcurrency(ctx); err != context.DeadlineExceeded {
			t.Errorf("acquire = %v, want DeadlineExceeded when the caller gives up", err)
		}
	})
}
