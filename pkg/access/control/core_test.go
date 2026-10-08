package control

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
)

// newTestCore returns a Core backed by empty tmp stores and a real
// (in-memory) quota.Enforcer. The reaper is NOT started — tests that
// want to exercise reap behaviour call reapStalePendingReservations
// directly with an injected "now".
func newTestCore(t *testing.T, staticKeys StaticKeySet) (*Core, *keys.FileKeyStore, *teams.FileTeamStore) {
	t.Helper()
	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	teamStore := teams.NewFileTeamStore(filepath.Join(dir, "teams.yaml"))
	enforcer := quota.NewEnforcer(
		quota.NewRateLimiter(),
		quota.NewConcurrencyLimiter(),
		quota.NewSpendTracker(""),
	)
	c := NewCore(staticKeys, keyStore, teamStore, nil, enforcer, nil)
	t.Cleanup(func() { c.Stop(context.Background()) })
	return c, keyStore, teamStore
}

func TestCore_Authenticate_StaticAdmin(t *testing.T) {
	t.Parallel()
	const adminKey = "admin-key-value-of-sufficient-length"
	c, _, _ := newTestCore(t, StaticKeySet{Admin: adminKey})

	ac, err := c.Authenticate(adminKey)
	require.NoError(t, err)
	require.NotNil(t, ac)
	require.NotNil(t, ac.Key)
	assert.Equal(t, "admin", ac.Key.ID)
	assert.Equal(t, RoleAdmin, ac.Key.Role)
	assert.False(t, ac.Key.IsVirtual)
	assert.Nil(t, ac.Team, "static keys have no team")
}

func TestCore_Authenticate_EmptyKey(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})
	_, err := c.Authenticate("")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyKey)
}

func TestCore_Authenticate_InvalidKey(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})
	_, err := c.Authenticate("totally-wrong")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidKey)
}

func TestCore_Enforce_SuspendedKey(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	ac := &AccessContext{
		Key: &KeyPrincipal{
			ID:        "vk-1",
			Role:      RoleUser,
			IsVirtual: true,
			Suspended: true,
		},
	}
	d, release, _ := c.Enforce(ac, "some-model")
	assert.False(t, d.Allowed)
	assert.Equal(t, DenyKeySuspended, d.Reason)
	assert.Contains(t, d.Message, "suspended")
	assert.Nil(t, release, "denied enforce must not return a release func")
}

func TestCore_Enforce_SuspendedTeam(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	ac := &AccessContext{
		Key: &KeyPrincipal{ID: "vk-1", Role: RoleUser, IsVirtual: true},
		Team: &TeamPrincipal{
			ID:        "team-1",
			Suspended: true,
		},
	}
	d, _, _ := c.Enforce(ac, "some-model")
	assert.False(t, d.Allowed)
	assert.Equal(t, DenyTeamSuspended, d.Reason)
}

func TestCore_Enforce_ExpiredKey(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	past := time.Now().Add(-time.Hour)
	ac := &AccessContext{
		Key: &KeyPrincipal{
			ID:        "vk-1",
			Role:      RoleUser,
			IsVirtual: true,
			ExpiresAt: &past,
		},
	}
	d, _, _ := c.Enforce(ac, "some-model")
	assert.False(t, d.Allowed)
	assert.Equal(t, DenyKeyExpired, d.Reason)
}

func TestCore_Enforce_ModelAccessDenied(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	ac := &AccessContext{
		Key: &KeyPrincipal{ID: "vk-1", Role: RoleUser, IsVirtual: true},
		Team: &TeamPrincipal{
			ID:            "team-1",
			AllowedModels: []string{"allowed-model"},
		},
	}
	d, _, _ := c.Enforce(ac, "other-model")
	assert.False(t, d.Allowed)
	assert.Equal(t, DenyModelAccess, d.Reason)
}

func TestCore_Enforce_Happy(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	ac := &AccessContext{
		Key: &KeyPrincipal{ID: "vk-1", Role: RoleUser, IsVirtual: true},
	}
	d, release, _ := c.Enforce(ac, "any-model")
	require.True(t, d.Allowed)
	require.NotNil(t, release, "allowed enforce must return a release func")
	release() // no panic
}

func TestDenialReason_String(t *testing.T) {
	t.Parallel()
	// Lock the string-id stability: logs and tests depend on these exact values.
	cases := map[DenialReason]string{
		DenyNone:           "none",
		DenyKeySuspended:   "key_suspended",
		DenyTeamSuspended:  "team_suspended",
		DenyKeyExpired:     "key_expired",
		DenyModelAccess:    "model_access_denied",
		DenyRPM:            "rpm_limit_exceeded",
		DenyTPM:            "tpm_limit_exceeded",
		DenyBudget:         "budget_exhausted",
		DenyConcurrency:    "concurrency_limit_exceeded",
		DenyAnonymousGated: "anonymous_gated",
	}
	for r, want := range cases {
		assert.Equal(t, want, r.String())
	}
}

func TestTranslateQuotaReason(t *testing.T) {
	t.Parallel()
	// Contract with pkg/access/quota: if quota adds a new reason string,
	// the default branch keeps the user unblocked with a 429-equivalent.
	assert.Equal(t, DenyRPM, translateQuotaReason("rpm_limit_exceeded"))
	assert.Equal(t, DenyTPM, translateQuotaReason("tpm_limit_exceeded"))
	assert.Equal(t, DenyBudget, translateQuotaReason("budget_exhausted"))
	assert.Equal(t, DenyConcurrency, translateQuotaReason("concurrency_limit_exceeded"))
	assert.Equal(t, DenyRPM, translateQuotaReason("unknown-reason-from-future"))
}

// TestCore_Enforce_ConcurrencyFailureCancelsBudget locks in the
// canonical step-5 invariant: if concurrency acquisition fails AFTER a
// budget reservation was taken in step 4, that reservation must be
// cancelled before returning. Without this, a key would hemorrhage
// budget to failed-acquire attempts over time.
func TestCore_Enforce_ConcurrencyFailureCancelsBudget(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	// Key with MaxParallelRequests=1 + SpendLimit set so a budget
	// reservation is actually taken in step 4.
	quotas := quota.QuotaConfig{
		MaxParallelRequests: 1,
		SpendLimit:          100,  // $100 — plenty of headroom, reservation succeeds
		DefaultMaxTokens:    1024, // so the reservation has a non-zero estimate
	}
	ac := &AccessContext{
		Key: &KeyPrincipal{ID: "vk-cc", Role: RoleUser, IsVirtual: true, Quotas: quotas},
	}

	// First enforce — grabs the only concurrency slot.
	d1, release1, _ := c.Enforce(ac, "model-a")
	require.True(t, d1.Allowed)
	require.NotNil(t, release1)
	defer release1()

	// Pre-assert: exactly one stash for this key (reservation taken in step 4).
	c.pendingMu.Lock()
	assert.Len(t, c.pendingReservations[ac.Key.ID], 1, "first enforce must stash")
	c.pendingMu.Unlock()

	// Second enforce — step 4 will succeed (budget has room), step 5
	// must fail (slot already held), and the reservation from this
	// second Check call must be cancelled, NOT stashed.
	d2, release2, _ := c.Enforce(ac, "model-a")
	require.False(t, d2.Allowed)
	require.Equal(t, DenyConcurrency, d2.Reason)
	assert.Nil(t, release2, "denied enforce returns no release")

	// The second call's reservation must NOT have been stashed — the
	// queue length stays at 1, not 2. This is the cancel-on-failure
	// invariant: if the second reservation had leaked into the stash,
	// subsequent FIFO settles would drift against real traffic.
	c.pendingMu.Lock()
	assert.Len(t, c.pendingReservations[ac.Key.ID], 1,
		"concurrency failure must cancel the budget reservation, not stash it")
	c.pendingMu.Unlock()
}

// TestCore_PendingReservations_PopByID locks in that
// popPendingReservation returns the entry matching the ID the caller
// supplies — the contract the inference log bridge relies on when
// settling against multiple in-flight requests for the same key
// without FIFO cross-settlement.
func TestCore_PendingReservations_PopByID(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})
	const keyID = "vk-byid"

	c.pendingMu.Lock()
	c.pendingReservations[keyID] = map[ReservationID]pendingReservationEntry{
		ReservationID(1): {reservations: []quota.ReservationRecord{{}}, createdAt: time.Now().Add(-2 * time.Second)},
		ReservationID(2): {reservations: []quota.ReservationRecord{{}, {}}, createdAt: time.Now().Add(-1 * time.Second)},
	}
	c.pendingMu.Unlock()

	// Pop the second-stashed one first — order of stash no longer
	// dictates order of pop.
	got := c.popPendingReservation(keyID, ReservationID(2))
	assert.Len(t, got, 2, "pop returns the 2-reservation batch keyed by ID 2")

	got = c.popPendingReservation(keyID, ReservationID(1))
	assert.Len(t, got, 1, "pop by ID 1 returns the 1-reservation batch")

	// Pop an already-drained ID → nil; map entry for key must be gone.
	got = c.popPendingReservation(keyID, ReservationID(1))
	assert.Nil(t, got)
	c.pendingMu.Lock()
	_, stillPresent := c.pendingReservations[keyID]
	c.pendingMu.Unlock()
	assert.False(t, stillPresent, "emptied key map must be removed")
}

// TestCore_PopPendingReservation_NoReservation pins that popping with
// NoReservation is a safe no-op — CancelPending relies on this.
func TestCore_PopPendingReservation_NoReservation(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})
	got := c.popPendingReservation("any-key", NoReservation)
	assert.Nil(t, got)
}

// TestCore_ReapStalePendingReservations drives reapStalePendingReservations
// with an injected "now" and asserts TTL-based cancellation. The reaper
// is the only bounded-memory guarantee on the stash; regressing it
// silently would leak budget reservations indefinitely under handler-
// error paths.
func TestCore_ReapStalePendingReservations(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	// Anchor on pendingReservationTTL so the test doesn't drift if the
	// constant is retuned. Fresh is half-TTL; stale is 2× TTL.
	fresh := time.Now().Add(-pendingReservationTTL / 2)
	stale := time.Now().Add(-2 * pendingReservationTTL)

	c.pendingMu.Lock()
	c.pendingReservations["vk-a"] = map[ReservationID]pendingReservationEntry{
		ReservationID(1): {reservations: []quota.ReservationRecord{{}}, createdAt: stale},
	}
	c.pendingReservations["vk-b"] = map[ReservationID]pendingReservationEntry{
		ReservationID(2): {reservations: []quota.ReservationRecord{{}}, createdAt: fresh},
		ReservationID(3): {reservations: []quota.ReservationRecord{{}}, createdAt: stale},
	}
	c.pendingMu.Unlock()

	reaped := c.reapStalePendingReservations(time.Now())
	assert.Equal(t, 2, reaped, "both stale entries must be reaped")

	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	_, vkAStillPresent := c.pendingReservations["vk-a"]
	assert.False(t, vkAStillPresent, "vk-a had only stale entries; map row removed")
	assert.Len(t, c.pendingReservations["vk-b"], 1, "vk-b fresh entry survives")
}

// TestCore_PendingReservations_ConcurrentSameKey hammers Enforce +
// RecordSpendByKey concurrently for a single key and asserts each
// settle lands on the reservation stash its own Enforce created — no
// FIFO cross-settlement. This is the core correctness property the
// per-reservation-ID refactor buys us: a handler that errors out
// mid-flight can no longer steal a sibling request's settlement.
func TestCore_PendingReservations_ConcurrentSameKey(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	quotas := quota.QuotaConfig{
		SpendLimit:       1000, // very large — reservations never exhaust
		DefaultMaxTokens: 100,
	}
	ac := &AccessContext{
		Key: &KeyPrincipal{ID: "vk-race", Role: RoleUser, IsVirtual: true, Quotas: quotas},
	}

	const N = 50
	ids := make(chan ReservationID, N)
	var wg sync.WaitGroup
	wg.Add(N)

	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			d, release, id := c.Enforce(ac, "model-race")
			require.True(t, d.Allowed)
			if release != nil {
				release()
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)

	c.pendingMu.Lock()
	queueLen := len(c.pendingReservations[ac.Key.ID])
	c.pendingMu.Unlock()
	assert.Equal(t, N, queueLen, "each Enforce must stash one reservation under a unique ID")

	// Now settle each one by its own ID concurrently.
	collected := make([]ReservationID, 0, N)
	for id := range ids {
		collected = append(collected, id)
	}
	var settleWg sync.WaitGroup
	settleWg.Add(len(collected))
	for _, id := range collected {
		go func() {
			defer settleWg.Done()
			c.RecordSpendByKey(ac.Key.ID, id, 1000, 10, 20)
		}()
	}
	settleWg.Wait()

	c.pendingMu.Lock()
	_, present := c.pendingReservations[ac.Key.ID]
	c.pendingMu.Unlock()
	assert.False(t, present, "every stash drained exactly once by its matching settle")

	state := c.GetKeySpend(ac.Key.ID)
	require.NotNil(t, state)
	assert.EqualValues(t, N, state.RequestCount,
		"RequestCount must equal completed Enforce+Settle pairs")
}

// TestCore_PendingReservations_CancelPending_Concurrent asserts that
// racing CancelPending and RecordSpendByKey against the same key's
// distinct stashes doesn't double-pop. Each reservation has its own
// ID; half are settled, half cancelled — the stash must drain to empty
// with no leftover entries.
func TestCore_PendingReservations_CancelPending_Concurrent(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	quotas := quota.QuotaConfig{
		SpendLimit:       1000,
		DefaultMaxTokens: 100,
	}
	ac := &AccessContext{
		Key: &KeyPrincipal{ID: "vk-race-cancel", Role: RoleUser, IsVirtual: true, Quotas: quotas},
	}

	const N = 30
	ids := make([]ReservationID, 0, N)
	var idsMu sync.Mutex
	var enforceWg sync.WaitGroup
	enforceWg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer enforceWg.Done()
			_, release, id := c.Enforce(ac, "model-race")
			if release != nil {
				release()
			}
			idsMu.Lock()
			ids = append(ids, id)
			idsMu.Unlock()
		}()
	}
	enforceWg.Wait()

	var drainWg sync.WaitGroup
	drainWg.Add(len(ids))
	for i, id := range ids {
		if i%2 == 0 {
			go func() {
				defer drainWg.Done()
				c.RecordSpendByKey(ac.Key.ID, id, 500, 5, 10)
			}()
		} else {
			go func() {
				defer drainWg.Done()
				c.CancelPending(ac.Key.ID, id)
			}()
		}
	}
	drainWg.Wait()

	c.pendingMu.Lock()
	_, present := c.pendingReservations[ac.Key.ID]
	c.pendingMu.Unlock()
	assert.False(t, present, "every stash drained by exactly one of "+
		"RecordSpendByKey or CancelPending; leftover entries indicate double-pop or miss")
}

// TestCore_LateSettle_AfterReap pins the documented failure mode: if
// the reaper cancels a stash and the log bridge fires later with the
// same ID, RecordSpendByKey silently skips the settle (no cross-
// settle, no panic) but still records TPM tokens so rate-limit math
// stays honest.
func TestCore_LateSettle_AfterReap(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	quotas := quota.QuotaConfig{
		SpendLimit:       1000,
		DefaultMaxTokens: 100,
	}
	ac := &AccessContext{
		Key: &KeyPrincipal{ID: "vk-late", Role: RoleUser, IsVirtual: true, Quotas: quotas},
	}
	_, release, id := c.Enforce(ac, "model-late")
	require.NotEqual(t, NoReservation, id, "expected a stash id when SpendLimit is set")
	release()

	// Age the stash past TTL and run the reaper.
	c.pendingMu.Lock()
	keyed := c.pendingReservations[ac.Key.ID]
	entry := keyed[id]
	entry.createdAt = time.Now().Add(-2 * pendingReservationTTL)
	keyed[id] = entry
	c.pendingMu.Unlock()

	reaped := c.reapStalePendingReservations(time.Now())
	assert.Equal(t, 1, reaped, "reaper must cancel the aged stash")

	// Bridge fires late with the same ID + a real cost. Settle must
	// not panic, must not charge the spend ledger a second time (the
	// reaper already cancelled), and must leave the stash map empty.
	require.NotPanics(t, func() {
		c.RecordSpendByKey(ac.Key.ID, id, 1000, 10, 20)
	}, "late settle after reap must not panic")

	c.pendingMu.Lock()
	_, present := c.pendingReservations[ac.Key.ID]
	c.pendingMu.Unlock()
	assert.False(t, present, "no stash row should linger after reap + late settle")

	// The spend ledger either has no row (reaper cancelled the only
	// reservation, no settlement ever landed) or a zero-spend row —
	// either is acceptable. The critical invariant is that we did NOT
	// double-charge.
	state := c.GetKeySpend(ac.Key.ID)
	if state != nil {
		assert.EqualValues(t, 0, state.SpendMicro,
			"reaped stash must not produce spend on late settle")
		assert.EqualValues(t, 0, state.ReservedMicro,
			"no reservation should remain after reap")
	}
}

// TestCore_SettleWithinTTL_RecordsFullLedger pins the cold-load
// regression: a settlement that arrives after the
// stash is created but BEFORE the reaper TTL elapses must update the
// full spend ledger (RequestCount, TokensIn/Out, SpendUSD). The
// original 30s TTL was tight enough that a 35s cold model launch
// raced the reaper; the regression manifested as the inference log
// showing tokens but the spend ledger reporting request_count=0 on
// every first-inference-after-launch. Bumped to 5min — this test is
// the sentinel that catches a future tightening below realistic
// cold-load latency.
func TestCore_SettleWithinTTL_RecordsFullLedger(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})

	quotas := quota.QuotaConfig{
		SpendLimit:       1000,
		DefaultMaxTokens: 100,
	}
	ac := &AccessContext{
		Key: &KeyPrincipal{ID: "vk-cold", Role: RoleUser, IsVirtual: true, Quotas: quotas},
	}
	_, release, id := c.Enforce(ac, "model-cold")
	require.NotEqual(t, NoReservation, id, "expected stash id with SpendLimit set")
	release()

	// Simulate a long-but-within-TTL request: backdate the stash to
	// just under the TTL window. Before the fix the TTL was 30s, so
	// any request taking longer would land here with the stash gone;
	// after the fix the TTL is 5min, comfortably covering this.
	c.pendingMu.Lock()
	keyed := c.pendingReservations[ac.Key.ID]
	entry := keyed[id]
	entry.createdAt = time.Now().Add(-pendingReservationTTL + 30*time.Second)
	keyed[id] = entry
	c.pendingMu.Unlock()

	// Reaper sweep — must NOT reap because we're still within TTL.
	reaped := c.reapStalePendingReservations(time.Now())
	assert.Equal(t, 0, reaped, "stash within TTL must survive reaper sweep")

	// Bridge fires with real tokens — settle path must update ledger.
	c.RecordSpendByKey(ac.Key.ID, id, 0, 35, 2)

	state := c.GetKeySpend(ac.Key.ID)
	require.NotNil(t, state, "spend ledger row must exist after settle")
	assert.EqualValues(t, 1, state.RequestCount, "request_count must increment by 1")
	assert.EqualValues(t, 35, state.TokensIn, "tokens_in must record actual prompt tokens")
	assert.EqualValues(t, 2, state.TokensOut, "tokens_out must record actual completion tokens")

	// Stash row must be drained after successful settle.
	c.pendingMu.Lock()
	_, present := c.pendingReservations[ac.Key.ID]
	c.pendingMu.Unlock()
	assert.False(t, present, "settled stash row should be removed")
}

// Authorize is the unmetered half of Enforce: it must spend none of the
// key's RPM, or a client counting tokens before each call halves its
// own request budget.
func TestCore_Authorize_DoesNotTickRPM(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})
	ac := &AccessContext{
		Key: &KeyPrincipal{ID: "vk-rpm", Role: RoleUser, IsVirtual: true,
			Quotas: quota.QuotaConfig{RPMLimit: 1}},
	}

	for range 3 {
		require.True(t, c.Authorize(ac, "any-model").Allowed)
	}
	d, release, _ := c.Enforce(ac, "any-model")
	require.True(t, d.Allowed, "Authorize calls must leave the single RPM slot unspent")
	release()

	d, _, _ = c.Enforce(ac, "any-model")
	assert.Equal(t, DenyRPM, d.Reason, "the limit itself still holds")
}

func TestCore_Authorize_DeniesWhatEnforceDenies(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCore(t, StaticKeySet{})
	past := time.Now().Add(-time.Hour)
	cases := map[DenialReason]*AccessContext{
		DenyKeySuspended: {Key: &KeyPrincipal{ID: "vk", IsVirtual: true, Suspended: true}},
		DenyTeamSuspended: {Key: &KeyPrincipal{ID: "vk", IsVirtual: true},
			Team: &TeamPrincipal{ID: "t", Suspended: true}},
		DenyKeyExpired: {Key: &KeyPrincipal{ID: "vk", IsVirtual: true, ExpiresAt: &past}},
		DenyModelAccess: {Key: &KeyPrincipal{ID: "vk", IsVirtual: true},
			Team: &TeamPrincipal{ID: "t", AllowedModels: []string{"other"}}},
	}
	for want, ac := range cases {
		assert.Equal(t, want, c.Authorize(ac, "m").Reason, want.String())
		d, _, _ := c.Enforce(ac, "m")
		assert.Equal(t, want, d.Reason, want.String())
	}
}
