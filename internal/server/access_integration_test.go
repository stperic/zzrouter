// package server — end-to-end integration tests for the access chain.
//
// These tests exercise a real AccessControl wired against real pkg/keys,
// pkg/teams, and pkg/quota implementations in temp-dir-backed stores.
// They cover the full request lifecycle that model_dispatch.go drives:
//
//     Authenticate (validate key + resolve team via key.TeamID)
//     Enforce       (suspended → expired → model access → quota → concurrency)
//     RecordSpendByKey (settle reservations via the inference log bridge)

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// reservationIDFromCtx extracts the control.ReservationID stashed on
// gin.Context by AccessControl.Enforce. Zero when no reservation was
// taken (e.g. no SpendLimit configured); safe to pass straight into
// RecordSpendByKey in that case.
func reservationIDFromCtx(c *gin.Context) uint64 {
	if v, ok := c.Get(string(CtxKeyReservationID)); ok {
		if id, ok := v.(control.ReservationID); ok {
			return uint64(id)
		}
	}
	return 0
}

// ============================================================================
// Harness
// ============================================================================

type accessRig struct {
	access   *AccessControl
	enforcer *quota.Enforcer
	keys     *keys.FileKeyStore
	teams    *teams.FileTeamStore
	groups   *stubGroupReader
}

// seedKey provisions a virtual key bound to the given team and returns its
// raw (plaintext) value. Callers must have already seedTeam'd the team.
func (r *accessRig) seedKey(t *testing.T, id, teamID, teamRole string, vk *keys.VirtualKey) string {
	t.Helper()
	vk.TeamID = teamID
	vk.TeamRole = teamRole
	raw, err := r.keys.Create(id, vk)
	require.NoError(t, err, "seed key %q", id)
	return raw
}

// seedTeam creates a shared team in the store.
func (r *accessRig) seedTeam(t *testing.T, id string, team *teams.Team) {
	t.Helper()
	if team.Kind == "" {
		team.Kind = teams.KindShared
	}
	require.NoError(t, r.teams.Create(id, team))
}

// makeRequest builds a gin.Context + ResponseRecorder for a synthetic HTTP
// request and attaches the given raw API key to X-API-Key.
func (r *accessRig) makeRequest(rawKey string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if rawKey != "" {
		req.Header.Set("X-API-Key", rawKey)
	}
	c.Request = req
	return c, w
}

type stubGroupReader struct {
	groups map[string]*modelgroup.ModelGroup
}

func newStubGroupReader() *stubGroupReader {
	return &stubGroupReader{groups: make(map[string]*modelgroup.ModelGroup)}
}

func (s *stubGroupReader) Get(name string) *modelgroup.ModelGroup {
	return s.groups[name]
}

func (s *stubGroupReader) Names() []string {
	names := make([]string, 0, len(s.groups))
	for n := range s.groups {
		names = append(names, n)
	}
	return names
}

// set seeds a group by name with the given replica model names.
func (s *stubGroupReader) set(name string, replicaModels ...string) {
	reps := make([]modelgroup.Replica, 0, len(replicaModels))
	for _, m := range replicaModels {
		reps = append(reps, modelgroup.Replica{Model: m})
	}
	s.groups[name] = &modelgroup.ModelGroup{Replicas: reps}
}

func newAccessRig(t *testing.T) *accessRig {
	t.Helper()
	return newAccessRigWithHooks(t, ModelHooks{})
}

func newAccessRigWithHooks(t *testing.T, hooks ModelHooks) *accessRig {
	t.Helper()

	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	teamStore := teams.NewFileTeamStore(filepath.Join(dir, "teams.yaml"))
	groups := newStubGroupReader()

	enforcer := quota.NewEnforcer(
		quota.NewRateLimiter(),
		quota.NewConcurrencyLimiter(),
		quota.NewSpendTracker(""),
	)

	gw := NewAccessControl(StaticKeySet{}, keyStore, teamStore, groups, enforcer, nil, hooks)
	t.Cleanup(func() { gw.Stop(context.Background()) })

	return &accessRig{
		access:   gw,
		enforcer: enforcer,
		keys:     keyStore,
		teams:    teamStore,
		groups:   groups,
	}
}

// ============================================================================
// A. Full auth + enforce happy path with team membership
// ============================================================================

func TestAccessIntegration_FullAuthAndEnforce_HappyPath(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name: "Alice",
		Role: "user",
	})

	c, w := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	require.Equal(t, http.StatusOK, w.Code)

	ac := GetAccessContext(c)
	require.NotNil(t, ac)
	require.NotNil(t, ac.Key)
	assert.Equal(t, "alice", ac.Key.ID)
	assert.True(t, ac.Key.IsVirtual)
	require.NotNil(t, ac.Team, "team principal should be resolved from key.TeamID")
	assert.Equal(t, "eng", ac.Team.ID)
	assert.Equal(t, teams.RoleOwner, ac.Team.MemberRole)

	require.True(t, rig.access.Enforce(c, "llama3"))

	val, exists := c.Get(string(CtxKeyConcurrencyRelease))
	require.True(t, exists)
	if release, ok := val.(func()); ok {
		release()
	}
}

// ============================================================================
// B. Quota denial at key scope
// ============================================================================

func TestAccessIntegration_KeyRPMDenial(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name: "Alice",
		Role: "user",
		QuotaConfig: quota.QuotaConfig{
			RPMLimit: 1,
		},
	})

	c1, w1 := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c1, RoleUser))
	require.True(t, rig.access.Enforce(c1, "llama3"))
	assert.Equal(t, http.StatusOK, w1.Code, "first request should pass through")

	c2, w2 := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c2, RoleUser))
	require.False(t, rig.access.Enforce(c2, "llama3"))
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
	assert.Contains(t, w2.Body.String(), "RPM limit exceeded")

	// Retry-After is now derived from the rate-limit reset window
	// (formerly hardcoded to "1"). Must be a positive integer.
	retryAfter, err := strconv.Atoi(w2.Header().Get("Retry-After"))
	require.NoError(t, err, "Retry-After must be an integer; got %q", w2.Header().Get("Retry-After"))
	assert.GreaterOrEqual(t, retryAfter, 1)

	// Zzrouter sibling block — parity with the 402 budget envelope.
	assert.Contains(t, w2.Body.String(), `"limit_kind":"rpm"`)
	assert.Contains(t, w2.Body.String(), `"scope":"key"`)
	assert.Contains(t, w2.Body.String(), `"entity_id":"alice"`)
	assert.Contains(t, w2.Body.String(), `"retry_after_seconds":`)
	assert.Contains(t, w2.Body.String(), `"next_reset_at":`)
}

// ============================================================================
// C. Quota denial at team scope blocks every member
// ============================================================================

func TestAccessIntegration_TeamBudgetExhaustionBlocksMembers(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{
		Name: "Engineering",
		QuotaConfig: quota.QuotaConfig{
			SpendLimit:       0.01,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 100,
		},
	})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleMember), &keys.VirtualKey{Name: "Alice", Role: "user"})
	bobRaw := rig.seedKey(t, "bob", "eng", string(teams.RoleMember), &keys.VirtualKey{Name: "Bob", Role: "user"})

	cA, _ := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(cA, RoleUser))
	require.True(t, rig.access.Enforce(cA, "llama3"))

	rig.access.RecordSpendByKey("alice", reservationIDFromCtx(cA), 10_000, 50, 100)

	cA2, wA2 := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(cA2, RoleUser))
	assert.False(t, rig.access.Enforce(cA2, "llama3"))
	assert.Equal(t, http.StatusPaymentRequired, wA2.Code)
	assert.Contains(t, wA2.Body.String(), "Budget exhausted")

	cB, wB := rig.makeRequest(bobRaw)
	require.True(t, rig.access.Authenticate(cB, RoleUser))
	assert.False(t, rig.access.Enforce(cB, "llama3"))
	assert.Equal(t, http.StatusPaymentRequired, wB.Code)
	assert.Contains(t, wB.Body.String(), "Budget exhausted")
}

// ============================================================================
// D. Team-level model access with group expansion
// ============================================================================

func TestAccessIntegration_TeamModelAccess(t *testing.T) {
	rig := newAccessRig(t)

	rig.groups.set("fast-chat", "llama3", "qwen")

	rig.seedTeam(t, "eng", &teams.Team{
		Name:          "Engineering",
		AllowedModels: []string{"fast-chat"},
	})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{Name: "Alice", Role: "user"})

	cases := []struct {
		model     string
		wantAllow bool
	}{
		{"llama3", true},
		{"qwen", true},
		{"mistral", false},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			c, w := rig.makeRequest(aliceRaw)
			require.True(t, rig.access.Authenticate(c, RoleUser))

			allowed := rig.access.Enforce(c, tc.model)
			assert.Equal(t, tc.wantAllow, allowed)
			if !tc.wantAllow {
				assert.Equal(t, http.StatusForbidden, w.Code)
				assert.Contains(t, w.Body.String(), "Model access denied")
			}
		})
	}
}

// ============================================================================
// E. Suspended virtual key rejected at auth layer
// ============================================================================

func TestAccessIntegration_SuspendedKeyRejectedAtAuth(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name:      "Alice",
		Role:      "user",
		Suspended: true,
	})

	c, w := rig.makeRequest(aliceRaw)
	assert.False(t, rig.access.Authenticate(c, RoleUser))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// F. Suspended team rejected at enforce layer
// ============================================================================

func TestAccessIntegration_SuspendedTeamRejectedAtEnforce(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{
		Name:      "Engineering",
		Suspended: true,
	})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{Name: "Alice", Role: "user"})

	c, w := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	require.False(t, rig.access.Enforce(c, "llama3"))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), `suspended`)
}

// ============================================================================
// G. Concurrent reservation race
// ============================================================================

func TestAccessIntegration_ConcurrentRequestsRespectRPMLimit(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name: "Alice",
		Role: "user",
		QuotaConfig: quota.QuotaConfig{
			RPMLimit: 3,
		},
	})

	const workers = 10
	var allowed atomic.Int32
	var denied atomic.Int32

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, _ := rig.makeRequest(aliceRaw)
			if !rig.access.Authenticate(c, RoleUser) {
				denied.Add(1)
				return
			}
			if rig.access.Enforce(c, "llama3") {
				allowed.Add(1)
			} else {
				denied.Add(1)
			}
		}()
	}

	close(start)
	wg.Wait()

	total := allowed.Load() + denied.Load()
	assert.EqualValues(t, workers, total)
	assert.LessOrEqual(t, allowed.Load(), int32(3))
	assert.GreaterOrEqual(t, allowed.Load(), int32(1))
	assert.GreaterOrEqual(t, denied.Load(), int32(workers-3))
}

// ============================================================================
// H. Post-response settlement round-trip: reserve → enforce → settle → verify
// ============================================================================

func TestAccessIntegration_SettlementRoundTrip_KeyAndTeam(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{
		Name: "Engineering",
		QuotaConfig: quota.QuotaConfig{
			SpendLimit:       50.00,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 200,
		},
	})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name: "Alice",
		Role: "user",
		QuotaConfig: quota.QuotaConfig{
			SpendLimit:       5.00,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 200,
		},
	})

	c, _ := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	require.True(t, rig.access.Enforce(c, "llama3"))

	const actualCostMicro = 1_750_000
	rig.access.RecordSpendByKey("alice", reservationIDFromCtx(c), actualCostMicro, 400, 800)

	keySpend := rig.access.GetKeySpend("alice")
	require.NotNil(t, keySpend)
	assert.InDelta(t, 1.75, keySpend.SpendUSD(), 1e-9)
	assert.EqualValues(t, 0, keySpend.ReservedMicro)
	assert.EqualValues(t, 400, keySpend.TokensIn)
	assert.EqualValues(t, 800, keySpend.TokensOut)
	assert.EqualValues(t, 1, keySpend.RequestCount)

	teamSpend := rig.access.GetTeamSpend("eng")
	require.NotNil(t, teamSpend)
	assert.InDelta(t, 1.75, teamSpend.SpendUSD(), 1e-9)
	assert.EqualValues(t, 0, teamSpend.ReservedMicro)
	assert.EqualValues(t, 1, teamSpend.RequestCount)
}

// ============================================================================
// I. Missing / invalid credentials
// ============================================================================

func TestAccessIntegration_MissingAPIKey(t *testing.T) {
	rig := newAccessRig(t)

	c, w := rig.makeRequest("")
	assert.False(t, rig.access.Authenticate(c, RoleUser))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "API key required")
}

func TestAccessIntegration_InvalidAPIKey(t *testing.T) {
	rig := newAccessRig(t)

	c, w := rig.makeRequest("zzr_not-a-real-key")
	assert.False(t, rig.access.Authenticate(c, RoleUser))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid")
}

// ============================================================================
// J. Concurrency denial rolls back budget reservation
// ============================================================================

func TestAccessIntegration_ConcurrencyDenialRollsBackBudgetReservation(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name: "Alice",
		Role: "user",
		QuotaConfig: quota.QuotaConfig{
			SpendLimit:          5.00,
			ResetPeriod:         "monthly",
			DefaultMaxTokens:    200,
			MaxParallelRequests: 1,
		},
	})

	c1, _ := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c1, RoleUser))
	require.True(t, rig.access.Enforce(c1, "llama3"))

	reservedAfterFirst := rig.access.GetKeySpend("alice").ReservedMicro
	require.Greater(t, reservedAfterFirst, int64(0))

	c2, w2 := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c2, RoleUser))
	require.False(t, rig.access.Enforce(c2, "llama3"))
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
	assert.Contains(t, w2.Body.String(), "Too many concurrent requests")

	reservedAfterSecond := rig.access.GetKeySpend("alice").ReservedMicro
	assert.Equal(t, reservedAfterFirst, reservedAfterSecond,
		"concurrency denial must cancel the budget reservation it raced with")

	if val, ok := c1.Get(string(CtxKeyConcurrencyRelease)); ok {
		if release, ok := val.(func()); ok {
			release()
		}
	}

	c3, _ := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c3, RoleUser))
	assert.True(t, rig.access.Enforce(c3, "llama3"))
}

// ============================================================================
// K. Handler-error path releases pending reservation immediately
// ============================================================================

func TestAccessIntegration_CancelPendingReservationIfUnsettled_ReleasesBudget(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name: "Alice",
		Role: "user",
		QuotaConfig: quota.QuotaConfig{
			SpendLimit:       5.00,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 200,
		},
	})

	c, _ := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	require.True(t, rig.access.Enforce(c, "llama3"))

	reservedAfterEnforce := rig.access.GetKeySpend("alice").ReservedMicro
	require.Greater(t, reservedAfterEnforce, int64(0))

	rig.access.CancelPendingReservationIfUnsettled(c)

	reservedAfterCancel := rig.access.GetKeySpend("alice").ReservedMicro
	assert.EqualValues(t, 0, reservedAfterCancel)
}

func TestAccessIntegration_CancelPendingReservationIfUnsettled_NoopAfterBridgeSettled(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name: "Alice",
		Role: "user",
		QuotaConfig: quota.QuotaConfig{
			SpendLimit:       5.00,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 200,
		},
	})

	c, _ := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	require.True(t, rig.access.Enforce(c, "llama3"))

	const actualCostMicro = 500_000
	rig.access.RecordSpendByKey("alice", reservationIDFromCtx(c), actualCostMicro, 100, 200)

	spendAfterSettle := rig.access.GetKeySpend("alice")
	require.NotNil(t, spendAfterSettle)
	assert.EqualValues(t, 0, spendAfterSettle.ReservedMicro)
	assert.InDelta(t, 0.50, spendAfterSettle.SpendUSD(), 1e-9)

	rig.access.CancelPendingReservationIfUnsettled(c)

	spendAfterCancel := rig.access.GetKeySpend("alice")
	assert.EqualValues(t, 0, spendAfterCancel.ReservedMicro)
	assert.InDelta(t, 0.50, spendAfterCancel.SpendUSD(), 1e-9)
	assert.EqualValues(t, 1, spendAfterCancel.RequestCount)
}

// ============================================================================
// L. Role hierarchy — user key cannot reach admin endpoints
// ============================================================================

func TestAccessIntegration_RoleHierarchy_UserCannotReachAdminEndpoint(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{Name: "Alice", Role: "user"})

	c, w := rig.makeRequest(aliceRaw)
	assert.False(t, rig.access.Authenticate(c, RoleAdmin))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "Insufficient permissions")
}

// ============================================================================
// M. Bridge releases the reservation when an error path fires
//    RecordCompletion(0, 0)
// ============================================================================

// TestAccessIntegration_BridgeReleasesReservationOnErrorPath pins the
// invariant the D2.4 error-path emission relies on: when the recorder
// fires RecordCompletion(0, 0) on a tagged failure, the
// InferenceLogBridge settles the stashed budget reservation with cost=0
// — releasing the held estimate without crediting spend. Production wires
// this pattern into ~6 failure paths (upstream 4xx/5xx, transport fail,
// mid-stream abort, fallback exhausted, pre-backend resolve fail, etc.).
//
// Without this assertion, a refactor that drops the ReservationID
// propagation, skips the bridge call on zero cost, or fails to pop the
// stash would silently leak reservation slots until the
// pendingReservationTTL reaper runs — a slow leak that's hard to catch
// in production.
func TestAccessIntegration_BridgeReleasesReservationOnErrorPath(t *testing.T) {
	rig := newAccessRig(t)

	// Wire a real InferenceLogBridge against the rig's AccessControl —
	// mirrors what server_factory.go does at startup.
	bridge := NewInferenceLogBridge(inferencelog.NewStore(0, 0), "test-node", false, nil)
	bridge.SetAccessControl(rig.access)
	llm.SetInferenceLogHook(bridge)
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name: "Alice",
		Role: "user",
		QuotaConfig: quota.QuotaConfig{
			SpendLimit:       5.00,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 200,
		},
	})

	c, _ := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	require.True(t, rig.access.Enforce(c, "llama3"))

	// Guard: assertions below prove nothing if no reservation exists.
	reservedAfterEnforce := rig.access.GetKeySpend("alice").ReservedMicro
	require.Greater(t, reservedAfterEnforce, int64(0),
		"Enforce must reserve budget when key has SpendLimit configured")

	// One case (model_resolve_failed) covers all 11 D2.3/D2.4 error_types —
	// they share the SetError + RecordCompletion(0, 0) shape; bridge ignores ErrorType.
	rec := llm.NewInferenceRecorder(c.Request.Context(), "llama3", "ollama")
	// Required: bridge gates settle on data.KeyID != "" (inference_log_bridge.go).
	rec.SetKeyID("alice")
	rig.access.MirrorReservationIDToRecorder(c, rec)
	rec.SetError("model_resolve_failed", "boom")
	rec.RecordCompletion(0, 0)

	spend := rig.access.GetKeySpend("alice")
	require.NotNil(t, spend)
	assert.EqualValues(t, 0, spend.ReservedMicro,
		"bridge must release the stashed reservation on RecordCompletion(0, 0)")
	assert.EqualValues(t, 0, spend.SpendMicro,
		"error path must not credit spend")
	// Zero-cost settle still increments RequestCount — the request happened, just at zero cost.
	assert.EqualValues(t, 1, spend.RequestCount,
		"zero-cost settle still counts as a recorded request")

	// The deferred CancelPendingReservationIfUnsettled (model_dispatch.go)
	// must be a no-op when the bridge has already settled — confirming
	// the stash was actually popped, not just sat on by both paths.
	rig.access.CancelPendingReservationIfUnsettled(c)
	assert.EqualValues(t, 0, rig.access.GetKeySpend("alice").ReservedMicro,
		"deferred cancel must remain a no-op once bridge has settled the stash")
}

// The catalog decorates ids with @node and the dispatcher strips that off
// before Enforce runs, so an allow list written from /v1/models must still
// match. Drives the real Server.modelIdentity, which is the function
// server_factory wires into ModelHooks.
func TestAccessIntegration_TeamModelAccess_CatalogIDs(t *testing.T) {
	rig := newAccessRigWithHooks(t, ModelHooks{Identity: (&Server{}).modelIdentity})

	rig.seedTeam(t, "eng", &teams.Team{
		Name:          "Engineering",
		AllowedModels: []string{"qwen2.5:0.5b@macbook-pro"},
	})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{Name: "Alice", Role: "user"})

	cases := []struct {
		model     string
		wantAllow bool
	}{
		{"qwen2.5:0.5b", true},          // what the dispatcher asks about
		{"qwen2.5:0.5b@worker-1", true}, // the same model on another node
		{"smollm:135m@macbook-pro", false},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			c, w := rig.makeRequest(aliceRaw)
			require.True(t, rig.access.Authenticate(c, RoleUser))

			assert.Equal(t, tc.wantAllow, rig.access.Enforce(c, tc.model))
			if !tc.wantAllow {
				assert.Equal(t, http.StatusForbidden, w.Code)
			}
		})
	}
}

// Headers were snapshotted before the quota check consumed the request,
// so the first call on a fresh key reported the full limit as remaining
// and a client self-throttling on the header overshot by one per window.
func TestAccessIntegration_RateLimitRemainingCountsThisRequest(t *testing.T) {
	rig := newAccessRig(t)

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	raw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name:        "Alice",
		Role:        "user",
		QuotaConfig: quota.QuotaConfig{RPMLimit: 5},
	})

	for i, want := range []string{"4", "3", "2"} {
		c, w := rig.makeRequest(raw)
		require.True(t, rig.access.Authenticate(c, RoleUser))
		require.True(t, rig.access.Enforce(c, "llama3"), "request %d denied under a limit of 5", i+1)

		assert.Equal(t, "5", w.Header().Get("X-Ratelimit-Limit-Requests"))
		assert.Equalf(t, want, w.Header().Get("X-Ratelimit-Remaining-Requests"),
			"after request %d", i+1)
	}
}
