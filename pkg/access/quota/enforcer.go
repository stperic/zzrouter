package quota

import (
	"context"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Decision is the result of a quota check. Pure data — no HTTP.
type Decision struct {
	Allowed  bool
	Reason   string    // "rpm_limit_exceeded", "tpm_limit_exceeded", "budget_exhausted", "concurrency_limit_exceeded"
	Scope    ScopeKind // which scope denied
	EntityID string    // which key/team

	// Budget context — populated on Reason == "budget_exhausted" so
	// the HTTP adapter can surface "X used of Y" without a second
	// tracker lookup. Microdollars; zero on non-budget denials.
	SpendLimitMicro int64
	SpendUsedMicro  int64 // already-settled spend
	SpendHeldMicro  int64 // outstanding reservations from peer requests
	BudgetPeriod    string
	PeriodStart     time.Time // start of the current budget period (UTC)
}

// QuotaScope pairs an entity with its quota config for chain-based enforcement.
type QuotaScope struct {
	Scope    ScopeKind
	EntityID string
	Quotas   QuotaConfig
}

// Enforcer checks quota chains. Pure decisions — never touches HTTP.
type Enforcer struct {
	rateLimiter *RateLimiter
	concurrency *ConcurrencyLimiter
	tracker     *SpendTracker
	metrics     MetricsRecorder
	breaches    BreachObserver
}

// NewEnforcer creates a quota enforcer wired to the given subsystems.
// The metrics recorder defaults to NullMetricsRecorder; call
// SetMetricsRecorder to install an observability hook.
func NewEnforcer(rl *RateLimiter, cl *ConcurrencyLimiter, st *SpendTracker) *Enforcer {
	return &Enforcer{
		rateLimiter: rl,
		concurrency: cl,
		tracker:     st,
		metrics:     NullMetricsRecorder{},
		breaches:    NullBreachObserver{},
	}
}

// SetMetricsRecorder replaces the observability recorder. Safe to call
// during startup (pre-traffic); not expected to be called under load.
// A nil argument is coerced to NullMetricsRecorder so the hot path
// never needs a nil check.
func (e *Enforcer) SetMetricsRecorder(r MetricsRecorder) {
	if r == nil {
		e.metrics = NullMetricsRecorder{}
		return
	}
	e.metrics = r
}

// SetBreachObserver replaces the breach event sink. Same nil-coerce
// + startup-only guarantees as SetMetricsRecorder.
func (e *Enforcer) SetBreachObserver(o BreachObserver) {
	if o == nil {
		e.breaches = NullBreachObserver{}
		return
	}
	e.breaches = o
}

// Start launches background goroutines for rate limiting and spend tracking.
// Delegates ctx to leaf subsystems. Restart-safe: leaves handle double-Start
// as a no-op and re-launch after Stop.
func (e *Enforcer) Start(ctx context.Context) {
	if e.rateLimiter != nil {
		e.rateLimiter.Start(ctx)
	}
	if e.tracker != nil {
		e.tracker.Start(ctx)
	}
}

// Stop terminates background goroutines owned by the enforcer's subsystems
// and flushes any pending spend state to disk. Safe to call multiple times;
// leaf Stop methods are idempotent and restart-safe.
func (e *Enforcer) Stop(ctx context.Context) {
	if e.tracker != nil {
		e.tracker.Stop(ctx)
	}
	if e.rateLimiter != nil {
		e.rateLimiter.Stop(ctx)
	}
}

// FlushSpend persists dirty spend state to disk without stopping any
// subsystems. Used by Server.Stop as a pre-drain phase so a slow HTTP
// shutdown can't push the final save into SIGKILL territory.
func (e *Enforcer) FlushSpend() {
	if e.tracker != nil {
		e.tracker.Flush()
	}
}

// Check evaluates RPM, TPM, and budget for each scope in the chain.
// Returns the first denial, or an allow decision if all pass.
// Budget reservations are made for each scope that has a spend limit;
// the caller MUST call CancelReservations if a later step (e.g., concurrency) fails.
func (e *Enforcer) Check(chain []QuotaScope) (Decision, []reservationRecord) {
	var reservations []reservationRecord

	for _, qs := range chain {
		// RPM
		if qs.Quotas.RPMLimit > 0 {
			scope := ThrottleScope{Kind: qs.Scope, EntityID: qs.EntityID}
			if !e.rateLimiter.AllowRPM(scope, qs.Quotas.RPMLimit) {
				e.cancelReservations(reservations)
				e.metrics.ObserveDecision(qs.Scope, "denied", "rpm_limit_exceeded")
				e.breaches.OnBreach(BreachEvent{Time: utils.NowUTC(), Scope: qs.Scope, ScopeID: qs.EntityID, Reason: "rpm_limit_exceeded"})
				return Decision{
					Allowed:  false,
					Reason:   "rpm_limit_exceeded",
					Scope:    qs.Scope,
					EntityID: qs.EntityID,
				}, nil
			}
		}

		// TPM
		if qs.Quotas.TPMLimit > 0 {
			scope := ThrottleScope{Kind: qs.Scope, EntityID: qs.EntityID}
			if !e.rateLimiter.AllowTPM(scope, qs.Quotas.TPMLimit) {
				e.cancelReservations(reservations)
				e.metrics.ObserveDecision(qs.Scope, "denied", "tpm_limit_exceeded")
				e.breaches.OnBreach(BreachEvent{Time: utils.NowUTC(), Scope: qs.Scope, ScopeID: qs.EntityID, Reason: "tpm_limit_exceeded"})
				return Decision{
					Allowed:  false,
					Reason:   "tpm_limit_exceeded",
					Scope:    qs.Scope,
					EntityID: qs.EntityID,
				}, nil
			}
		}

		// Budget reservation
		if qs.Quotas.SpendLimit > 0 && e.tracker != nil {
			scope := SpendScope{Kind: qs.Scope, EntityID: qs.EntityID}
			limitMicro := qs.Quotas.SpendLimitMicro()
			period := ParsePeriod(qs.Quotas.ResetPeriod)
			// Per-request budget reservation estimate.
			//
			// When DefaultMaxTokens is set explicitly, use it × 10µUSD
			// (the calibration constant: ~$0.00001/token across most
			// frontier models). When it's unset, derive a sane
			// reservation from the spend limit itself instead of a
			// flat 1024-token fallback.
			//
			// Why: a flat fallback ($0.01024 per reservation) silently
			// breaks any key with a spend limit below ~$0.01 — first
			// reservation trips even though actual cost is sub-cent.
			// The cap (≤ DefaultMaxTokensFallback × 10µ) preserves
			// existing behavior for high-budget keys; large limits
			// keep the historic estimate, which still gives 5+
			// concurrent reservations before tripping.
			var estimatedCostMicro int64
			if qs.Quotas.DefaultMaxTokens > 0 {
				estimatedCostMicro = int64(qs.Quotas.DefaultMaxTokens) * 10
			} else {
				estimatedCostMicro = limitMicro / 2
				if estimatedCostMicro < MinReservationMicro {
					estimatedCostMicro = MinReservationMicro
				}
				if max := int64(DefaultMaxTokensFallback) * 10; estimatedCostMicro > max {
					estimatedCostMicro = max
				}
			}

			if !e.tracker.ReserveBudget(scope, limitMicro, period, estimatedCostMicro) {
				e.cancelReservations(reservations)
				e.metrics.ObserveDecision(qs.Scope, "denied", "budget_exhausted")
				// Pull current state for the denial envelope. State
				// is guaranteed present after a failed reservation
				// (ReserveBudget creates the row before checking).
				var spent, held int64
				var periodStart time.Time
				if st := e.tracker.GetState(scope); st != nil {
					spent = st.SpendMicro
					held = st.ReservedMicro
					periodStart = st.PeriodStart
				}
				e.breaches.OnBreach(BreachEvent{
					Time:            utils.NowUTC(),
					Scope:           qs.Scope,
					ScopeID:         qs.EntityID,
					Reason:          "budget_exhausted",
					SpendLimitMicro: limitMicro,
					SpendUsedMicro:  spent,
					SpendHeldMicro:  held,
					BudgetPeriod:    string(period),
				})
				return Decision{
					Allowed:         false,
					Reason:          "budget_exhausted",
					Scope:           qs.Scope,
					EntityID:        qs.EntityID,
					SpendLimitMicro: limitMicro,
					SpendUsedMicro:  spent,
					SpendHeldMicro:  held,
					BudgetPeriod:    string(period),
					PeriodStart:     periodStart,
				}, nil
			}
			reservations = append(reservations, reservationRecord{
				scope:              scope,
				estimatedCostMicro: estimatedCostMicro,
			})
		}
	}

	// Record an "allowed" decision against the narrowest scope in the
	// chain (keys win over teams when both are present). If the chain
	// is empty, no observation — the caller didn't perform a check.
	if len(chain) > 0 {
		e.metrics.ObserveDecision(chain[len(chain)-1].Scope, "allowed", "")
	}

	return Decision{Allowed: true}, reservations
}

// AcquireConcurrency tries to acquire a concurrency slot for the first scope
// in the chain that has MaxParallelRequests > 0.
// Returns a release function and the decision.
//
// Callers MUST `defer release()` on the line immediately following a
// non-nil return. A panic between Acquire and the deferred release
// leaks the slot and the `zzrouter_quota_concurrency_active` gauge
// until process restart — the counter is deliberately authoritative
// against live state, not reconstructable from a ledger.
func (e *Enforcer) AcquireConcurrency(chain []QuotaScope) (release func(), decision Decision) {
	for _, qs := range chain {
		if qs.Quotas.MaxParallelRequests <= 0 {
			continue
		}
		scope := ThrottleScope{Kind: qs.Scope, EntityID: qs.EntityID}
		if !e.concurrency.Acquire(scope, qs.Quotas.MaxParallelRequests) {
			e.metrics.ObserveDecision(qs.Scope, "denied", "concurrency_limit_exceeded")
			e.breaches.OnBreach(BreachEvent{Time: utils.NowUTC(), Scope: qs.Scope, ScopeID: qs.EntityID, Reason: "concurrency_limit_exceeded"})
			return nil, Decision{
				Allowed:  false,
				Reason:   "concurrency_limit_exceeded",
				Scope:    qs.Scope,
				EntityID: qs.EntityID,
			}
		}
		e.metrics.ObserveConcurrencyAcquire(qs.Scope)
		acquiredScope := qs.Scope
		return func() {
			e.concurrency.Release(scope)
			e.metrics.ObserveConcurrencyRelease(acquiredScope)
		}, Decision{Allowed: true}
	}
	return func() {}, Decision{Allowed: true}
}

// CancelReservations cancels all budget reservations in the list.
func (e *Enforcer) CancelReservations(reservations []reservationRecord) {
	e.cancelReservations(reservations)
}

func (e *Enforcer) cancelReservations(reservations []reservationRecord) {
	for _, r := range reservations {
		e.tracker.CancelReservation(r.scope, r.estimatedCostMicro)
	}
}

// SettleReservations settles all budget reservations with actual costs.
// Emits one ObserveSpendSettlement per reservation so the exported
// metric `sum by (scope) (zzrouter_quota_spend_settled_usd_total)`
// matches the spend ledger (which also records against every scope
// that held a reservation).
//
// Threshold detection runs atomically inside the tracker; any
// newly-crossed budget threshold (50/80/95) emits a single
// informational BreachEvent on the observer. Crossings are deduped
// per period so multiple settlements past the same boundary fire
// exactly one event. The envelope on the event is captured under the
// same lock that claimed the bit, so used+held describes the basis
// the percentage was claimed against.
func (e *Enforcer) SettleReservations(reservations []reservationRecord, actualCostMicro, tokensIn, tokensOut int64) {
	usd := 0.0
	if actualCostMicro > 0 {
		usd = MicroToUSD(actualCostMicro)
	}
	for _, r := range reservations {
		crossed, env := e.tracker.SettleAndDetectCrossings(r.scope, r.estimatedCostMicro, actualCostMicro, tokensIn, tokensOut)
		if usd > 0 {
			e.metrics.ObserveSpendSettlement(r.scope.Kind, usd)
		}
		for _, pct := range crossed {
			reason := ThresholdReason(pct)
			if reason == "" {
				continue
			}
			e.breaches.OnBreach(BreachEvent{
				Time:             utils.NowUTC(),
				Scope:            r.scope.Kind,
				ScopeID:          r.scope.EntityID,
				Reason:           reason,
				SpendLimitMicro:  env.LimitMicro,
				SpendUsedMicro:   env.SpendMicro,
				SpendHeldMicro:   env.ReservedMicro,
				BudgetPeriod:     string(env.Period),
				ThresholdPercent: pct,
			})
		}
	}
}

// RecordTokens records tokens consumed post-response for TPM tracking.
func (e *Enforcer) RecordTokens(scope ThrottleScope, tokens int64) {
	if e.rateLimiter != nil {
		e.rateLimiter.RecordTokens(scope, tokens)
	}
}

// ===== Spend introspection / admin =====
//
// These read-only / admin helpers let higher layers (HTTP controllers,
// TUIs) report and reset spend without taking a direct dependency on
// the underlying SpendTracker type.

// GetSpendState returns a snapshot of the spend state for a single scope,
// or nil when no traffic has been recorded for it yet.
func (e *Enforcer) GetSpendState(scope SpendScope) *SpendState {
	if e.tracker == nil {
		return nil
	}
	return e.tracker.GetState(scope)
}

// GetAllSpendStates returns a snapshot of every tracked spend state.
// Map keys are SpendScope.String() — the same format SpendTracker uses
// internally. Safe to consume concurrently with enforcement.
func (e *Enforcer) GetAllSpendStates() map[string]*SpendState {
	if e.tracker == nil {
		return map[string]*SpendState{}
	}
	return e.tracker.GetAllStates()
}

// ResetSpend zeros the spend ledger for a scope, starting a fresh period.
// No-op if the scope has never been tracked.
func (e *Enforcer) ResetSpend(scope SpendScope) {
	if e.tracker != nil {
		e.tracker.ResetScope(scope)
	}
}

// ForgetEntity removes every tracked spend row for the given entity,
// across all per-model sub-rows. Call after a key or team is deleted so
// its ledger stops occupying memory and stops being re-persisted.
func (e *Enforcer) ForgetEntity(kind ScopeKind, entityID string) {
	if e.tracker != nil {
		e.tracker.ForgetEntity(kind, entityID)
	}
}

// WriteRateLimitHeaders returns OpenAI-style rate limit header values.
func (e *Enforcer) WriteRateLimitHeaders(chain []QuotaScope) map[string]string {
	headers := make(map[string]string)
	for _, qs := range chain {
		if qs.Quotas.RPMLimit > 0 || qs.Quotas.TPMLimit > 0 {
			scope := ThrottleScope{Kind: qs.Scope, EntityID: qs.EntityID}
			snap := e.rateLimiter.Snapshot(scope, qs.Quotas.RPMLimit, qs.Quotas.TPMLimit)
			// Use the first scope's limits (key-level) for response headers
			if _, exists := headers["x-ratelimit-limit-requests"]; !exists {
				if snap.RPMLimit > 0 {
					headers["x-ratelimit-limit-requests"] = formatInt(snap.RPMLimit)
					headers["x-ratelimit-remaining-requests"] = formatInt(snap.RPMRemaining)
					headers["x-ratelimit-reset-requests"] = formatDuration(snap.RPMResetSeconds)
				}
				if snap.TPMLimit > 0 {
					headers["x-ratelimit-limit-tokens"] = formatInt(snap.TPMLimit)
					headers["x-ratelimit-remaining-tokens"] = formatInt(snap.TPMRemaining)
					headers["x-ratelimit-reset-tokens"] = formatDuration(snap.TPMResetSeconds)
				}
			}
		}
	}
	return headers
}

// reservationRecord tracks a pending budget reservation for cancel/settle.
type reservationRecord struct {
	scope              SpendScope
	estimatedCostMicro int64
}

// ReservationRecord is the exported type for reservations held by callers.
type ReservationRecord = reservationRecord

func formatInt(n int) string {
	// Simple int-to-string without importing strconv at package level
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 12)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

func formatDuration(seconds float64) string {
	if seconds <= 0 {
		return "0s"
	}
	ms := int(seconds * 1000)
	if ms < 1000 {
		return formatInt(ms) + "ms"
	}
	return formatInt(ms/1000) + "s"
}
