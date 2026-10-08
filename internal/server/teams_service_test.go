package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
)

func newTeamsTestHarness(t *testing.T) (*TeamsService, *keys.FileKeyStore, *teams.FileTeamStore, *AccessControl) {
	t.Helper()

	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	teamStore := teams.NewFileTeamStore(filepath.Join(dir, "teams.yaml"))

	enforcer := quota.NewEnforcer(
		quota.NewRateLimiter(),
		quota.NewConcurrencyLimiter(),
		quota.NewSpendTracker(""),
	)

	access := NewAccessControl(StaticKeySet{}, keyStore, teamStore, nil, enforcer, nil, ModelHooks{})
	t.Cleanup(func() { access.Stop(context.Background()) })

	return NewTeamsService(teamStore, keyStore, access), keyStore, teamStore, access
}

// seedKeyBound creates a virtual key with the given TeamID/TeamRole.
func seedKeyBound(t *testing.T, store *keys.FileKeyStore, id, teamID, role string) {
	t.Helper()
	_, err := store.Create(id, &keys.VirtualKey{
		Name:     id,
		Role:     "user",
		TeamID:   teamID,
		TeamRole: role,
	})
	require.NoError(t, err, "seed key %q", id)
}

// ============================================================================
// CreateTeam — shared only
// ============================================================================

func TestTeamsService_CreateTeam_Shared(t *testing.T) {
	svc, _, _, _ := newTeamsTestHarness(t)

	resp, err := svc.CreateTeam(&CreateTeamRequest{
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
	svc, _, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(&CreateTeamRequest{
		ID:   "personal-squatter",
		Name: "Squatter",
	}, "caller")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
}

func TestTeamsService_CreateTeam_RejectsDuplicate(t *testing.T) {
	svc, _, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(&CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)

	_, err = svc.CreateTeam(&CreateTeamRequest{ID: "eng", Name: "Engineering 2"}, "caller")
	require.Error(t, err)
	assert.True(t, errors.Is(err, teams.ErrTeamExists))
}

// ============================================================================
// UpdateTeam
// ============================================================================

func TestTeamsService_UpdateTeam_PatchFields(t *testing.T) {
	svc, _, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(&CreateTeamRequest{
		ID: "eng", Name: "Engineering", RPMLimit: 100,
	}, "caller")
	require.NoError(t, err)

	newName := "Engineering (renamed)"
	newRPM := 200
	resp, err := svc.UpdateTeam("eng", &UpdateTeamRequest{
		Name:     &newName,
		RPMLimit: &newRPM,
	}, "caller2")
	require.NoError(t, err)
	assert.Equal(t, "Engineering (renamed)", resp.Name)
	assert.Equal(t, 200, resp.RPMLimit)
	assert.Equal(t, "caller2", resp.UpdatedBy)
}

func TestTeamsService_UpdateTeam_RejectsPersonal(t *testing.T) {
	svc, _, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreatePersonalTeam("personal-alice", "alice", "caller")
	require.NoError(t, err)

	newName := "hacked"
	_, err = svc.UpdateTeam("personal-alice", &UpdateTeamRequest{Name: &newName}, "caller")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPersonalTeamImmutable))
}

// ============================================================================
// DeleteTeam
// ============================================================================

func TestTeamsService_DeleteTeam_EmptyOK(t *testing.T) {
	svc, _, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(&CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)

	require.NoError(t, svc.DeleteTeam("eng", "test"))
	_, err = svc.GetTeam("eng")
	require.Error(t, err)
}

func TestTeamsService_DeleteTeam_WithMembersRejected(t *testing.T) {
	svc, keyStore, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(&CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)
	seedKeyBound(t, keyStore, "alice", "eng", string(teams.RoleMember))

	err = svc.DeleteTeam("eng", "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "member keys")
}

func TestTeamsService_DeleteTeam_RejectsPersonal(t *testing.T) {
	svc, _, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreatePersonalTeam("personal-alice", "alice", "caller")
	require.NoError(t, err)

	err = svc.DeleteTeam("personal-alice", "test")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPersonalTeamImmutable))
}

func TestTeamsService_DeleteTeam_NotFound(t *testing.T) {
	svc, _, _, _ := newTeamsTestHarness(t)

	err := svc.DeleteTeam("missing", "test")
	require.Error(t, err)
	assert.True(t, errors.Is(err, teams.ErrTeamNotFound))
}

// ============================================================================
// GetTeamKeys — backed by keyStore.ListByTeam
// ============================================================================

func TestTeamsService_GetTeamKeys_FiltersByTeamID(t *testing.T) {
	svc, keyStore, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(&CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)

	seedKeyBound(t, keyStore, "alice", "eng", string(teams.RoleOwner))
	seedKeyBound(t, keyStore, "bob", "eng", string(teams.RoleMember))
	seedKeyBound(t, keyStore, "carol", "other-team", string(teams.RoleMember))

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
	svc, _, _, _ := newTeamsTestHarness(t)

	resp, err := svc.CreatePersonalTeam("personal-alice", "alice", "caller")
	require.NoError(t, err)
	assert.Equal(t, string(teams.KindPersonal), resp.Kind)
	assert.Equal(t, "Personal: alice", resp.Name)
}

func TestTeamsService_DeletePersonalTeam(t *testing.T) {
	svc, _, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreatePersonalTeam("personal-alice", "alice", "caller")
	require.NoError(t, err)

	require.NoError(t, svc.DeletePersonalTeam("personal-alice", "test"))
	_, err = svc.GetTeam("personal-alice")
	require.Error(t, err)
}

func TestTeamsService_DeletePersonalTeam_RejectsShared(t *testing.T) {
	svc, _, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(&CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
	require.NoError(t, err)

	// DeletePersonalTeam returns not-found for shared teams (soft-no-op for
	// the KeysService cascade path).
	err = svc.DeletePersonalTeam("eng", "test")
	require.Error(t, err)
	assert.True(t, errors.Is(err, teams.ErrTeamNotFound))
}

// ============================================================================
// Reconciliation sweep
// ============================================================================

func TestTeamsService_ReconcilePersonalTeams_ReapsOrphans(t *testing.T) {
	svc, keyStore, _, _ := newTeamsTestHarness(t)

	// Orphan personal team — no referencing key.
	_, err := svc.CreatePersonalTeam("personal-orphan", "orphan", "caller")
	require.NoError(t, err)

	// Personal team with its key — should be kept.
	_, err = svc.CreatePersonalTeam("personal-alice", "alice", "caller")
	require.NoError(t, err)
	seedKeyBound(t, keyStore, "alice", "personal-alice", string(teams.RoleOwner))

	// Shared team with no keys — should be kept (shared teams don't get reaped).
	_, err = svc.CreateTeam(&CreateTeamRequest{ID: "eng", Name: "Engineering"}, "caller")
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
	svc, _, _, _ := newTeamsTestHarness(t)

	_, err := svc.CreateTeam(&CreateTeamRequest{
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
	svc, _, _, _ := newTeamsTestHarness(t)
	_, err := svc.CreateTeam(&CreateTeamRequest{ID: "eng", Name: "Before"}, "admin")
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
	assert.Contains(t, w.Body.String(), "membership is set on the key")
	assert.Contains(t, w.Body.String(), "/zzrouter/v1/keys/", "the error must name where to go instead")

	// The rename is still not applied — that is the caller's to retry —
	// but they now know exactly which field to drop.
	got, err := svc.GetTeam("eng")
	require.NoError(t, err)
	assert.Equal(t, "Before", got.Name)
}
