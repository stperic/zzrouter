package quota

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestEnforcer() *Enforcer {
	rl := NewRateLimiter()
	cl := NewConcurrencyLimiter()
	st := NewSpendTracker("")
	return NewEnforcer(rl, cl, st)
}

func TestEnforcer_Check_SingleScope_AllPass(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	chain := []QuotaScope{{
		Scope:    ScopeKey,
		EntityID: "key-1",
		Quotas:   QuotaConfig{RPMLimit: 100, TPMLimit: 100000},
	}}

	d, reservations := e.Check(chain)
	assert.True(t, d.Allowed)
	assert.Empty(t, reservations)
}

func TestEnforcer_Check_RPMDenied(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	chain := []QuotaScope{{
		Scope:    ScopeKey,
		EntityID: "key-1",
		Quotas:   QuotaConfig{RPMLimit: 1},
	}}

	// First request passes
	d, _ := e.Check(chain)
	require.True(t, d.Allowed)

	// Second request denied
	d, _ = e.Check(chain)
	assert.False(t, d.Allowed)
	assert.Equal(t, "rpm_limit_exceeded", d.Reason)
	assert.Equal(t, ScopeKey, d.Scope)
	assert.Equal(t, "key-1", d.EntityID)
}

func TestEnforcer_Check_BudgetDenied(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	chain := []QuotaScope{{
		Scope:    ScopeKey,
		EntityID: "key-1",
		Quotas:   QuotaConfig{SpendLimit: 0.001, ResetPeriod: "monthly", DefaultMaxTokens: 50000},
	}}

	// First reservation may pass (depends on estimate vs limit)
	// With SpendLimit=0.001 = 1000 microdollars, and DefaultMaxTokens=50000 * 10 = 500000 micro estimate
	// This should be denied immediately
	d, _ := e.Check(chain)
	assert.False(t, d.Allowed)
	assert.Equal(t, "budget_exhausted", d.Reason)
}

func TestEnforcer_Check_MultiScope_KeyDenied(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	chain := []QuotaScope{
		{Scope: ScopeKey, EntityID: "key-1", Quotas: QuotaConfig{RPMLimit: 1}},
		{Scope: ScopeTeam, EntityID: "team-1", Quotas: QuotaConfig{RPMLimit: 100}},
	}

	d, _ := e.Check(chain)
	require.True(t, d.Allowed)

	d, _ = e.Check(chain)
	assert.False(t, d.Allowed)
	assert.Equal(t, ScopeKey, d.Scope) // Key denied, not team
}

func TestEnforcer_Check_MultiScope_TeamDenied(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	chain := []QuotaScope{
		{Scope: ScopeKey, EntityID: "key-1", Quotas: QuotaConfig{RPMLimit: 100}},
		{Scope: ScopeTeam, EntityID: "team-1", Quotas: QuotaConfig{RPMLimit: 1}},
	}

	d, _ := e.Check(chain)
	require.True(t, d.Allowed)

	d, _ = e.Check(chain)
	assert.False(t, d.Allowed)
	assert.Equal(t, ScopeTeam, d.Scope) // Team denied
}

func TestEnforcer_AcquireConcurrency(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	chain := []QuotaScope{{
		Scope:    ScopeKey,
		EntityID: "key-1",
		Quotas:   QuotaConfig{MaxParallelRequests: 2},
	}}

	// Acquire first
	release1, d := e.AcquireConcurrency(chain)
	require.True(t, d.Allowed)
	require.NotNil(t, release1)

	// Acquire second
	release2, d := e.AcquireConcurrency(chain)
	require.True(t, d.Allowed)

	// Third should be denied
	_, d = e.AcquireConcurrency(chain)
	assert.False(t, d.Allowed)
	assert.Equal(t, "concurrency_limit_exceeded", d.Reason)

	// Release one, then acquire should work
	release1()
	release3, d := e.AcquireConcurrency(chain)
	assert.True(t, d.Allowed)

	release2()
	release3()
}

func TestEnforcer_BudgetReservationCancelledOnConcurrencyDenial(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()

	// Set up: key has budget and concurrency limit of 1
	chain := []QuotaScope{{
		Scope:    ScopeKey,
		EntityID: "key-budget-conc",
		Quotas: QuotaConfig{
			SpendLimit:          100.0, // $100
			ResetPeriod:         "monthly",
			MaxParallelRequests: 1,
			DefaultMaxTokens:    100,
		},
	}}

	// First: check quotas (reserves budget) + acquire concurrency
	d, reservations := e.Check(chain)
	require.True(t, d.Allowed)
	require.Len(t, reservations, 1)

	release, d := e.AcquireConcurrency(chain)
	require.True(t, d.Allowed)

	// Second: check quotas (reserves budget) + concurrency should be denied
	d2, reservations2 := e.Check(chain)
	require.True(t, d2.Allowed)

	_, d3 := e.AcquireConcurrency(chain)
	assert.False(t, d3.Allowed)

	// Caller must cancel reservations when concurrency fails
	e.CancelReservations(reservations2)

	// Verify: the reservation was cancelled (check tracker state)
	scope := SpendScope{Kind: ScopeKey, EntityID: "key-budget-conc"}
	state := e.tracker.GetState(scope)
	require.NotNil(t, state)
	// Only the first reservation should remain
	assert.Equal(t, reservations[0].estimatedCostMicro, state.ReservedMicro)

	release()
}

// TestEnforcer_SpendIntrospection_RoundTrip verifies the admin helpers
// GetSpendState / GetAllSpendStates / ResetSpend. These are the only way
// HTTP controllers can observe or clear spend without reaching into
// quota's internals, so the contract has to hold across reserve → settle
// → query → reset cycles.
func TestEnforcer_SpendIntrospection_RoundTrip(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()

	chain := []QuotaScope{{
		Scope:    ScopeKey,
		EntityID: "key-report",
		Quotas: QuotaConfig{
			SpendLimit:       1.00, // $1
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 100, // ~= 1000 microdollars reserved
		},
	}}

	// Before any traffic: GetSpendState returns nil.
	scope := SpendScope{Kind: ScopeKey, EntityID: "key-report"}
	require.Nil(t, e.GetSpendState(scope), "unexpected spend state before first check")

	// Reserve + settle one request.
	d, reservations := e.Check(chain)
	require.True(t, d.Allowed)
	require.Len(t, reservations, 1)
	e.SettleReservations(reservations, 420_000 /* $0.42 */, 100, 200)

	// Now GetSpendState reflects the settled cost.
	state := e.GetSpendState(scope)
	require.NotNil(t, state)
	assert.Equal(t, int64(420_000), state.SpendMicro)
	assert.EqualValues(t, 0, state.ReservedMicro, "reservation should be cleared after settle")
	assert.EqualValues(t, 100, state.TokensIn)
	assert.EqualValues(t, 200, state.TokensOut)
	assert.EqualValues(t, 1, state.RequestCount)

	// GetAllSpendStates surfaces the same row.
	all := e.GetAllSpendStates()
	require.Len(t, all, 1)
	for _, s := range all {
		assert.Equal(t, "key-report", s.EntityID)
		assert.Equal(t, ScopeKey, s.Kind)
	}

	// ResetSpend clears it back to zero but leaves the slot in place.
	e.ResetSpend(scope)
	after := e.GetSpendState(scope)
	require.NotNil(t, after)
	assert.EqualValues(t, 0, after.SpendMicro)
	assert.EqualValues(t, 0, after.TokensIn)
	assert.EqualValues(t, 0, after.RequestCount)
}

// TestEnforcer_SpendIntrospection_UnknownScope verifies read helpers don't
// panic on scopes that were never reserved against (empty tracker state).
func TestEnforcer_SpendIntrospection_UnknownScope(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	assert.Nil(t, e.GetSpendState(SpendScope{Kind: ScopeKey, EntityID: "ghost"}))
	assert.Empty(t, e.GetAllSpendStates())
	// ResetSpend on an unknown scope is a no-op.
	e.ResetSpend(SpendScope{Kind: ScopeTeam, EntityID: "nobody"})
}

// TestEnforcer_SmartReservationFallback pins the smart auto-fallback:
// when DefaultMaxTokens is unset, reservation estimate scales with the
// spend limit (capped at the historic 1024-token fallback). Tight-
// budget keys are no longer broken-by-construction.
func TestEnforcer_SmartReservationFallback(t *testing.T) {
	t.Parallel()
	historicCap := int64(DefaultMaxTokensFallback) * 10 // 10240µ — historic fallback
	tests := []struct {
		name           string
		spendLimitUSD  float64
		wantFirstAllow bool // first reservation attempt must succeed
	}{
		// Tight budgets (would all deny under the flat 1024 fallback).
		{"$0.001 — formerly broken", 0.001, true},
		{"$0.0005 — formerly broken", 0.0005, true},
		{"$0.005 — formerly broken", 0.005, true},
		// At/around the cap boundary — historic behavior preserved.
		{"$0.05 — cap kicks in", 0.05, true},
		{"$1.00 — high budget unchanged", 1.00, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnforcer()
			d, _ := e.Check([]QuotaScope{{
				Scope:    ScopeKey,
				EntityID: "smart-" + tt.name,
				Quotas:   QuotaConfig{SpendLimit: tt.spendLimitUSD, ResetPeriod: "monthly"},
			}})
			assert.Equal(t, tt.wantFirstAllow, d.Allowed,
				"first reservation against $%.4f should %s", tt.spendLimitUSD,
				map[bool]string{true: "succeed", false: "fail"}[tt.wantFirstAllow])
		})
	}

	// Cap invariant: $1 budget still uses the 10240µ historic estimate,
	// not the unbounded half-the-budget value.
	t.Run("cap invariant for high budget", func(t *testing.T) {
		e := newTestEnforcer()
		scope := SpendScope{Kind: ScopeKey, EntityID: "cap-check"}
		// First N reservations must all succeed against $1 budget at
		// 10240µ each — confirms we're using the cap, not $0.50.
		for i := 0; i < 5; i++ {
			d, _ := e.Check([]QuotaScope{{
				Scope:    ScopeKey,
				EntityID: "cap-check",
				Quotas:   QuotaConfig{SpendLimit: 1.00, ResetPeriod: "monthly"},
			}})
			require.True(t, d.Allowed, "reservation %d should succeed at cap=$0.01024 against $1 budget", i+1)
		}
		st := e.GetSpendState(scope)
		require.NotNil(t, st)
		// 5 reservations × 10240µ each = 51200µ held.
		assert.Equal(t, int64(5)*historicCap, st.ReservedMicro)
	})

	// Threshold check: $0.02048 = 20480µ is the smallest spend_limit
	// where the smart fallback's limit/2 hits the historic cap exactly
	// (limit/2 == 10240). Just below it, the new value diverges from
	// the old; just at/above, behavior is byte-identical to pre-change.
	t.Run("threshold at $0.02048", func(t *testing.T) {
		e := newTestEnforcer()
		// Budget exactly at threshold: estimate = limit/2 = 10240 = cap.
		// First reservation: 0+0+10240 ≤ 20480 ⇒ allow.
		// Second reservation: 0+10240+10240 = 20480, not > 20480 ⇒ allow.
		// Third reservation: 0+20480+10240 = 30720 > 20480 ⇒ deny.
		for i := 0; i < 2; i++ {
			d, _ := e.Check([]QuotaScope{{
				Scope:    ScopeKey,
				EntityID: "threshold",
				Quotas:   QuotaConfig{SpendLimit: 0.02048, ResetPeriod: "monthly"},
			}})
			require.True(t, d.Allowed, "reservation %d at threshold budget should fit", i+1)
		}
		d, _ := e.Check([]QuotaScope{{
			Scope:    ScopeKey,
			EntityID: "threshold",
			Quotas:   QuotaConfig{SpendLimit: 0.02048, ResetPeriod: "monthly"},
		}})
		assert.False(t, d.Allowed, "third reservation must trip the gate")
	})

	// Floor check: a sub-200µ budget where limit/2 would truncate to
	// less than MinReservationMicro (100µ). Before the floor was added,
	// the reservation could be 0µ and let concurrent requests blow
	// past the budget before settle catches up. With the floor at
	// 100µ, the original "deny on first reservation" loud failure
	// mode is preserved (100 > 50 budget ⇒ deny).
	t.Run("floor for sub-200µ budgets", func(t *testing.T) {
		e := newTestEnforcer()
		d, _ := e.Check([]QuotaScope{{
			Scope:    ScopeKey,
			EntityID: "floor",
			// $0.00005 = 50µ: half = 25µ, but floor brings it to 100µ.
			// 0+0+100 > 50 ⇒ deny (no silent under-reservation).
			Quotas: QuotaConfig{SpendLimit: 0.00005, ResetPeriod: "monthly"},
		}})
		assert.False(t, d.Allowed, "floor must keep sub-floor budgets in the loud-deny path")
	})
}
