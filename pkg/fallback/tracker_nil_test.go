package fallback

import "testing"

// TestTrackersTolerateNilReceiver reproduces the typed-nil panic: the
// preview path stores the trackers as concrete pointers and passes them
// as interfaces, so a nil tracker arrives as a non-nil interface holding
// a nil pointer and the call-site `== nil` guard cannot catch it.
func TestTrackersTolerateNilReceiver(t *testing.T) {
	var loads *LoadTracker
	var latency *LatencyTracker

	// Boxed into interfaces exactly as the preview controller does.
	var loadIface interface {
		InFlight(string) int64
	} = loads
	var latIface LatencySnapshotter = latency

	if got := loadIface.InFlight("r1"); got != 0 {
		t.Errorf("InFlight on nil tracker = %d, want 0", got)
	}
	avg, samples := latIface.Snapshot("r1")
	if avg != 0 || samples != 0 {
		t.Errorf("Snapshot on nil tracker = (%v, %d), want (0, 0)", avg, samples)
	}

	// Write paths must be inert rather than panicking too.
	loads.Acquire("r1")
	loads.Release("r1")
	latency.Record("r1", 1)
	if latency.HasData("r1") {
		t.Error("HasData on nil tracker = true, want false")
	}
	if latency.AverageLatency("r1") != 0 {
		t.Error("AverageLatency on nil tracker != 0")
	}
}
