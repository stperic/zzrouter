package quota

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureObserver struct {
	mu     sync.Mutex
	events []BreachEvent
}

func (c *captureObserver) OnBreach(e BreachEvent) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

// TestEnforcer_BreachObserver_RPMDeny pins that an RPM denial fires
// OnBreach with the right scope identity + reason.
func TestEnforcer_BreachObserver_RPMDeny(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	obs := &captureObserver{}
	e.SetBreachObserver(obs)

	chain := []QuotaScope{{Scope: ScopeKey, EntityID: "alice", Quotas: QuotaConfig{RPMLimit: 1}}}
	require.True(t, mustCheck(e, chain).Allowed, "first request fits within RPM=1")
	require.False(t, mustCheck(e, chain).Allowed, "second request hits the cap")

	obs.mu.Lock()
	defer obs.mu.Unlock()
	require.Len(t, obs.events, 1)
	assert.Equal(t, "rpm_limit_exceeded", obs.events[0].Reason)
	assert.Equal(t, ScopeKey, obs.events[0].Scope)
	assert.Equal(t, "alice", obs.events[0].ScopeID)
	assert.False(t, obs.events[0].Time.IsZero())
}

// TestEnforcer_BreachObserver_BudgetDeny pins that a budget denial
// carries the spend-limit context the SSE consumer wants to surface.
func TestEnforcer_BreachObserver_BudgetDeny(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	obs := &captureObserver{}
	e.SetBreachObserver(obs)

	// SpendLimit=0.001 = 1000µ, DefaultMaxTokens=50000 → 500000µ estimate;
	// first reservation already exceeds the limit → immediate deny.
	chain := []QuotaScope{{
		Scope:    ScopeTeam,
		EntityID: "team-x",
		Quotas:   QuotaConfig{SpendLimit: 0.001, ResetPeriod: "monthly", DefaultMaxTokens: 50000},
	}}
	require.False(t, mustCheck(e, chain).Allowed, "first reservation exhausts the budget")

	obs.mu.Lock()
	defer obs.mu.Unlock()
	require.Len(t, obs.events, 1)
	ev := obs.events[0]
	assert.Equal(t, "budget_exhausted", ev.Reason)
	assert.Equal(t, "team-x", ev.ScopeID)
	assert.Equal(t, "monthly", ev.BudgetPeriod)
	assert.Greater(t, ev.SpendLimitMicro, int64(0))
}

// TestEnforcer_BreachObserver_ConcurrencyDeny pins concurrency denials.
func TestEnforcer_BreachObserver_ConcurrencyDeny(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	obs := &captureObserver{}
	e.SetBreachObserver(obs)

	chain := []QuotaScope{{Scope: ScopeKey, EntityID: "alice", Quotas: QuotaConfig{MaxParallelRequests: 1}}}
	rel, d := e.AcquireConcurrency(chain)
	require.True(t, d.Allowed)
	require.NotNil(t, rel)
	defer rel()

	// Second acquire blocks at the cap.
	rel2, d2 := e.AcquireConcurrency(chain)
	require.False(t, d2.Allowed)
	assert.Nil(t, rel2)

	obs.mu.Lock()
	defer obs.mu.Unlock()
	require.Len(t, obs.events, 1)
	assert.Equal(t, "concurrency_limit_exceeded", obs.events[0].Reason)
}

// TestEnforcer_BreachObserver_AllowsDoNotFire pins the negative case —
// an allowed decision must NOT fire a breach event.
func TestEnforcer_BreachObserver_AllowsDoNotFire(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	obs := &captureObserver{}
	e.SetBreachObserver(obs)

	chain := []QuotaScope{{Scope: ScopeKey, EntityID: "alice", Quotas: QuotaConfig{RPMLimit: 100}}}
	for i := 0; i < 5; i++ {
		require.True(t, mustCheck(e, chain).Allowed)
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	assert.Empty(t, obs.events, "allowed decisions must not fire breach events")
}

// TestEnforcer_BreachObserver_NilCoercedToNull pins the safety net for
// SetBreachObserver(nil) — should not crash on the next deny.
func TestEnforcer_BreachObserver_NilCoercedToNull(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	e.SetBreachObserver(nil)

	chain := []QuotaScope{{Scope: ScopeKey, EntityID: "alice", Quotas: QuotaConfig{RPMLimit: 1}}}
	require.True(t, mustCheck(e, chain).Allowed)
	require.False(t, mustCheck(e, chain).Allowed, "nil observer must not turn deny into allow")
}

// TestEnforcer_BreachObserver_BudgetThresholdFires pins that settling
// reservations past a budget threshold fires a budget_threshold_*
// informational breach event with the right envelope (limit/used/
// percent/period). One settlement that crosses two boundaries fires
// two events.
func TestEnforcer_BreachObserver_BudgetThresholdFires(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	obs := &captureObserver{}
	e.SetBreachObserver(obs)

	// $1 budget, $0.50 estimate per request — first reservation reserves
	// half the budget. Settling at $0.90 actual lifts SpendMicro past
	// 50% AND 80% in a single call, so we expect two events.
	chain := []QuotaScope{{
		Scope:    ScopeKey,
		EntityID: "thresh-key",
		Quotas:   QuotaConfig{SpendLimit: 1.0, ResetPeriod: "monthly", DefaultMaxTokens: 50_000},
	}}
	d, reservations := e.Check(chain)
	require.True(t, d.Allowed)
	require.Len(t, reservations, 1)

	e.SettleReservations(reservations, 900_000, 100, 50)

	obs.mu.Lock()
	defer obs.mu.Unlock()
	require.Len(t, obs.events, 2, "one settlement past 50%% AND 80%% emits two events")

	assert.Equal(t, "budget_threshold_50", obs.events[0].Reason)
	assert.Equal(t, uint8(50), obs.events[0].ThresholdPercent)
	assert.Equal(t, "thresh-key", obs.events[0].ScopeID)
	assert.Equal(t, "monthly", obs.events[0].BudgetPeriod)
	assert.Equal(t, int64(1_000_000), obs.events[0].SpendLimitMicro)
	assert.Equal(t, int64(900_000), obs.events[0].SpendUsedMicro)
	assert.False(t, obs.events[0].Time.IsZero())

	assert.Equal(t, "budget_threshold_80", obs.events[1].Reason)
	assert.Equal(t, uint8(80), obs.events[1].ThresholdPercent)
}

// TestEnforcer_BreachObserver_BudgetThresholdDedupes pins that two
// settlements past the same threshold fire exactly one event.
func TestEnforcer_BreachObserver_BudgetThresholdDedupes(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer()
	obs := &captureObserver{}
	e.SetBreachObserver(obs)

	chain := []QuotaScope{{
		Scope:    ScopeKey,
		EntityID: "dedup-key",
		Quotas:   QuotaConfig{SpendLimit: 10.0, ResetPeriod: "monthly", DefaultMaxTokens: 50_000},
	}}
	d1, res1 := e.Check(chain)
	require.True(t, d1.Allowed)
	e.SettleReservations(res1, 5_500_000, 0, 0) // crosses 50% ($5.50 / $10)

	d2, res2 := e.Check(chain)
	require.True(t, d2.Allowed)
	e.SettleReservations(res2, 500_000, 0, 0) // settles to $6.00, still inside 50% band

	obs.mu.Lock()
	defer obs.mu.Unlock()
	require.Len(t, obs.events, 1)
	assert.Equal(t, "budget_threshold_50", obs.events[0].Reason)
}

func mustCheck(e *Enforcer, chain []QuotaScope) Decision {
	d, _ := e.Check(chain)
	return d
}
