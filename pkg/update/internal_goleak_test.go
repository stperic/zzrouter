package update

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils/clock"
)

// TestLifecycle_Scheduler_NoGoroutineLeak exercises the Scheduler
// Start→Stop contract and fails if the schedulerLoop goroutine
// outlives the test. Mirrors TestScheduler_Start_AndStop but with a
// goleak check — catches any regression of the Stop-drops-lock-then-
// wg.Wait pattern that bit the scheduler once already.
func TestLifecycle_Scheduler_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	enabled := true
	cfg := &config.UpdateConfig{
		Enabled:            &enabled,
		CheckIntervalHours: 1,
	}
	s := NewScheduler(cfg, clock.System())

	ctx := context.Background()
	s.Start(ctx)
	// Give the goroutine a moment to park in its select. Without this
	// Stop can race Start and close stopChan before schedulerLoop
	// enters the select — which is fine for shutdown, but less
	// representative of the real-life race goleak is guarding against.
	time.Sleep(20 * time.Millisecond)
	s.Stop()
}
