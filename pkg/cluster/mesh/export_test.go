package mesh

import (
	"errors"
	"testing"
)

// driveToDown drives an endpoint's liveness state to DOWN by feeding
// `livenessThreshold` consecutive failed probes. Test-only helper; the
// production path goes through HealthMonitor.probe → Observe.
func driveToDown(t *testing.T, reg *EndpointRegistry, url string) {
	t.Helper()
	for range livenessThreshold {
		reg.Observe(url, ProbeResult{OK: false, Err: errors.New("test: driveToDown")})
	}
}

// driveToUp drives an endpoint to StatusUp via a single successful probe.
func driveToUp(t *testing.T, reg *EndpointRegistry, url string) {
	t.Helper()
	reg.Observe(url, ProbeResult{OK: true, Quality: QualityFull})
}

// SetTickedChForTest wires a channel that receives after each ticker-
// driven ResourceTracker collection. Must be called before Start —
// the collector goroutine captures the channel reference when it
// runs, and the runtime does not guarantee that a post-Start
// assignment is observable to the running goroutine. Panics if
// the tracker has already started, to catch the misuse loudly.
//
// Sends are non-blocking and coalescing; see the tickedCh doc on
// ResourceTracker for the full contract.
func (rt *ResourceTracker) SetTickedChForTest(ch chan<- struct{}) {
	if rt.started.Load() {
		panic("SetTickedChForTest called after Start")
	}
	rt.tickedCh = ch
}
