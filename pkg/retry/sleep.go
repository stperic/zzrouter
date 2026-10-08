package retry

import (
	"context"
	"time"
)

// Sleep waits for d or until ctx is cancelled, whichever comes first.
// Returns ctx.Err() on cancellation (nil if the timer fires first).
// A non-positive d returns immediately after checking ctx.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
