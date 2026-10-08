package mesh

import (
	"testing"
	"time"
)

func TestResourceTrackerCreation(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 30*time.Second)

	if tracker == nil {
		t.Fatal("NewResourceTracker returned nil")
	}

	if tracker.nodeName != "test-node" {
		t.Errorf("nodeName = %q, want %q", tracker.nodeName, "test-node")
	}

	if tracker.updateFreq != 30*time.Second {
		t.Errorf("updateFreq = %v, want %v", tracker.updateFreq, 30*time.Second)
	}
}

func TestResourceTrackerDefaultFrequency(t *testing.T) {
	t.Parallel()
	// Zero frequency should default to 30 seconds
	tracker := NewResourceTracker("test-node", 0)

	if tracker.updateFreq != 30*time.Second {
		t.Errorf("updateFreq = %v, want default 30s", tracker.updateFreq)
	}
}

func TestResourceTrackerCollectNow(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 30*time.Second)

	metrics := tracker.CollectNow()

	if metrics == nil {
		t.Fatal("CollectNow returned nil")
	}

	// Verify node name is set
	if metrics.NodeName != "test-node" {
		t.Errorf("NodeName = %q, want %q", metrics.NodeName, "test-node")
	}

	// Verify collection timestamp is recent
	collectedAt := time.Unix(metrics.CollectedAt, 0)
	if time.Since(collectedAt) > 5*time.Second {
		t.Error("CollectedAt is not recent")
	}

	// RAM should always be populated
	if metrics.RAMTotalMB <= 0 {
		t.Error("RAMTotalMB should be > 0")
	}

	if metrics.RAMAvailableMB <= 0 {
		t.Error("RAMAvailableMB should be > 0")
	}

	// RAM available should be <= total
	if metrics.RAMAvailableMB > metrics.RAMTotalMB {
		t.Errorf("RAMAvailableMB (%d) > RAMTotalMB (%d)", metrics.RAMAvailableMB, metrics.RAMTotalMB)
	}
}

func TestResourceTrackerStartStop(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 100*time.Millisecond)

	ctx := t.Context()

	// Start collects initial metrics synchronously before spawning the
	// ticker goroutine; no wait needed to observe the first snapshot.
	tracker.Start(ctx)

	metrics := tracker.GetMetrics()
	if metrics == nil {
		t.Error("GetMetrics returned nil after Start")
	}

	// Stop the tracker
	tracker.Stop()

	// Should still be able to get cached metrics after stop
	metrics = tracker.GetMetrics()
	if metrics == nil {
		t.Error("GetMetrics returned nil after Stop")
	}
}

func TestResourceTrackerSetActiveModels(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 30*time.Second)

	// Collect initial metrics
	tracker.CollectNow()

	// Set active models count
	tracker.SetActiveModels(3)

	metrics := tracker.GetMetrics()
	if metrics == nil {
		t.Fatal("GetMetrics returned nil")
	}

	if metrics.ActiveModels != 3 {
		t.Errorf("ActiveModels = %d, want 3", metrics.ActiveModels)
	}
}

func TestCollectResourceMetricsStandalone(t *testing.T) {
	t.Parallel()
	// Test the standalone function
	metrics := CollectResourceMetrics("standalone-test")

	if metrics == nil {
		t.Fatal("CollectResourceMetrics returned nil")
	}

	if metrics.NodeName != "standalone-test" {
		t.Errorf("NodeName = %q, want %q", metrics.NodeName, "standalone-test")
	}

	// Should have valid RAM metrics
	if metrics.RAMTotalMB <= 0 {
		t.Error("RAMTotalMB should be > 0")
	}
}

func TestResourceMetricsCopy(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 30*time.Second)
	tracker.CollectNow()

	// Get metrics twice
	metrics1 := tracker.GetMetrics()
	metrics2 := tracker.GetMetrics()

	// Should be separate copies
	metrics1.ActiveModels = 99

	if metrics2.ActiveModels == 99 {
		t.Error("GetMetrics should return a copy, not the same reference")
	}
}

func TestResourceTrackerPeriodicUpdate(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 50*time.Millisecond)

	// Buffered capacity 1 so a single tick doesn't block the collector
	// if the test hasn't yet entered the receive.
	ticked := make(chan struct{}, 1)
	tracker.SetTickedChForTest(ticked)

	ctx := t.Context()
	tracker.Start(ctx)
	t.Cleanup(tracker.Stop)

	// Wait for one ticker-driven collection — proves periodic update
	// actually fires without relying on wall-clock sleep.
	select {
	case <-ticked:
	case <-time.After(2 * time.Second):
		t.Fatal("tracker never reported a ticker-driven collection")
	}

	metrics := tracker.GetMetrics()
	if metrics == nil {
		t.Fatal("GetMetrics returned nil")
	}

	collectedAt := time.Unix(metrics.CollectedAt, 0)
	if time.Since(collectedAt) > 2*time.Second {
		t.Errorf("Metrics should be periodically updated, but last collection was %v ago", time.Since(collectedAt))
	}

	if metrics.RAMTotalMB <= 0 {
		t.Error("Expected RAM metrics to be collected during periodic update")
	}
}
