package quota

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	persistInterval     = 60 * time.Second
	reaperInterval      = 5 * time.Minute
	reservationStaleTTL = 10 * time.Minute
)

// SpendTracker manages per-scope spend state in memory with periodic disk persistence.
// Budget reservation pattern: reserve before request, settle/cancel after response.
//
// Lifecycle: Start(ctx) launches persist and reaper goroutines; Stop(ctx)
// cancels them and performs a final save. Restart-safe: Start→Stop→Start cycles cleanly.
type SpendTracker struct {
	mu     sync.Mutex
	states map[string]*SpendState // SpendScope.String() → state
	dirty  bool
	path   string // spend.json

	lifecycleMu sync.Mutex
	lifeCtx     context.Context
	lifeCancel  context.CancelFunc
	// wg tracks persistLoop + reaperLoop so Stop blocks until both
	// exit. The final t.save() in Stop must run AFTER both loops are
	// quiescent — otherwise persistLoop could race save() on the
	// state map.
	wg sync.WaitGroup
}

// NewSpendTracker creates a spend tracker. Loads existing state from path if present.
func NewSpendTracker(path string) *SpendTracker {
	t := &SpendTracker{
		states: make(map[string]*SpendState),
		path:   path,
	}

	if path != "" {
		switch file, err := LoadSpend(path); {
		case err == nil:
			t.states = file.States
			slog.Info("Spend state loaded from disk", "scopes", len(t.states), "path", path)
		case errors.Is(err, fs.ErrNotExist):
			// First run — no file yet. Silently start fresh.
		default:
			slog.Warn("Failed to load spend state, starting fresh", "error", err, "path", path)
		}
	}

	return t
}

// Start begins the persist loop and reservation reaper goroutines.
// Calling Start twice on a running tracker is a no-op.
// Restart-safe: may be called again after Stop.
func (t *SpendTracker) Start(ctx context.Context) {
	t.lifecycleMu.Lock()
	if t.lifeCancel != nil { // already started
		t.lifecycleMu.Unlock()
		return
	}
	t.lifeCtx, t.lifeCancel = context.WithCancel(ctx)
	t.lifecycleMu.Unlock()

	t.wg.Add(2)
	go func() { defer t.wg.Done(); t.persistLoop() }()
	go func() { defer t.wg.Done(); t.reaperLoop() }()
}

// Stop terminates background goroutines and performs a final save.
// Safe to call multiple times and safe to call when Start was never invoked.
// Restart-safe: a subsequent Start will re-launch the goroutines.
func (t *SpendTracker) Stop(_ context.Context) {
	// Cancel first, then clear lifeCancel under lock. lifeCtx MUST NOT
	// be niled: persistLoop + reaperLoop read t.lifeCtx.Done() at the
	// head of every select iteration, and niling it would race their
	// exit — they observe a cancelled ctx and return cleanly on their
	// own. A subsequent Start re-assigns lifeCtx to a fresh derived
	// context, which is safe because lifeCancel==nil below gates the
	// duplicate-start short-circuit check.
	t.lifecycleMu.Lock()
	cancel := t.lifeCancel
	t.lifeCancel = nil
	t.lifecycleMu.Unlock()

	if cancel != nil {
		cancel()
	}
	// Wait for persistLoop + reaperLoop to exit so the final save
	// below cannot race an in-flight periodic persist against the
	// state map.
	t.wg.Wait()
	t.save()
}

// Flush writes any dirty spend state to disk immediately. Same code path
// as the Stop-time final save and the periodic persistLoop tick — the
// shutdown ordering calls this before HTTP drain so a slow drain can't
// push the flush into systemd's SIGKILL window. Safe to call while the
// tracker is still running.
func (t *SpendTracker) Flush() {
	t.save()
}

// ReserveBudget atomically checks (SpendMicro + ReservedMicro < limitMicro)
// and increments ReservedMicro. Returns true if the reservation was granted.
//
// limitMicro is cached on the state so SettleAndDetectCrossings can
// compute threshold % without a re-fetch. ThresholdsFired is zeroed
// on the lazy period reset so a fresh period re-arms the crossings.
func (t *SpendTracker) ReserveBudget(scope SpendScope, limitMicro int64, period BudgetPeriod, estimatedCostMicro int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := scope.String()
	state := t.getOrCreate(key, scope, period)

	// Lazy period reset
	if IsExpired(state.PeriodStart, state.Period) {
		state.SpendMicro = 0
		state.ReservedMicro = 0
		state.TokensIn = 0
		state.TokensOut = 0
		state.RequestCount = 0
		state.ThresholdsFired = 0
		state.PeriodStart = PeriodStartFor(utils.Now(), period)
		state.Period = period
	}

	if state.SpendMicro+state.ReservedMicro+estimatedCostMicro > limitMicro {
		return false
	}

	state.ReservedMicro += estimatedCostMicro
	state.LastReservedAt = utils.NowUTC()
	state.LimitMicro = limitMicro
	t.dirty = true
	return true
}

// SettleReservation decrements ReservedMicro by estimated, increments SpendMicro by actual.
func (t *SpendTracker) SettleReservation(scope SpendScope, estimatedCostMicro, actualCostMicro int64, tokensIn, tokensOut int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.settleLocked(scope, estimatedCostMicro, actualCostMicro, tokensIn, tokensOut)
}

// SettleEnvelope is a captured-under-lock snapshot of post-settle
// spend state, returned alongside the crossings so the caller emits
// BreachEvents whose envelope (used / held / limit / period) matches
// the basis the threshold was claimed against. Returning a snapshot
// closes the read-after-unlock race a separate GetState() would open.
type SettleEnvelope struct {
	LimitMicro    int64
	SpendMicro    int64
	ReservedMicro int64
	Period        BudgetPeriod
}

// SettleAndDetectCrossings is SettleReservation plus atomic detection
// of newly-crossed budget thresholds. Returns the percentages that
// crossed for the first time this period (subset of the threshold
// list) AND a snapshot of the post-settle envelope. Caller emits
// events — the tracker only tracks state.
//
// Detection runs INSIDE the lock so two concurrent settlements that
// both push past a threshold can't both emit; the first claims the
// bit, the second sees it set and returns nothing for that
// percentage. The envelope is captured in the same lock acquisition
// so the per-event SpendUsedMicro / SpendHeldMicro values describe
// exactly the basis the bit was claimed against.
//
// Returns no crossings when LimitMicro is zero (no reservation has
// ever granted this scope a limit) — without a basis there's no way
// to compute %. The envelope is still populated.
func (t *SpendTracker) SettleAndDetectCrossings(scope SpendScope, estimatedCostMicro, actualCostMicro int64, tokensIn, tokensOut int64) ([]uint8, SettleEnvelope) {
	t.mu.Lock()
	defer t.mu.Unlock()

	state := t.settleLocked(scope, estimatedCostMicro, actualCostMicro, tokensIn, tokensOut)
	if state == nil {
		return nil, SettleEnvelope{}
	}

	envelope := SettleEnvelope{
		LimitMicro:    state.LimitMicro,
		SpendMicro:    state.SpendMicro,
		ReservedMicro: state.ReservedMicro,
		Period:        state.Period,
	}

	if state.LimitMicro <= 0 {
		return nil, envelope
	}

	// Integer percentage check against microdollar limit. The form
	// `used >= limit/100*pct` avoids overflow on extreme limits at
	// the cost of ≤1µ × pct ≈ $0.0001 of precision near the boundary
	// — well below any realistic threshold step.
	used := state.SpendMicro + state.ReservedMicro
	pctBasis := state.LimitMicro / 100
	var crossed []uint8
	for i, pct := range budgetThresholdPercents {
		bit := byte(1) << i
		if state.ThresholdsFired&bit != 0 {
			continue // already fired this period
		}
		if used >= pctBasis*int64(pct) {
			state.ThresholdsFired |= bit
			t.dirty = true
			crossed = append(crossed, pct)
		}
	}
	return crossed, envelope
}

// settleLocked is the body of SettleReservation with the caller
// holding t.mu. Returns the state pointer (nil when scope is unknown)
// so SettleAndDetectCrossings can run threshold checks without a
// second map lookup.
//
// Mirrors ReserveBudget's lazy period-reset path: a settle that
// arrives after the period boundary (no intervening reserve to roll
// the period over) zeros spend + threshold bits BEFORE applying the
// new actualCost. Without this, a request that straddles midnight
// would have its actualCost added to the prior period's accumulated
// total and threshold detection would dedup against last period's
// bitfield.
func (t *SpendTracker) settleLocked(scope SpendScope, estimatedCostMicro, actualCostMicro int64, tokensIn, tokensOut int64) *SpendState {
	key := scope.String()
	state := t.states[key]
	if state == nil {
		return nil
	}

	if IsExpired(state.PeriodStart, state.Period) {
		state.SpendMicro = 0
		state.ReservedMicro = 0
		state.TokensIn = 0
		state.TokensOut = 0
		state.RequestCount = 0
		state.ThresholdsFired = 0
		state.PeriodStart = PeriodStartFor(utils.Now(), state.Period)
	}

	state.ReservedMicro -= estimatedCostMicro
	if state.ReservedMicro < 0 {
		state.ReservedMicro = 0
	}
	state.SpendMicro += actualCostMicro
	state.TokensIn += tokensIn
	state.TokensOut += tokensOut
	state.RequestCount++
	t.dirty = true
	return state
}

// CancelReservation decrements ReservedMicro (failed/cancelled request).
func (t *SpendTracker) CancelReservation(scope SpendScope, estimatedCostMicro int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := scope.String()
	state := t.states[key]
	if state == nil {
		return
	}

	state.ReservedMicro -= estimatedCostMicro
	if state.ReservedMicro < 0 {
		state.ReservedMicro = 0
	}
	t.dirty = true
}

// GetState returns a copy of the spend state for a scope, or nil.
func (t *SpendTracker) GetState(scope SpendScope) *SpendState {
	t.mu.Lock()
	defer t.mu.Unlock()

	state := t.states[scope.String()]
	if state == nil {
		return nil
	}
	cp := *state
	return &cp
}

// GetAllStates returns a copy of all tracked states.
func (t *SpendTracker) GetAllStates() map[string]*SpendState {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := make(map[string]*SpendState, len(t.states))
	for k, v := range t.states {
		cp := *v
		result[k] = &cp
	}
	return result
}

// ForgetEntity removes every tracked row for the given (kind, entityID),
// including per-model sub-rows. Called when a key or team is deleted so its
// spend ledger doesn't linger in memory (and get re-serialized to spend.json)
// forever. No-op if no rows match.
func (t *SpendTracker) ForgetEntity(kind ScopeKind, entityID string) {
	if entityID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	removed := 0
	for key, state := range t.states {
		if state.Kind == kind && state.EntityID == entityID {
			delete(t.states, key)
			removed++
		}
	}
	if removed > 0 {
		t.dirty = true
	}
}

// ResetScope clears the spend for a scope. Threshold bits are cleared
// alongside so the operator-driven reset re-arms threshold events for
// the fresh period that starts here (otherwise an admin who reset
// after only the 50% bit fired would never see 80% / 95% again until
// the natural period boundary).
func (t *SpendTracker) ResetScope(scope SpendScope) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := scope.String()
	if state, ok := t.states[key]; ok {
		state.SpendMicro = 0
		state.ReservedMicro = 0
		state.TokensIn = 0
		state.TokensOut = 0
		state.RequestCount = 0
		state.ThresholdsFired = 0
		state.PeriodStart = PeriodStartFor(utils.Now(), state.Period)
		t.dirty = true
	}
}

func (t *SpendTracker) getOrCreate(key string, scope SpendScope, period BudgetPeriod) *SpendState {
	state := t.states[key]
	if state == nil {
		state = &SpendState{
			EntityID:    scope.EntityID,
			Kind:        scope.Kind,
			PeriodStart: PeriodStartFor(utils.Now(), period),
			Period:      period,
		}
		t.states[key] = state
	}
	return state
}

func (t *SpendTracker) persistLoop() {
	ticker := time.NewTicker(persistInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.save()
		case <-t.lifeCtx.Done():
			return
		}
	}
}

func (t *SpendTracker) reaperLoop() {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.reapStaleReservations()
		case <-t.lifeCtx.Done():
			return
		}
	}
}

// reapStaleReservations zeros ReservedMicro for any scope where
// LastReservedAt is older than reservationStaleTTL.
func (t *SpendTracker) reapStaleReservations() {
	t.mu.Lock()
	defer t.mu.Unlock()

	cutoff := utils.Now().Add(-reservationStaleTTL)
	for _, state := range t.states {
		if state.ReservedMicro > 0 && state.LastReservedAt.Before(cutoff) {
			slog.Info("Reaping stale budget reservation",
				"entity", state.EntityID, "kind", state.Kind,
				"reserved_micro", state.ReservedMicro)
			state.ReservedMicro = 0
			t.dirty = true
		}
	}
}

func (t *SpendTracker) save() {
	if t.path == "" {
		return
	}

	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return
	}

	file := &SpendFile{
		Version: 1,
		States:  make(map[string]*SpendState, len(t.states)),
	}
	for k, v := range t.states {
		cp := *v
		file.States[k] = &cp
	}
	// Clear dirty up front so concurrent writes that land between here and
	// the I/O below can still mark the tracker dirty for the next tick.
	t.dirty = false
	t.mu.Unlock()

	if err := SaveSpend(t.path, file); err != nil {
		slog.Warn("Failed to persist spend state", "error", err)
		// Revert the dirty flag so we retry on the next save tick. A
		// concurrent writer may have already set it, which is fine — we
		// store-true unconditionally and the outcome is the same.
		t.mu.Lock()
		t.dirty = true
		t.mu.Unlock()
	}
}
