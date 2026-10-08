// Package quota provides entity-agnostic quota enforcement: rate limiting,
// concurrency limiting, and spend tracking with budget reservation.
// It never imports keys or teams — callers build QuotaScope chains and
// pass them to the Enforcer.
//
// # Lock Ordering
//
// Six struct-field mutexes live on three independent subsystems:
//
//   - [RateLimiter]         — rpmMu, tpmMu, lifecycleMu
//   - [SpendTracker]        — mu, lifecycleMu
//   - [ConcurrencyLimiter]  — mu
//
// The subsystems are independent: no function in this package acquires a
// lock on one subsystem while holding a lock on another. The [Enforcer]
// calls them sequentially (reservation → concurrency → spend settle),
// never nested. `lifecycleMu` guards Start/Stop only and is never held
// across a call into another subsystem.
//
// If a new code path crosses subsystems under a held lock, declare the
// order explicitly here first.
package quota

import (
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// QuotaConfig defines the quota limits for a key or team.
// Embedded inline via yaml:",inline" on VirtualKey and Team.
type QuotaConfig struct {
	RPMLimit            int     `yaml:"rpm_limit,omitempty" json:"rpm_limit,omitempty"`
	TPMLimit            int     `yaml:"tpm_limit,omitempty" json:"tpm_limit,omitempty"`
	MaxParallelRequests int     `yaml:"max_parallel_requests,omitempty" json:"max_parallel_requests,omitempty"`
	SpendLimit          float64 `yaml:"spend_limit,omitempty" json:"spend_limit,omitempty"`   // USD, display only — tracker uses microdollars internally
	ResetPeriod         string  `yaml:"reset_period,omitempty" json:"reset_period,omitempty"` // "daily", "weekly", "monthly"
	DefaultMaxTokens    int     `yaml:"default_max_tokens,omitempty" json:"default_max_tokens,omitempty"`
}

// SpendLimitMicro returns the spend limit as microdollars (int64).
func (q *QuotaConfig) SpendLimitMicro() int64 {
	return USDToMicro(q.SpendLimit)
}

// ScopeKind distinguishes key-level from team-level enforcement.
type ScopeKind string

const (
	ScopeKey  ScopeKind = "key"
	ScopeTeam ScopeKind = "team"
)

// ThrottleScope is the map key for rate limiters.
// Kind prevents collisions when a key and its team share the same model.
type ThrottleScope struct {
	Kind     ScopeKind
	EntityID string
	Model    string // "" for global
}

// SpendScope is the map key for spend tracking.
type SpendScope struct {
	Kind     ScopeKind
	EntityID string
	Model    string // "" for global
}

// String returns a compound key for map lookups.
func (s SpendScope) String() string {
	return string(s.Kind) + ":" + s.EntityID + ":" + s.Model
}

// SpendState tracks spending for one scope in the current period.
//
// LimitMicro and ThresholdsFired support the budget threshold event
// stream: the limit is cached on each ReserveBudget so settlement can
// compute % consumed without re-fetching QuotaConfig, and the bitfield
// records which crossings (50/80/95) have already fired this period
// so each one emits at most one event per period regardless of how
// many settlements take SpendMicro past the boundary.
type SpendState struct {
	EntityID        string       `json:"entity_id"`
	Kind            ScopeKind    `json:"kind"`
	SpendMicro      int64        `json:"spend_micro"` // 1 USD = 1_000_000
	ReservedMicro   int64        `json:"reserved_micro"`
	LastReservedAt  time.Time    `json:"last_reserved_at"` // set on each ReserveBudget; reaper zeros ReservedMicro when stale
	TokensIn        int64        `json:"tokens_in"`
	TokensOut       int64        `json:"tokens_out"`
	RequestCount    int64        `json:"request_count"`
	PeriodStart     time.Time    `json:"period_start"`
	Period          BudgetPeriod `json:"period"`
	LimitMicro      int64        `json:"limit_micro,omitempty"`      // most-recent reservation limit; basis for threshold % calculation
	ThresholdsFired uint8        `json:"thresholds_fired,omitempty"` // bitfield: bit 0=50%, bit 1=80%, bit 2=95% — reset on period roll
}

// SpendUSD returns the current spend in dollars.
func (s *SpendState) SpendUSD() float64 { return float64(s.SpendMicro) / 1_000_000 }

// BudgetPeriod represents a budget reset cycle.
type BudgetPeriod string

const (
	PeriodDaily   BudgetPeriod = "daily"
	PeriodWeekly  BudgetPeriod = "weekly"
	PeriodMonthly BudgetPeriod = "monthly"
)

// budgetThresholdPercents are the percentages emitted as separate
// informational breach events when crossed for the first time in a
// period. Order matters: lower thresholds bit-numbered first so the
// bitfield reads naturally (bit 0 = 50%, bit 1 = 80%, bit 2 = 95%).
//
// 95% is the late-warning rather than 99% because settlements are
// estimated then trued-up — a 99% trigger sometimes wouldn't fire
// before the next settlement carried the scope past 100% (and then
// the agent gets only the budget_exhausted denial, no advance notice).
//
// Unexported so the array can't be mutated from outside the package.
// Use BudgetThresholdPercents() to read it.
var budgetThresholdPercents = [...]uint8{50, 80, 95}

// BudgetThresholdPercents returns a copy of the threshold percentages
// that may fire as informational breach events.
func BudgetThresholdPercents() []uint8 {
	out := make([]uint8, len(budgetThresholdPercents))
	copy(out, budgetThresholdPercents[:])
	return out
}

// ParsePeriod converts a string to BudgetPeriod, defaulting to monthly.
func ParsePeriod(s string) BudgetPeriod {
	switch BudgetPeriod(s) {
	case PeriodDaily:
		return PeriodDaily
	case PeriodWeekly:
		return PeriodWeekly
	case PeriodMonthly:
		return PeriodMonthly
	default:
		return PeriodMonthly
	}
}

// PeriodStartFor returns the start of the current period containing t.
func PeriodStartFor(t time.Time, period BudgetPeriod) time.Time {
	t = t.UTC()
	switch period {
	case PeriodDaily:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case PeriodWeekly:
		weekday := int(t.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		monday := t.AddDate(0, 0, -(weekday - 1))
		return time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, time.UTC)
	case PeriodMonthly:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
}

// NextPeriodStart returns the start of the next period.
func NextPeriodStart(periodStart time.Time, period BudgetPeriod) time.Time {
	switch period {
	case PeriodDaily:
		return periodStart.AddDate(0, 0, 1)
	case PeriodWeekly:
		return periodStart.AddDate(0, 0, 7)
	case PeriodMonthly:
		return periodStart.AddDate(0, 1, 0)
	default:
		return periodStart.AddDate(0, 1, 0)
	}
}

// IsExpired returns true if now is past the end of the period.
func IsExpired(start time.Time, period BudgetPeriod) bool {
	return !utils.Now().Before(NextPeriodStart(start, period))
}

// USDToMicro converts dollars to microdollars.
func USDToMicro(usd float64) int64 {
	return int64(usd * 1_000_000)
}

// MicroToUSD converts microdollars to dollars.
func MicroToUSD(micro int64) float64 {
	return float64(micro) / 1_000_000
}

// DefaultMaxTokensFallback is the system-wide fallback for budget reservation
// estimates when neither the request body nor QuotaConfig specify max_tokens.
const DefaultMaxTokensFallback = 1024

// MinReservationMicro is the floor for the smart auto-fallback reservation
// estimate (10 tokens × 10 µUSD/token = 100 µUSD). Without it, sub-microUSD
// truncated budgets (e.g. spend_limit < ~$0.0002) would reserve 0 µ and let
// concurrent requests blow past the budget before settle catches up — a
// silent enforcement gap. With the floor, budgets below 200 µUSD still hit
// the historic "deny on first reservation" failure mode (loud), instead.
const MinReservationMicro int64 = 100
