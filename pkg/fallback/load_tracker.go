package fallback

import "sync"

// LoadTracker tracks in-flight requests per deployment name.
// Used by the least-load strategy to route to the least busy deployment.
//
// A nil *LoadTracker is a valid "no tracking configured" value and every
// method tolerates it. Callers hold the tracker as a concrete pointer but
// pass it as an interface (loadSnapshotter), and a nil pointer boxed in an
// interface is not == nil — so a `if loads == nil` guard at the call site
// does not fire and the method runs on a nil receiver.
type LoadTracker struct {
	mu       sync.RWMutex
	inflight map[string]int64
}

// NewLoadTracker creates a new load tracker.
func NewLoadTracker() *LoadTracker {
	return &LoadTracker{
		inflight: make(map[string]int64),
	}
}

// Acquire increments the in-flight count for the given deployment.
func (t *LoadTracker) Acquire(deploymentName string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.inflight[deploymentName]++
	t.mu.Unlock()
}

// Release decrements the in-flight count for the given deployment.
func (t *LoadTracker) Release(deploymentName string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.inflight[deploymentName] > 0 {
		t.inflight[deploymentName]--
	}
	if t.inflight[deploymentName] == 0 {
		delete(t.inflight, deploymentName)
	}
	t.mu.Unlock()
}

// InFlight returns the current in-flight count for a deployment.
// A nil tracker reports 0, i.e. "nothing known to be in flight".
func (t *LoadTracker) InFlight(deploymentName string) int64 {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.inflight[deploymentName]
}
