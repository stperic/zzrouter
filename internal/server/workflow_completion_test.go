package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/accesstest"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/recovery"
	"github.com/stretchr/testify/require"
)

func TestIssuerSharingAssessment(t *testing.T) {
	for issuer, expected := range map[string]string{"http://localhost:9090": "local_only", "https://localhost": "local_only", "http://127.0.0.1": "local_only", "http://[::1]:9090": "local_only", "https://router.example": "configured", "http://router.example": "unavailable", "": "unavailable"} {
		require.Equal(t, expected, issuerSharing(issuer), issuer)
	}
}

func TestWorkflowAuditAndRecoveryRequireAdmin(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeCoordinator)
	// The ordinary OAuth fixture intentionally has no persisted node/provider
	// configuration. Add a real disposable installation for inventory reads.
	require.NoError(t, os.WriteFile(filepath.Join(s.configDir, "node.yaml"), []byte("node: {name: test-host, port: 9090}\n"), 0o600))
	_, err := templates.PrepareProviders(filepath.Join(s.configDir, "providers"))
	require.NoError(t, err)
	cfg, err := config.ReadExistingNodeConfig(s.configDir)
	require.NoError(t, err)
	apps, err := config.LoadAppsConfig(filepath.Join(s.configDir, "providers"))
	require.NoError(t, err)
	inventory, err := recovery.Capture(t.Context(), cfg, apps)
	require.NoError(t, err)
	require.NoError(t, recovery.Save(t.Context(), inventory))
	for _, path := range []string{"/zzrouter/v1/access/audit", "/zzrouter/v1/system/recovery"} {
		for _, credential := range []struct {
			key  string
			code int
		}{{"", http.StatusUnauthorized}, {TestUserKey, http.StatusForbidden}, {TestAdminKey, http.StatusOK}} {
			result := makeAuthRequest(t, s, "GET", path, credential.key, nil)
			require.Equal(t, credential.code, result.Code, string(result.Body))
		}
	}
	_, owner := createBrowserOwner(t, s, endpoint)
	pageResponse := makeAuthRequest(t, s, "GET", "/zzrouter/v1/access/audit?limit=1", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, pageResponse.Code)
	var response struct {
		Data auditPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(pageResponse.Body, &response))
	require.Len(t, response.Data.Events, 1)
	require.Equal(t, audit.ActionPersonCreated, response.Data.Events[0].Action)
	require.Equal(t, audit.KindActor(people.Kind, owner.ID), response.Data.Events[0].Actor)
	require.NotEmpty(t, response.Data.Events[0].RequestID)
	require.NotEmpty(t, response.Data.Next)
	for cursor, code := range map[string]int{"malformed": http.StatusConflict, strings.Repeat("x", 4096): http.StatusBadRequest} {
		stale := makeAuthRequest(t, s, "GET", "/zzrouter/v1/access/audit?cursor="+cursor, TestAdminKey, nil)
		require.Equal(t, code, stale.Code, "a cursor no store issues is refused; one longer than any it issues is a bad parameter")
	}
	for _, limit := range []string{"0", "101", "invalid"} {
		result := makeAuthRequest(t, s, "GET", "/zzrouter/v1/access/audit?limit="+limit, TestAdminKey, nil)
		require.Equal(t, http.StatusBadRequest, result.Code)
	}
}

// A key joining a shared team and that team's deletion are one after the
// other, never both, in either order: both wait at the store's writer gate.
func TestSharedKeyEnrollmentAndTeamDeletionSerialize(t *testing.T) {
	svc, store, access := newTeamsTestHarness(t)
	keyService := NewKeysService(store, access)
	_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "gate", Name: "Gate"}, "operator")
	require.NoError(t, err)
	joined := map[bool]int{}
	for round := 0; joined[true] == 0 || joined[false] == 0; round++ {
		require.Less(t, round, 12, "both orders occur")
		teamID, keyID := fmt.Sprint("shared-", round), fmt.Sprint("app-", round)
		_, err := svc.CreateTeam(t.Context(), &CreateTeamRequest{ID: teamID, Name: "Shared"}, "operator")
		require.NoError(t, err)
		var createErr, deleteErr error
		create := func() {
			_, createErr = keyService.CreateKey(context.Background(), keyID, &CreateKeyRequest{Name: "App", TeamID: teamID}, "operator")
		}
		remove := func() { deleteErr = svc.DeleteTeam(context.Background(), teamID, "operator") }
		if round%2 == 0 {
			accesstest.QueuedRace(t, store, "gate", create, remove)
		} else {
			accesstest.QueuedRace(t, store, "gate", remove, create)
		}
		if createErr == nil {
			require.ErrorIs(t, deleteErr, teams.ErrTeamHasMembers)
			team, err := svc.GetTeam(teamID)
			require.NoError(t, err)
			require.Equal(t, 1, team.Membership.AppKeys)
			require.False(t, team.Membership.CanDelete)
		} else {
			require.ErrorIs(t, createErr, teams.ErrTeamNotFound)
			require.True(t, isInvalidInput(createErr), "joining a deleted team is the caller's error: %v", createErr)
			require.NoError(t, deleteErr)
			require.Nil(t, store.Snapshot().Key(keyID))
		}
		joined[createErr == nil]++
	}
}
