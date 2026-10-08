package prov_apps

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestLifecycle_ProviderAppManager_NoGoroutineLeak exercises the
// Manager's Start→Stop lifecycle and verifies no owned goroutine
// outlives the test.
//
// The Manager spawns two long-lived goroutines on Start — eventLoop
// and idleReaper. Both exit on `<-shutdownCtx.Done()`. idleReaper
// returns immediately; eventLoop enters a bounded drain window
// (eventDrainWindow = 100ms, see manager.go) so late audit-event
// emitters aren't dropped. We sleep past that window before calling
// VerifyNone so the test reflects the package's actual shutdown
// contract — fire-and-forget drain, not join-on-Stop.
//
// Per-instance goroutines (runInstanceLifecycle, watchReadiness,
// healthMonitor) are tracked on Instance.wg and joined by
// StopInstance, which Manager.Stop calls for each running instance.
// No instances are started here, so that path isn't exercised —
// cover it in manager_test.go lifecycle tests rather than in this
// goleak guard.
func TestLifecycle_ProviderAppManager_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	m, err := NewProviderAppManager(testAppsConfig())
	if err != nil {
		t.Fatalf("NewProviderAppManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Wait past eventDrainWindow (100ms) + a small safety margin so
	// eventLoop has exited its drain select before VerifyNone runs.
	time.Sleep(200 * time.Millisecond)
}
