package fallback

import (
	"sync"
	"time"
)

// LatencyTracker maintains a rolling window of request latencies per deployment.
// Used by the fastest strategy to route to the lowest-latency deployment.
//
// A nil *LatencyTracker is a valid "no tracking configured" value and every
// method tolerates it. Callers hold the tracker as a concrete pointer but
// pass it as an interface (LatencySnapshotter), and a nil pointer boxed in
// an interface is not == nil — so a `if latency == nil` guard at the call
// site does not fire and the method runs on a nil receiver.
type LatencyTracker struct {
	mu       sync.RWMutex
	windows  map[string]*latencyWindow
	capacity int
}

type latencyWindow struct {
	samples []time.Duration
	head    int
	count   int
}

// NewLatencyTracker creates a tracker with the given window size per deployment.
func NewLatencyTracker(windowSize int) *LatencyTracker {
	if windowSize <= 0 {
		windowSize = 100
	}
	return &LatencyTracker{
		windows:  make(map[string]*latencyWindow),
		capacity: windowSize,
	}
}

// Record adds a latency sample for the given deployment.
func (t *LatencyTracker) Record(deploymentName string, latency time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	w, ok := t.windows[deploymentName]
	if !ok {
		w = &latencyWindow{
			samples: make([]time.Duration, t.capacity),
		}
		t.windows[deploymentName] = w
	}

	w.samples[w.head] = latency
	w.head = (w.head + 1) % t.capacity
	if w.count < t.capacity {
		w.count++
	}
}

// AverageLatency returns the average latency for a deployment.
// Returns 0 if no samples exist (caller should treat as "no data").
func (t *LatencyTracker) AverageLatency(deploymentName string) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	w, ok := t.windows[deploymentName]
	if !ok || w.count == 0 {
		return 0
	}

	var total time.Duration
	for i := 0; i < w.count; i++ {
		total += w.samples[i]
	}
	return total / time.Duration(w.count)
}

// HasData returns true if at least one latency sample exists for the deployment.
func (t *LatencyTracker) HasData(deploymentName string) bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	w, ok := t.windows[deploymentName]
	return ok && w.count > 0
}

// Snapshot returns the average latency and current sample count for
// the deployment in a single RLock window. Used by the /state endpoint
// so a polling agent doesn't take the tracker mutex twice per replica.
// Returns (0, 0) when no samples exist.
func (t *LatencyTracker) Snapshot(deploymentName string) (time.Duration, int) {
	if t == nil {
		return 0, 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	w, ok := t.windows[deploymentName]
	if !ok || w.count == 0 {
		return 0, 0
	}
	var total time.Duration
	for i := 0; i < w.count; i++ {
		total += w.samples[i]
	}
	return total / time.Duration(w.count), w.count
}
