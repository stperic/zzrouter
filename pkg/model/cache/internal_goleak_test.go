package cache

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

// TestLifecycle_Cache_NoGoroutineLeak exercises Cache Start→Stop and
// fails if the lifecycle/refresh goroutines or in-flight notify
// goroutines outlive the test. Uses the existing newWorkerCache
// fixture to match what cache_lifecycle_test.go exercises.
func TestLifecycle_Cache_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx := context.Background()
	c := newWorkerCache(t)
	c.Start(ctx)
	c.Stop(ctx)
}
