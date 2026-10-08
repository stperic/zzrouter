package harness

import (
	"context"
	"time"
)

// PollUntil invokes probe at interval until it returns done=true, a
// non-nil terminal error, or ctx fires. Terminal errors short-circuit
// the loop and propagate as-is so callers can errors.Is/As. The first
// probe runs immediately; subsequent probes wait for either interval
// or ctx.Done.
//
// Use this instead of inline `for time.Now().Before(deadline)` loops:
// it folds deadline + cancellation + terminal-failure handling into one
// shape so a probe func that detects an unrecoverable state (e.g.
// deployment status=failed) bails fast instead of ticking out.
func PollUntil(ctx context.Context, interval time.Duration, probe func(context.Context) (done bool, terminal error)) error {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	for {
		done, err := probe(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
