package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

// newKeysTestHarness wires a complete KeysService + TeamsService + AccessControl
// against temp-dir-backed stores. Every test gets isolated state.
func newKeysTestHarness(t *testing.T) (*KeysService, *TeamsService, access.Store, *AccessControl) {
	t.Helper()
	teamsSvc, store, accessControl := newTeamsTestHarness(t)
	return NewKeysService(store, teamsSvc, accessControl), teamsSvc, store, accessControl
}

// ============================================================================
// Suspend / unsuspend via PATCH
// ============================================================================

func TestKeysService_UpdateKey_Suspend(t *testing.T) {
	svc, _, _, _ := newKeysTestHarness(t)

	_, err := svc.CreateKey(t.Context(), "alice", &CreateKeyRequest{Name: "Alice"}, "admin")
	require.NoError(t, err)

	suspend := true
	resp, err := svc.UpdateKey(t.Context(), "alice", &UpdateKeyRequest{Suspended: &suspend}, "admin-who-did-it")
	require.NoError(t, err)
	assert.True(t, resp.Suspended)
	assert.NotNil(t, resp.SuspendedAt)
	assert.Equal(t, "admin-who-did-it", resp.SuspendedBy)

	// Unsuspend clears the audit fields.
	unsuspend := false
	resp, err = svc.UpdateKey(t.Context(), "alice", &UpdateKeyRequest{Suspended: &unsuspend}, "other-admin")
	require.NoError(t, err)
	assert.False(t, resp.Suspended)
	assert.Nil(t, resp.SuspendedAt)
	assert.Empty(t, resp.SuspendedBy)
}

// ============================================================================
// Personal-team cascade on DeleteKey (architect-flagged test gap)
// ============================================================================

func TestKeysService_DeleteKey_CascadesPersonalTeam(t *testing.T) {
	svc, teamsSvc, _, _ := newKeysTestHarness(t)

	// Create a key with no team_id → personal team auto-created.
	resp, err := svc.CreateKey(t.Context(), "alice", &CreateKeyRequest{Name: "Alice"}, "admin")
	require.NoError(t, err)
	require.Equal(t, "personal-alice", resp.TeamID)
	require.Equal(t, "owner", resp.TeamRole)

	// The personal team exists.
	_, err = teamsSvc.GetTeam("personal-alice")
	require.NoError(t, err, "personal team should exist after key create")

	// Delete the key.
	require.NoError(t, svc.DeleteKey(t.Context(), "alice", "test"))

	// The personal team is gone — cascade fired.
	_, err = teamsSvc.GetTeam("personal-alice")
	require.Error(t, err)
	assert.True(t, errors.Is(err, teams.ErrTeamNotFound),
		"personal team should be cascade-deleted with its sole-owner key")
}

func TestKeysService_DeleteKey_SharedTeamNotCascaded(t *testing.T) {
	svc, teamsSvc, _, _ := newKeysTestHarness(t)

	// Create a shared team, then put a key in it.
	_, err := teamsSvc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering"}, "admin")
	require.NoError(t, err)

	_, err = svc.CreateKey(t.Context(), "alice", &CreateKeyRequest{
		Name:   "Alice",
		TeamID: "eng",
	}, "admin")
	require.NoError(t, err)

	// Delete the key.
	require.NoError(t, svc.DeleteKey(t.Context(), "alice", "test"))

	// The shared team survives — cascade must NOT touch shared teams.
	team, err := teamsSvc.GetTeam("eng")
	require.NoError(t, err, "shared team should never be cascade-deleted")
	assert.Equal(t, "eng", team.ID)
}

// ============================================================================
// CreateKey refusals write nothing
// ============================================================================

// A duplicate id is the caller's conflict, refused before a personal team
// is written, so the first key's team is untouched and nothing is orphaned.
func TestKeysService_CreateKey_DuplicateIDIsConflict(t *testing.T) {
	svc, teamsSvc, _, _ := newKeysTestHarness(t)

	_, err := svc.CreateKey(t.Context(), "alice", &CreateKeyRequest{Name: "Alice"}, "admin")
	require.NoError(t, err)

	_, err = svc.CreateKey(t.Context(), "alice", &CreateKeyRequest{Name: "Alice Again"}, "admin")
	require.Error(t, err)
	assert.True(t, isConflict(err), "a duplicate key id reported as: %v", err)

	team, err := teamsSvc.GetTeam("personal-alice")
	require.NoError(t, err)
	assert.Equal(t, "Personal: Alice", team.Name)
	assert.Equal(t, 0, teamsSvc.ReconcilePersonalTeams())
}

// An invalid expires_at is refused before the personal team is written, so
// a corrected retry with the same id succeeds.
func TestKeysService_CreateKey_InvalidExpiryLeavesNoTeam(t *testing.T) {
	svc, teamsSvc, _, _ := newKeysTestHarness(t)

	bad := "2027-01-01"
	_, err := svc.CreateKey(t.Context(), "ci", &CreateKeyRequest{Name: "CI", ExpiresAt: &bad}, "admin")
	require.Error(t, err)
	assert.True(t, isInvalidInput(err), "an invalid expiry reported as: %v", err)
	_, err = teamsSvc.GetTeam("personal-ci")
	assert.ErrorIs(t, err, teams.ErrTeamNotFound)

	good := "2027-01-01T00:00:00Z"
	resp, err := svc.CreateKey(t.Context(), "ci", &CreateKeyRequest{Name: "CI", ExpiresAt: &good}, "admin")
	require.NoError(t, err)
	assert.Equal(t, "personal-ci", resp.TeamID)
}

// ============================================================================
// Concurrent personal-team auto-create (architect-flagged test gap)
// ============================================================================

func TestKeysService_CreateKey_ConcurrentPersonalTeamCreate(t *testing.T) {
	svc, teamsSvc, _, _ := newKeysTestHarness(t)

	// Hammer: 20 goroutines each trying to create a distinct key with
	// auto-personal-team. The shared KeysService.mu + TeamsService.mu
	// discipline must serialize them so every call completes without
	// data races, every key ends up with its own personal team, and no
	// orphans linger.
	const workers = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := "alice-" + itoa(idx)
			_, err := svc.CreateKey(t.Context(), name, &CreateKeyRequest{Name: name}, "admin")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err)
	}

	// Every key has its own personal team.
	for i := 0; i < workers; i++ {
		name := "alice-" + itoa(i)
		_, err := teamsSvc.GetTeam("personal-" + name)
		assert.NoError(t, err, "missing personal team for %s", name)
	}

	// No orphans after the hammer.
	reaped := teamsSvc.ReconcilePersonalTeams()
	assert.Equal(t, 0, reaped)
}

// itoa avoids pulling strconv into the top of this file just for test names.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	buf := make([]byte, 0, 8)
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	return string(buf)
}

// ============================================================================
// Caller-caused rejections must classify as caller-caused
// ============================================================================

// Every one of these is a value the caller sent in the request body.
// Before, each returned a bare error, which the controller could only
// read as 500 -- and a 500 tells an agent that retrying the identical
// request might work, when nothing about it ever will. The file already
// used invalidInputf for its other rejections; these three had simply
// been missed, which is how a half-applied pattern fails: silently, and
// only on the paths nobody drove.
func TestKeysService_CreateKey_RejectsBadTeamInputAsCallerError(t *testing.T) {
	svc, teamsSvc, _, _ := newKeysTestHarness(t)

	_, err := teamsSvc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "shared", Name: "Shared"}, "admin")
	require.NoError(t, err)

	cases := []struct {
		name string
		req  *CreateKeyRequest
	}{
		{"unknown team", &CreateKeyRequest{Name: "k", TeamID: "no-such-team"}},
		{"invalid team_role", &CreateKeyRequest{Name: "k", TeamID: "shared", TeamRole: "not-a-role"}},
		// With no team_id the personal-team branch assigns owner and
		// never looked at team_role, so this answered 201 and dropped
		// the caller's value. Silently accepting a value you ignore is
		// the same lie as a 500 -- the response describes something
		// that did not happen.
		{"invalid team_role with no team", &CreateKeyRequest{Name: "k", TeamRole: "not-a-role"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateKey(t.Context(), "probe-"+tc.name, tc.req, "admin")
			require.Error(t, err)
			assert.True(t, isInvalidInput(err),
				"a rejected request body reported as a server fault: %v", err)
		})
	}
}

// ============================================================================
// Key insert failing after its personal team was written
// ============================================================================

// createKeyFault is a real store whose key creation fails: as a failed
// persist, or, with cancel set, as a client that leaves once its personal
// team is written.
type createKeyFault struct {
	access.Store
	cancel context.CancelFunc
}

func (f createKeyFault) CreateTeam(ctx context.Context, id string, team teams.Team, ev audit.Event) (*teams.Team, error) {
	created, err := f.Store.CreateTeam(ctx, id, team, ev)
	if f.cancel != nil {
		f.cancel()
	}
	return created, err
}

func (f createKeyFault) CreateKey(ctx context.Context, id string, key keys.VirtualKey, ev audit.Event) (*keys.VirtualKey, string, error) {
	if f.cancel == nil {
		return nil, "", errors.New("injected persist failure")
	}
	return f.Store.CreateKey(ctx, id, key, ev)
}

func TestKeysService_CreateKey_RollsBackPersonalTeamWhenKeyFails(t *testing.T) {
	for _, cause := range []string{"persist failure", "client disconnect"} {
		t.Run(cause, func(t *testing.T) {
			dir := t.TempDir()
			store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
			require.NoError(t, err)
			fault := createKeyFault{Store: store}
			ctx := t.Context()
			if cause == "client disconnect" {
				ctx, fault.cancel = context.WithCancel(ctx)
				defer fault.cancel()
			}
			teamsSvc := NewTeamsService(fault, newTestAccessControl(t, fault))
			svc := NewKeysService(fault, teamsSvc, teamsSvc.access)

			_, err = svc.CreateKey(ctx, "alice", &CreateKeyRequest{Name: "Alice"}, "admin")
			require.Error(t, err)
			assert.Nil(t, store.Snapshot().Key("alice"))
			assert.Nil(t, store.Snapshot().Team("personal-alice"), "the personal team is rolled back with the failed key")
			require.NoError(t, store.Close())
			reopened, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = reopened.Close() })
			assert.Nil(t, reopened.Snapshot().Team("personal-alice"), "no orphan is left on disk for the next start to sweep")
		})
	}
}

// ============================================================================
// Spend rows leave with their key or team
// ============================================================================

func TestKeyAndTeamDeletionPurgeSpend(t *testing.T) {
	store := newTestAccessStore(t)
	enforcer := quota.NewEnforcer(quota.NewRateLimiter(), quota.NewConcurrencyLimiter(), quota.NewSpendTracker(""))
	accessControl := NewAccessControl(StaticKeySet{}, store, nil, enforcer, nil, ModelHooks{})
	teamsSvc := NewTeamsService(store, accessControl)
	svc := NewKeysService(store, teamsSvc, accessControl)
	_, err := teamsSvc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering"}, "admin")
	require.NoError(t, err)
	_, err = svc.CreateKey(t.Context(), "shared-key", &CreateKeyRequest{Name: "Shared", TeamID: "eng"}, "admin")
	require.NoError(t, err)
	_, err = svc.CreateKey(t.Context(), "solo", &CreateKeyRequest{Name: "Solo"}, "admin")
	require.NoError(t, err)
	for _, scope := range []quota.QuotaScope{
		{Scope: quota.ScopeKey, EntityID: "shared-key"}, {Scope: quota.ScopeTeam, EntityID: "eng"},
		{Scope: quota.ScopeKey, EntityID: "solo"}, {Scope: quota.ScopeTeam, EntityID: "personal-solo"},
	} {
		scope.Quotas = quota.QuotaConfig{SpendLimit: 5, ResetPeriod: "monthly", DefaultMaxTokens: 100}
		_, reservations := enforcer.Check([]quota.QuotaScope{scope}, paidQuotes())
		enforcer.SettleReservations(reservations, 250_000, 10, 10)
	}
	require.NotNil(t, accessControl.GetKeySpend("solo"))
	require.NotNil(t, accessControl.GetTeamSpend("personal-solo"))

	require.NoError(t, svc.DeleteKey(t.Context(), "solo", "admin"))
	assert.Nil(t, accessControl.GetKeySpend("solo"))
	assert.Nil(t, accessControl.GetTeamSpend("personal-solo"), "the cascaded personal team's spend goes too")
	require.NoError(t, svc.DeleteKey(t.Context(), "shared-key", "admin"))
	assert.Nil(t, accessControl.GetKeySpend("shared-key"))
	require.NotNil(t, accessControl.GetTeamSpend("eng"), "a shared team keeps its spend after a member leaves")
	require.NoError(t, teamsSvc.DeleteTeam(t.Context(), "eng", "admin"))
	assert.Nil(t, accessControl.GetTeamSpend("eng"))
}

// A PATCH that would leave a key the node cannot load again is the caller's
// error, and nothing is written.
func TestUpdateKey_EmptyNameIsRefusedAs400(t *testing.T) {
	svc, _, store, _ := newKeysTestHarness(t)
	_, err := svc.CreateKey(t.Context(), "alice", &CreateKeyRequest{Name: "Alice"}, "admin")
	require.NoError(t, err)

	ctrl := &KeysController{service: svc}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/zzrouter/v1/keys/alice", strings.NewReader(`{"name":""}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "alice"}}
	ctrl.UpdateKey(c)

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, "Alice", store.Snapshot().Key("alice").Name)
}
