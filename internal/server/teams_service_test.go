package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/filestore"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
)

func newTeamsTestHarness(t *testing.T) (*TeamsService, access.Store, *AccessControl) {
	t.Helper()
	store := newTestAccessStore(t)
	accessControl := newTestAccessControl(t, store)
	return NewTeamsService(store, accessControl), store, accessControl
}

// newTestAccessControl wires AccessControl to store with an in-memory enforcer.
func newTestAccessControl(t *testing.T, store access.Store) *AccessControl {
	t.Helper()
	enforcer := quota.NewEnforcer(
		quota.NewRateLimiter(),
		quota.NewConcurrencyLimiter(),
		quota.NewSpendTracker(""),
	)
	accessControl := NewAccessControl(StaticKeySet{}, store, nil, enforcer, nil, ModelHooks{})
	t.Cleanup(func() { accessControl.Stop(context.Background()) })
	return accessControl
}

// seedKeyBound creates a virtual key with the given TeamID/TeamRole.
func seedKeyBound(t *testing.T, store access.Store, id, teamID, role string) {
	t.Helper()
	seedTestKey(t, store, id, keys.VirtualKey{
		Name:     id,
		Role:     "user",
		TeamID:   teamID,
		TeamRole: role,
	})
}

// ============================================================================
// CreateTeam — shared only
// ============================================================================

func TestTeamsService_CreateTeam_Shared(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	resp, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{
		ID:            "engineering",
		Name:          "Engineering",
		AllowedModels: []string{"fast-chat"},
		RPMLimit:      100,
	}, "admin-caller")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "engineering", resp.ID)
	assert.Equal(t, string(teams.KindShared), resp.Kind)
	assert.Equal(t, 100, resp.RPMLimit)
	assert.Equal(t, "admin-caller", resp.CreatedBy)
}

func TestTeamsService_CreateTeam_RejectsPersonalPrefix(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{
		ID:   "personal-squatter",
		Name: "Squatter",
	}, "caller")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
}

func TestTeamsService_CreateTeam_RejectsDuplicate(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)

	_, err = svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering 2"}, "caller")
	require.Error(t, err)
	assert.True(t, errors.Is(err, teams.ErrTeamExists))
}

// ============================================================================
// UpdateTeam
// ============================================================================

func TestTeamsService_UpdateTeam_PatchFields(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{
		ID: "eng", Name: "Engineering", RPMLimit: 100,
	}, "caller")
	require.NoError(t, err)

	newName := "Engineering (renamed)"
	newRPM := 200
	resp, err := svc.UpdateTeam(t.Context(), "eng", &UpdateTeamRequest{
		Name:     &newName,
		RPMLimit: &newRPM,
	}, "caller2")
	require.NoError(t, err)
	assert.Equal(t, "Engineering (renamed)", resp.Name)
	assert.Equal(t, 200, resp.RPMLimit)
	assert.Equal(t, "caller2", resp.UpdatedBy)
}

func TestTeamsService_UpdateTeam_RejectsPersonal(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreatePersonalTeam(t.Context(), "personal-alice", "alice", "caller")
	require.NoError(t, err)

	newName := "hacked"
	_, err = svc.UpdateTeam(t.Context(), "personal-alice", &UpdateTeamRequest{Name: &newName}, "caller")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPersonalTeamImmutable))
}

// ============================================================================
// DeleteTeam
// ============================================================================

func TestTeamsService_DeleteTeam_EmptyOK(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)

	require.NoError(t, svc.DeleteTeam(t.Context(), "eng", "test"))
	_, err = svc.GetTeam("eng")
	require.Error(t, err)
}

func TestTeamsService_DeleteTeam_WithMembersRejected(t *testing.T) {
	svc, store, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)
	seedKeyBound(t, store, "alice", "eng", string(teams.RoleMember))

	err = svc.DeleteTeam(t.Context(), "eng", "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "member keys")
}

func TestTeamsService_DeleteTeam_RejectsPersonal(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreatePersonalTeam(t.Context(), "personal-alice", "alice", "caller")
	require.NoError(t, err)

	err = svc.DeleteTeam(t.Context(), "personal-alice", "test")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPersonalTeamImmutable))
}

func TestTeamsService_DeleteTeam_NotFound(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	err := svc.DeleteTeam(t.Context(), "missing", "test")
	require.Error(t, err)
	assert.True(t, errors.Is(err, teams.ErrTeamNotFound))
}

// ============================================================================
// GetTeamKeys — backed by Snapshot.KeysByTeam
// ============================================================================

func TestTeamsService_GetTeamKeys_FiltersByTeamID(t *testing.T) {
	svc, store, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)

	seedKeyBound(t, store, "alice", "eng", string(teams.RoleOwner))
	seedKeyBound(t, store, "bob", "eng", string(teams.RoleMember))
	seedKeyBound(t, store, "carol", "other-team", string(teams.RoleMember))

	resp, err := svc.GetTeamKeys("eng")
	require.NoError(t, err)
	assert.Equal(t, "eng", resp.TeamID)
	assert.Len(t, resp.Keys, 2)

	roles := make(map[string]string, 2)
	for _, k := range resp.Keys {
		roles[k.KeyID] = k.Role
	}
	assert.Equal(t, "owner", roles["alice"])
	assert.Equal(t, "member", roles["bob"])
}

// ============================================================================
// Personal-team lifecycle
// ============================================================================

func TestTeamsService_CreatePersonalTeam(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	resp, err := svc.CreatePersonalTeam(t.Context(), "personal-alice", "alice", "caller")
	require.NoError(t, err)
	assert.Equal(t, string(teams.KindPersonal), resp.Kind)
	assert.Equal(t, "Personal: alice", resp.Name)
}

func TestTeamsService_DeletePersonalTeam(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreatePersonalTeam(t.Context(), "personal-alice", "alice", "caller")
	require.NoError(t, err)

	require.NoError(t, svc.DeletePersonalTeam(t.Context(), "personal-alice", "test"))
	_, err = svc.GetTeam("personal-alice")
	require.Error(t, err)
}

func TestTeamsService_DeletePersonalTeam_RejectsShared(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)

	// DeletePersonalTeam returns not-found for shared teams (soft-no-op for
	// the KeysService cascade path).
	err = svc.DeletePersonalTeam(t.Context(), "eng", "test")
	require.Error(t, err)
	assert.True(t, errors.Is(err, teams.ErrTeamNotFound))
}

// ============================================================================
// Reconciliation sweep
// ============================================================================

func TestTeamsService_ReconcilePersonalTeams_ReapsOrphans(t *testing.T) {
	svc, store, _ := newTeamsTestHarness(t)

	// Orphan personal team — no referencing key.
	_, err := svc.CreatePersonalTeam(t.Context(), "personal-orphan", "orphan", "caller")
	require.NoError(t, err)

	// Personal team with its key — should be kept.
	_, err = svc.CreatePersonalTeam(t.Context(), "personal-alice", "alice", "caller")
	require.NoError(t, err)
	seedKeyBound(t, store, "alice", "personal-alice", string(teams.RoleOwner))

	// Shared team with no keys — should be kept (shared teams don't get reaped).
	_, err = svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)

	reaped := svc.ReconcilePersonalTeams()
	assert.Equal(t, 1, reaped)

	_, err = svc.GetTeam("personal-orphan")
	assert.Error(t, err, "orphan personal team should be reaped")

	_, err = svc.GetTeam("personal-alice")
	assert.NoError(t, err, "personal team with its key should survive")

	_, err = svc.GetTeam("eng")
	assert.NoError(t, err, "shared team should never be reaped")
}

// ============================================================================
// GetTeamUsage
// ============================================================================

func TestTeamsService_GetTeamUsage_FreshTeamReturnsZeros(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{
		ID: "eng", Name: "Engineering", RPMLimit: 100, SpendLimit: 50.0,
	}, "caller")
	require.NoError(t, err)

	resp, err := svc.GetTeamUsage("eng")
	require.NoError(t, err)
	assert.Equal(t, "eng", resp.TeamID)
	assert.EqualValues(t, 0, resp.TokensIn)
	assert.Equal(t, 100, resp.RPMLimit)
	assert.Equal(t, 50.0, resp.SpendLimit)
}

// The API specification documents a members merge-patch this service has
// never implemented. Lenient binding discarded it silently; strict
// binding would have failed the whole patch with a message that named no
// alternative, so a caller following the documented example lost a
// rename it never learned had failed.
func TestUpdateTeam_MembersIsRefusedWithSomewhereToGo(t *testing.T) {
	svc, _, _ := newTeamsTestHarness(t)
	_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Before"}, "admin")
	require.NoError(t, err)

	ctrl := &TeamsController{service: svc}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/zzrouter/v1/teams/eng",
		strings.NewReader(`{"name":"After","members":{"k":{"role":"member"}}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "eng"}}

	ctrl.UpdateTeam(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "membership is immutable after key creation")
	assert.Contains(t, w.Body.String(), "POST /zzrouter/v1/keys", "the error must name the supported replacement workflow")
	assert.Contains(t, w.Body.String(), "then revoke the old key")
	assert.NotContains(t, w.Body.String(), "PATCH /zzrouter/v1/keys")

	// The rename is still not applied — that is the caller's to retry —
	// but they now know exactly which field to drop.
	got, err := svc.GetTeam("eng")
	require.NoError(t, err)
	assert.Equal(t, "Before", got.Name)
}

// A team update leaves warmed key authentications alone: each dropped entry
// would cost an Argon2 verify on the next request.
func TestUpdateTeam_KeepsWarmedAuthCache(t *testing.T) {
	const envName = "ZZROUTER_TEST_TEAM_UPDATE_CACHE_KEY"
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keys.yaml"), []byte(`keys:
  ci:
    name: CI
    key_env: `+envName+`
    team_id: eng
    team_role: member
`), 0o600))
	store, err := filestore.Open(t.Context(), dir, filestore.Options{})
	require.NoError(t, err)
	accessControl := newTestAccessControl(t, store)
	svc := NewTeamsService(store, accessControl)
	_, err = svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering"}, "admin")
	require.NoError(t, err)
	authenticates := func(raw string) bool {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		c.Request.Header.Set("X-API-Key", raw)
		return accessControl.Authenticate(c, RoleUser)
	}
	t.Setenv(envName, "first-environment-secret")
	require.True(t, authenticates("first-environment-secret"))
	// Only the cached principal still accepts the old value now.
	t.Setenv(envName, "second-environment-secret")

	renamed := "Platform"
	_, err = svc.UpdateTeam(t.Context(), "eng", &UpdateTeamRequest{Name: &renamed}, "admin")
	require.NoError(t, err)
	assert.True(t, authenticates("first-environment-secret"), "the team update kept the warmed entry")
	accessControl.InvalidateAuthCache()
	assert.False(t, authenticates("first-environment-secret"), "the probe detects a dropped entry")
}
