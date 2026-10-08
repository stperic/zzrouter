// Package fallback provides fallback routing for model groups.
// When a deployment fails with a retriable error, the dispatcher
// automatically tries the next deployment in priority order.
//
// # Components
//
// Tracker types (each independent, each holds its own mutex):
//
//   - [Breaker]           — circuit breaker, open/half-open/closed state
//   - [CooldownManager]   — per-deployment cooldown window with
//     background cleanup goroutine
//   - [HealthChecker]     — HTTP probe loop per target, drives
//     healthy/unhealthy flag
//   - [LatencyTracker]    — EWMA p50/p99 per deployment
//   - [LoadTracker]       — in-flight request count per deployment
//   - [RateLimitTracker]  — sliding-window rate-limit counters
//
// Strategy types ([Fastest], [LeastLoad], [Priority]) compose the
// trackers read-only; they do not hold locks.
//
// # Lock Ordering
//
// Six struct-field mutexes, one per tracker. The trackers are
// **independent** — no code path acquires a lock on one tracker while
// holding a lock on another. The dispatcher calls them sequentially
// (check cooldown → check health → choose by strategy → record
// latency / load on return). If a new path crosses trackers under a
// held lock, declare the order explicitly here first.
//
// # Lifecycle
//
// [CooldownManager] and [HealthChecker] spawn background goroutines
// on Start and block on their own WaitGroups in Stop. Other trackers
// are passive (no goroutines).
package fallback

import "time"

// Attempt records the result of trying a single deployment.
type Attempt struct {
	// DeploymentName identifies which deployment was tried.
	DeploymentName string

	// StatusCode is the HTTP status from the backend (0 if connection failed).
	StatusCode int

	// Err is the error if the attempt failed at the transport level.
	Err error

	// Duration is how long this attempt took.
	Duration time.Duration

	// Outcome is a short label: "success", "retriable", "non_retriable", "cooldown_skip", "on_demand_skip".
	Outcome string
}

// Outcome constants for Attempt.
const (
	OutcomeSuccess      = "success"
	OutcomeRetriable    = "retriable"
	OutcomeNonRetriable = "non_retriable"
	OutcomeCooldownSkip = "cooldown_skip"
	OutcomeOnDemandSkip = "on_demand_skip"
)
