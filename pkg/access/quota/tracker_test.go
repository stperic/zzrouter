package quota

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpendTracker_ReserveAndSettle(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")

	scope := SpendScope{Kind: ScopeKey, EntityID: "key-1"}
	limitMicro := int64(10_000_000) // $10

	// Reserve $1 estimate
	ok := tr.ReserveBudget(scope, limitMicro, PeriodMonthly, 1_000_000)
	require.True(t, ok)

	state := tr.GetState(scope)
	require.NotNil(t, state)
	assert.Equal(t, int64(1_000_000), state.ReservedMicro)
	assert.Equal(t, int64(0), state.SpendMicro)

	// Settle with actual cost of $0.50
	tr.SettleReservation(scope, 1_000_000, 500_000, 100, 50)

	state = tr.GetState(scope)
	assert.Equal(t, int64(0), state.ReservedMicro)
	assert.Equal(t, int64(500_000), state.SpendMicro)
	assert.Equal(t, int64(100), state.TokensIn)
	assert.Equal(t, int64(50), state.TokensOut)
	assert.Equal(t, int64(1), state.RequestCount)
}

func TestSpendTracker_ReserveAndCancel(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")

	scope := SpendScope{Kind: ScopeKey, EntityID: "key-1"}
	limitMicro := int64(10_000_000)

	ok := tr.ReserveBudget(scope, limitMicro, PeriodMonthly, 1_000_000)
	require.True(t, ok)

	tr.CancelReservation(scope, 1_000_000)

	state := tr.GetState(scope)
	assert.Equal(t, int64(0), state.ReservedMicro)
	assert.Equal(t, int64(0), state.SpendMicro)
}

func TestSpendTracker_BudgetExhausted(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")

	scope := SpendScope{Kind: ScopeKey, EntityID: "key-1"}
	limitMicro := int64(1_000_000) // $1

	// Reserve $0.80
	ok := tr.ReserveBudget(scope, limitMicro, PeriodMonthly, 800_000)
	require.True(t, ok)

	// Try to reserve another $0.30 — should fail (0.80 + 0.30 > 1.00)
	ok = tr.ReserveBudget(scope, limitMicro, PeriodMonthly, 300_000)
	assert.False(t, ok)

	// Reserve $0.19 — should pass (0.80 + 0.19 < 1.00)
	ok = tr.ReserveBudget(scope, limitMicro, PeriodMonthly, 190_000)
	assert.True(t, ok)
}

func TestSpendTracker_ConcurrentReservations(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")

	scope := SpendScope{Kind: ScopeTeam, EntityID: "team-1"}
	limitMicro := int64(5_000_000) // $5

	// Reserve from multiple "requests"
	ok1 := tr.ReserveBudget(scope, limitMicro, PeriodMonthly, 2_000_000)
	ok2 := tr.ReserveBudget(scope, limitMicro, PeriodMonthly, 2_000_000)
	ok3 := tr.ReserveBudget(scope, limitMicro, PeriodMonthly, 2_000_000) // should fail

	assert.True(t, ok1)
	assert.True(t, ok2)
	assert.False(t, ok3) // 2+2+2 > 5
}

func TestSpendTracker_PeriodReset(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")

	scope := SpendScope{Kind: ScopeKey, EntityID: "key-1"}
	limitMicro := int64(1_000_000) // $1

	// Manually create an expired state
	tr.mu.Lock()
	key := scope.String()
	tr.states[key] = &SpendState{
		EntityID:    "key-1",
		Kind:        ScopeKey,
		SpendMicro:  900_000,                      // $0.90 spent
		PeriodStart: time.Now().AddDate(0, -2, 0), // 2 months ago
		Period:      PeriodMonthly,
	}
	tr.mu.Unlock()

	// Reserve should pass because period has expired (lazy reset)
	ok := tr.ReserveBudget(scope, limitMicro, PeriodMonthly, 500_000)
	assert.True(t, ok)

	state := tr.GetState(scope)
	assert.Equal(t, int64(0), state.SpendMicro) // reset
	assert.Equal(t, int64(500_000), state.ReservedMicro)
}

func TestSpendTracker_MicrodollarMath(t *testing.T) {
	t.Parallel()
	assert.Equal(t, int64(1_000_000), USDToMicro(1.0))
	assert.Equal(t, int64(500_000), USDToMicro(0.5))
	assert.Equal(t, int64(10), USDToMicro(0.00001))
	assert.Equal(t, 1.0, MicroToUSD(1_000_000))
	assert.Equal(t, 0.5, MicroToUSD(500_000))
}

func TestSpendTracker_ReaperZerosStaleReservations(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")

	scope := SpendScope{Kind: ScopeKey, EntityID: "key-stale"}

	// Create state with stale reservation
	tr.mu.Lock()
	key := scope.String()
	tr.states[key] = &SpendState{
		EntityID:       "key-stale",
		Kind:           ScopeKey,
		ReservedMicro:  1_000_000,
		LastReservedAt: time.Now().Add(-15 * time.Minute), // 15 min ago > 10 min TTL
		PeriodStart:    PeriodStartFor(time.Now(), PeriodMonthly),
		Period:         PeriodMonthly,
	}
	tr.mu.Unlock()

	// Run reaper
	tr.reapStaleReservations()

	state := tr.GetState(scope)
	require.NotNil(t, state)
	assert.Equal(t, int64(0), state.ReservedMicro) // reaped
}

func TestSpendTracker_ReaperKeepsRecentReservations(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")

	scope := SpendScope{Kind: ScopeKey, EntityID: "key-recent"}

	// Create state with fresh reservation
	tr.mu.Lock()
	key := scope.String()
	tr.states[key] = &SpendState{
		EntityID:       "key-recent",
		Kind:           ScopeKey,
		ReservedMicro:  1_000_000,
		LastReservedAt: time.Now().Add(-2 * time.Minute), // 2 min ago < 10 min TTL
		PeriodStart:    PeriodStartFor(time.Now(), PeriodMonthly),
		Period:         PeriodMonthly,
	}
	tr.mu.Unlock()

	tr.reapStaleReservations()

	state := tr.GetState(scope)
	require.NotNil(t, state)
	assert.Equal(t, int64(1_000_000), state.ReservedMicro) // kept
}

func TestSpendTracker_ResetScope(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")

	scope := SpendScope{Kind: ScopeKey, EntityID: "key-1"}
	tr.ReserveBudget(scope, 10_000_000, PeriodMonthly, 500_000)
	tr.SettleReservation(scope, 500_000, 300_000, 100, 50)

	tr.ResetScope(scope)

	state := tr.GetState(scope)
	require.NotNil(t, state)
	assert.Equal(t, int64(0), state.SpendMicro)
	assert.Equal(t, int64(0), state.ReservedMicro)
	assert.Equal(t, int64(0), state.RequestCount)
}

func TestSpendTracker_Thresholds_FireOnceThenDedup(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")
	scope := SpendScope{Kind: ScopeKey, EntityID: "thresh-dedup"}
	limit := int64(1_000_000) // $1

	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 1_000_000))

	// First settlement past 50% — fires once. Envelope mirrors post-settle state.
	first, env := tr.SettleAndDetectCrossings(scope, 1_000_000, 600_000, 0, 0)
	require.Equal(t, []uint8{50}, first)
	assert.Equal(t, int64(1_000_000), env.LimitMicro)
	assert.Equal(t, int64(600_000), env.SpendMicro)
	assert.Equal(t, int64(0), env.ReservedMicro)
	assert.Equal(t, PeriodMonthly, env.Period)

	// Add another reservation + settlement that stays inside the same band — no event.
	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 50_000))
	second, _ := tr.SettleAndDetectCrossings(scope, 50_000, 50_000, 0, 0)
	assert.Nil(t, second, "no new threshold crossings when staying inside same band")

	state := tr.GetState(scope)
	require.NotNil(t, state)
	assert.Equal(t, uint8(0b001), state.ThresholdsFired, "50%% bit set, 80/95 untouched")
}

func TestSpendTracker_Thresholds_AllThreeAcrossSettlements(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")
	scope := SpendScope{Kind: ScopeTeam, EntityID: "thresh-all"}
	limit := int64(1_000_000) // $1

	// 50% crossing
	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 1_000_000))
	crossed, _ := tr.SettleAndDetectCrossings(scope, 1_000_000, 500_000, 0, 0)
	require.Equal(t, []uint8{50}, crossed)

	// 80% crossing
	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 500_000))
	crossed, _ = tr.SettleAndDetectCrossings(scope, 500_000, 300_000, 0, 0)
	require.Equal(t, []uint8{80}, crossed)

	// 95% crossing
	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 200_000))
	crossed, _ = tr.SettleAndDetectCrossings(scope, 200_000, 150_000, 0, 0)
	require.Equal(t, []uint8{95}, crossed)

	state := tr.GetState(scope)
	assert.Equal(t, uint8(0b111), state.ThresholdsFired)
}

func TestSpendTracker_Thresholds_OneSettlementCrossesMultiple(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")
	scope := SpendScope{Kind: ScopeKey, EntityID: "thresh-batch"}
	limit := int64(1_000_000)

	// One large settlement that lands at 90% — crosses 50% AND 80% in a single call.
	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 1_000_000))
	crossed, _ := tr.SettleAndDetectCrossings(scope, 1_000_000, 900_000, 0, 0)
	assert.Equal(t, []uint8{50, 80}, crossed)
	assert.NotContains(t, crossed, uint8(95))
}

func TestSpendTracker_Thresholds_ReArmedOnPeriodReset(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")
	scope := SpendScope{Kind: ScopeKey, EntityID: "thresh-period"}
	limit := int64(1_000_000)

	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 1_000_000))
	first, _ := tr.SettleAndDetectCrossings(scope, 1_000_000, 500_000, 0, 0)
	require.Equal(t, []uint8{50}, first)

	// Force the period boundary backwards so the next ReserveBudget triggers the lazy reset.
	tr.mu.Lock()
	tr.states[scope.String()].PeriodStart = time.Now().AddDate(0, -2, 0)
	tr.mu.Unlock()

	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 1_000_000))
	second, _ := tr.SettleAndDetectCrossings(scope, 1_000_000, 500_000, 0, 0)
	assert.Equal(t, []uint8{50}, second, "period reset re-arms threshold so the next 50%% crossing fires again")
}

func TestSpendTracker_Thresholds_ReArmedOnResetScope(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")
	scope := SpendScope{Kind: ScopeKey, EntityID: "thresh-reset"}
	limit := int64(1_000_000)

	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 1_000_000))
	first, _ := tr.SettleAndDetectCrossings(scope, 1_000_000, 500_000, 0, 0)
	require.Equal(t, []uint8{50}, first)

	tr.ResetScope(scope)
	state := tr.GetState(scope)
	require.NotNil(t, state)
	assert.Equal(t, uint8(0), state.ThresholdsFired, "operator reset clears the bitfield")

	// Cross 50% again — should fire because the bitfield was cleared.
	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 1_000_000))
	second, _ := tr.SettleAndDetectCrossings(scope, 1_000_000, 500_000, 0, 0)
	assert.Equal(t, []uint8{50}, second)
}

func TestSpendTracker_Thresholds_NoLimitNoEvents(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")
	scope := SpendScope{Kind: ScopeKey, EntityID: "thresh-nolimit"}

	// State exists but never reserved — LimitMicro is zero, detection bails
	// but the envelope still snapshots whatever post-settle state we have.
	tr.mu.Lock()
	tr.states[scope.String()] = &SpendState{
		EntityID:    scope.EntityID,
		Kind:        scope.Kind,
		PeriodStart: PeriodStartFor(time.Now(), PeriodMonthly),
		Period:      PeriodMonthly,
	}
	tr.mu.Unlock()

	crossed, env := tr.SettleAndDetectCrossings(scope, 0, 500_000, 0, 0)
	assert.Nil(t, crossed)
	assert.Equal(t, int64(0), env.LimitMicro)
	assert.Equal(t, int64(500_000), env.SpendMicro)
}

// Settlement that arrives after the period boundary (no intervening
// reserve to roll the period over) must reset spend + threshold bits
// before applying the new actualCost. Without the reset, last
// period's accumulated SpendMicro carries forward into the new
// period's accounting AND the bitfield silently dedups the new
// period's first crossing.
func TestSpendTracker_Thresholds_PeriodRolledByLateSettleAlone(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")
	scope := SpendScope{Kind: ScopeKey, EntityID: "thresh-late"}
	limit := int64(1_000_000)

	require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, 1_000_000))
	first, _ := tr.SettleAndDetectCrossings(scope, 1_000_000, 600_000, 0, 0)
	require.Equal(t, []uint8{50}, first, "set the 50%% bit in the original period")

	// Push the period boundary backwards so any subsequent operation
	// sees the period as expired. Caller does NOT call ReserveBudget
	// — only a settle arrives. The settle path itself must trigger
	// the reset and re-arm threshold detection.
	tr.mu.Lock()
	tr.states[scope.String()].PeriodStart = time.Now().AddDate(0, -2, 0)
	tr.mu.Unlock()

	// Settle into the would-be next period without a reserve. Spend and
	// thresholds should reset; this 700µ actualCost should be the entire
	// new-period spend and trigger 50%.
	crossed, env := tr.SettleAndDetectCrossings(scope, 0, 700_000, 0, 0)
	assert.Equal(t, []uint8{50}, crossed, "late settle re-arms thresholds for the new period")
	assert.Equal(t, int64(700_000), env.SpendMicro, "new-period spend equals only the late settle")
}

func TestSpendTracker_Thresholds_ConcurrentSettlementsFireExactlyOnce(t *testing.T) {
	t.Parallel()
	tr := NewSpendTracker("")
	scope := SpendScope{Kind: ScopeKey, EntityID: "thresh-race"}
	limit := int64(10_000_000) // $10

	// Pre-fund 50 reservations of $0.05 each — when settled they total
	// $5 (50% of limit), exactly at the threshold. Race the settlements
	// from many goroutines and assert exactly one returns the {50}
	// crossing — the rest see the bit already set.
	const workers = 50
	const perWorker = 100_000 // $0.10 settlement
	reservedEach := int64(50_000)
	for range workers {
		require.True(t, tr.ReserveBudget(scope, limit, PeriodMonthly, reservedEach))
	}

	var fired atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			crossed, _ := tr.SettleAndDetectCrossings(scope, reservedEach, perWorker, 0, 0)
			for _, pct := range crossed {
				if pct == 50 {
					fired.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), fired.Load(), "exactly one settlement claims the 50%% threshold")
}

func TestSpendTracker_PersistAndLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "spend.json")

	// Create and populate tracker
	tr := NewSpendTracker(path)
	scope := SpendScope{Kind: ScopeKey, EntityID: "key-persist"}
	tr.ReserveBudget(scope, 10_000_000, PeriodMonthly, 500_000)
	tr.SettleReservation(scope, 500_000, 300_000, 100, 50)

	// Force save
	tr.save()

	// Verify file exists
	_, err := os.Stat(path)
	require.NoError(t, err)

	// Load into new tracker
	tr2 := NewSpendTracker(path)
	state := tr2.GetState(scope)
	require.NotNil(t, state)
	assert.Equal(t, int64(300_000), state.SpendMicro)
	assert.Equal(t, int64(100), state.TokensIn)
}

// TestSpendTracker_PersistAndLoad_PreservesThresholds pins that
// LimitMicro and ThresholdsFired survive a coordinator restart.
// Without persistence, the bitfield gets cleared and the next
// settlement past 50% would re-fire after restart — duplicate event
// for the same period.
func TestSpendTracker_PersistAndLoad_PreservesThresholds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "spend.json")

	tr := NewSpendTracker(path)
	scope := SpendScope{Kind: ScopeKey, EntityID: "key-thresh-persist"}
	require.True(t, tr.ReserveBudget(scope, 10_000_000, PeriodMonthly, 1_000_000))
	crossed, _ := tr.SettleAndDetectCrossings(scope, 1_000_000, 6_000_000, 0, 0) // crosses 50%
	require.Equal(t, []uint8{50}, crossed)

	tr.save()

	tr2 := NewSpendTracker(path)
	state := tr2.GetState(scope)
	require.NotNil(t, state)
	assert.Equal(t, int64(10_000_000), state.LimitMicro, "limit cached on reservation must survive reload")
	assert.Equal(t, uint8(0b001), state.ThresholdsFired, "fired-bit must survive reload so post-restart settle dedups")

	// Settle past 50% again on the restored tracker — must NOT re-fire.
	require.True(t, tr2.ReserveBudget(scope, 10_000_000, PeriodMonthly, 100_000))
	again, _ := tr2.SettleAndDetectCrossings(scope, 100_000, 100_000, 0, 0)
	assert.Empty(t, again, "post-restart settle in the same period must dedup against persisted bitfield")
}
