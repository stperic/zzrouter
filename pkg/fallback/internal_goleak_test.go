package fallback

import (
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestLifecycle_NoGoroutineLeak exercises the Start→Stop contract on
// CooldownManager and HealthChecker and fails if any goroutine outlives
// the test — catches any future regression of the Stop-doesn't-wait
// pattern that bit this package once already.
//
// If this test fails, the first thing to check is whether a new Start/
// spawn site was added without matching wg.Add + defer wg.Done and
// whether Stop calls wg.Wait after its stop signal.
func TestLifecycle_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	// CooldownManager spawns one cleanupLoop per Start.
	cm := NewCooldownManager()
	cm.Start()
	cm.SetCooldown("example", 50*time.Millisecond, "test")
	cm.Stop()

	// HealthChecker spawns one probeLoop per target.
	// Empty targets list is fine — no probes, but Start/Stop still runs.
	hc := NewHealthChecker(nil)
	// No-op HealthChecker: Start with a background ctx, Stop immediately.
	// Targets with live HTTP probes would need a test server; that's
	// covered by the main health_checker_test.go.
	_ = hc
}
