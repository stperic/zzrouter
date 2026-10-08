package quota

import (
	"context"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"
)

// TestLifecycle_NoGoroutineLeak exercises RateLimiter and SpendTracker
// Start→Stop and fails if persistLoop / reaperLoop / cleanupLoop
// outlive the test. Covers the three goroutines fixed in the
// package-architecture leak-cleanup wave.
func TestLifecycle_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx := context.Background()

	rl := NewRateLimiter()
	rl.Start(ctx)
	rl.Stop(ctx)

	spendPath := filepath.Join(t.TempDir(), "spend.json")
	st := NewSpendTracker(spendPath)
	st.Start(ctx)
	st.Stop(ctx)
}
