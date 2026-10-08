package quota

import "sync"

// ConcurrencyLimiter tracks in-flight requests per scope.
// Keyed by ThrottleScope to distinguish key-level from team-level.
type ConcurrencyLimiter struct {
	mu       sync.RWMutex
	inflight map[ThrottleScope]int
}

// NewConcurrencyLimiter creates a new concurrency limiter.
func NewConcurrencyLimiter() *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		inflight: make(map[ThrottleScope]int),
	}
}

// Acquire increments the in-flight count for the scope.
// Returns true if under the limit, false if the limit would be exceeded.
// A limit of 0 means unlimited.
func (cl *ConcurrencyLimiter) Acquire(scope ThrottleScope, limit int) bool {
	if limit <= 0 {
		return true
	}

	cl.mu.Lock()
	defer cl.mu.Unlock()

	if cl.inflight[scope] >= limit {
		return false
	}

	cl.inflight[scope]++
	return true
}

// Release decrements the in-flight count for the scope.
func (cl *ConcurrencyLimiter) Release(scope ThrottleScope) {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	if cl.inflight[scope] > 0 {
		cl.inflight[scope]--
	}
	if cl.inflight[scope] == 0 {
		delete(cl.inflight, scope)
	}
}

// InFlight returns the current in-flight count for a scope.
func (cl *ConcurrencyLimiter) InFlight(scope ThrottleScope) int {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	return cl.inflight[scope]
}
