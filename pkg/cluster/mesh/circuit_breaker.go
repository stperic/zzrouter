package mesh

// The circuit breaker implementation lives in pkg/fallback so cluster,
// connectivity, and any future consumer share one tested code path. This
// file preserves the cluster-package surface that current callers actually
// use, as type aliases over the unified types. New code should depend on
// pkg/fallback directly.

import "github.com/stperic/zzrouter/pkg/fallback"

// CircuitBreakerState aliases fallback.BreakerState. Existing callers
// reference cluster.StateOpen / StateClosed / StateHalfOpen below.
type CircuitBreakerState = fallback.BreakerState

const (
	StateClosed   = fallback.BreakerClosed
	StateOpen     = fallback.BreakerOpen
	StateHalfOpen = fallback.BreakerHalfOpen
)

// CircuitBreaker and CircuitBreakerManager are aliases of the fallback
// implementation. The aliases keep cluster's public API stable while the
// behavior is unified.
type (
	CircuitBreaker        = fallback.Breaker
	CircuitBreakerManager = fallback.BreakerManager
	CircuitBreakerConfig  = fallback.BreakerConfig
)

// NewCircuitBreakerManager constructs a manager with default thresholds
// (3 failures → open, 30s timeout, 2 successes → close).
func NewCircuitBreakerManager() *CircuitBreakerManager {
	return fallback.NewBreakerManager()
}
