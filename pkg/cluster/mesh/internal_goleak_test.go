package mesh

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestLifecycle_ResourceTracker_NoGoroutineLeak exercises
// ResourceTracker Start→Stop and fails if the runCollector goroutine
// outlives the test.
func TestLifecycle_ResourceTracker_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	rt := NewResourceTracker("test-node", 1*time.Hour) // Long interval → no tick fires during test
	rt.Start(context.Background())
	rt.Stop()
}
