package clusternode

import (
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestLifecycle_PairingStore_NoGoroutineLeak exercises PairingStore
// Stop and fails if the gcLoop goroutine outlives the test.
// PairingStore already tracks gcLoop on stopWg and Stop calls
// stopWg.Wait — this test guards against a regression.
func TestLifecycle_PairingStore_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	s := NewPairingStore(500 * time.Millisecond)
	s.Stop()
}
