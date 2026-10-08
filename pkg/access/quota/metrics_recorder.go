package quota

// MetricsRecorder is the quota package's outbound observability hook.
// The package itself takes no dependency on OpenTelemetry or Prometheus;
// the concrete implementation lives in
// pkg/observability/quotametrics and is wired at startup. A Null
// recorder is the zero-value so tests and boot paths that haven't
// configured observability behave identically.
//
// All methods are called on the hot path of quota enforcement;
// implementations must return quickly and must not block. Label
// cardinality is deliberately low — `scope` takes only the strings
// "key" or "team", `reason` is a fixed set of the Decision.Reason
// values — so the series count is bounded regardless of tenant count.
type MetricsRecorder interface {
	// ObserveDecision records one quota decision (one call to
	// Enforcer.Check or AcquireConcurrency). `outcome` is "allowed"
	// or "denied"; `reason` is the Decision.Reason string on denials,
	// empty on allows.
	ObserveDecision(scope ScopeKind, outcome, reason string)

	// ObserveSpendSettlement records actual-cost settlement of a
	// reserved budget entry (SettleReservations path). `usd` is the
	// settled amount.
	ObserveSpendSettlement(scope ScopeKind, usd float64)

	// ObserveConcurrencyAcquire increments the active-concurrency
	// gauge for scope.
	ObserveConcurrencyAcquire(scope ScopeKind)

	// ObserveConcurrencyRelease decrements the active-concurrency
	// gauge for scope.
	ObserveConcurrencyRelease(scope ScopeKind)
}

// NullMetricsRecorder is a MetricsRecorder that drops every call.
// Zero-value Enforcer uses this so a partially-wired test harness
// does not crash on recording.
type NullMetricsRecorder struct{}

// ObserveDecision is a no-op.
func (NullMetricsRecorder) ObserveDecision(_ ScopeKind, _, _ string) {}

// ObserveSpendSettlement is a no-op.
func (NullMetricsRecorder) ObserveSpendSettlement(_ ScopeKind, _ float64) {}

// ObserveConcurrencyAcquire is a no-op.
func (NullMetricsRecorder) ObserveConcurrencyAcquire(_ ScopeKind) {}

// ObserveConcurrencyRelease is a no-op.
func (NullMetricsRecorder) ObserveConcurrencyRelease(_ ScopeKind) {}
