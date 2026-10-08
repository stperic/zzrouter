package quota

import "time"

// BreachEvent carries every quota-significant signal an agent might
// want to react to: outright denials (rpm/tpm/budget/concurrency
// exceeded) AND informational budget-threshold crossings.
//
// Reason vocabulary (closed enum):
//   - rpm_limit_exceeded
//   - tpm_limit_exceeded
//   - budget_exhausted
//   - concurrency_limit_exceeded
//   - budget_threshold_50  (informational; fires once per period at 50% used)
//   - budget_threshold_80  (informational; fires once per period at 80% used)
//   - budget_threshold_95  (informational; fires once per period at 95% used)
//
// Spend-envelope semantics differ slightly between the two budget
// event flavors:
//   - budget_exhausted: SpendUsedMicro is the already-settled spend
//     captured immediately BEFORE the failed reservation lands; the
//     denied estimate is not added.
//   - budget_threshold_*: SpendUsedMicro and SpendHeldMicro are the
//     post-settle snapshot captured under the same lock that claimed
//     the threshold bit, so used+held equals the basis the
//     percentage was claimed against.
//
// ThresholdPercent populates only on threshold events so an agent
// can branch on `event.threshold_percent > 0`.
type BreachEvent struct {
	Time             time.Time `json:"time"`
	Scope            ScopeKind `json:"scope"`    // "key" | "team"
	ScopeID          string    `json:"scope_id"` // the key ID or team ID
	Reason           string    `json:"reason"`
	SpendLimitMicro  int64     `json:"spend_limit_micro,omitempty"`
	SpendUsedMicro   int64     `json:"spend_used_micro,omitempty"`
	SpendHeldMicro   int64     `json:"spend_held_micro,omitempty"`
	BudgetPeriod     string    `json:"budget_period,omitempty"`
	ThresholdPercent uint8     `json:"threshold_percent,omitempty"` // populated on budget_threshold_* events
}

// Implementations MUST be non-blocking — fires on the quota hot path.
type BreachObserver interface {
	OnBreach(event BreachEvent)
}

// NullBreachObserver is the zero-value observer; drops every event.
type NullBreachObserver struct{}

// OnBreach is a no-op.
func (NullBreachObserver) OnBreach(_ BreachEvent) {}

// ThresholdReason returns the canonical reason string for a budget
// threshold event at the given percentage. Returns "" for unknown
// percentages so callers can guard with a length check.
func ThresholdReason(pct uint8) string {
	switch pct {
	case 50:
		return "budget_threshold_50"
	case 80:
		return "budget_threshold_80"
	case 95:
		return "budget_threshold_95"
	default:
		return ""
	}
}
