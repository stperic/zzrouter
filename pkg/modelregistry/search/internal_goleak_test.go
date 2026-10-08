package search

import (
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestLifecycle_NoGoroutineLeak exercises SearchCache Start→Stop and
// fails if the cleanupLoop goroutine outlives the test. If it fires
// after a future edit, check that Stop still calls wg.Wait after
// close(stopCh).
func TestLifecycle_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	c := NewSearchCache(50 * time.Millisecond)
	c.Start()
	c.Set("example", "value")
	c.Stop()
}
