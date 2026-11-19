package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stperic/common/mcp/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/recovery"
	"github.com/stperic/zzrouter/pkg/utils"
)

const resetPath = "/zzrouter/v1/system/state/reset"

// resetCall posts body to path as the local operator, under the Idempotency-Key key unless it is empty.
func resetCall(t *testing.T, s *Server, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	r := localGateRequest(http.MethodPost, path, readLocalProof(t, s).Proof, string(data))
	if key != "" {
		r.Header.Set(idempotencyHeader, key)
	}
	return serveLocalGate(s, r)
}

func previewReset(t *testing.T, s *Server, backupID string) resetPreview {
	t.Helper()
	w := resetCall(t, s, resetPath+"/preview", "", map[string]string{"backup_id": backupID})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return peopleResponse[resetPreview](t, w.Body.Bytes())
}

func resetBody(backupID string) map[string]string { return map[string]string{"backup_id": backupID} }

func resetRows(t *testing.T, s *Server) []audit.Event {
	t.Helper()
	events, _, err := s.accessStore.AuditPage(t.Context(), audit.Filter{Action: audit.ActionStoreReset}, "", audit.MaxPage)
	require.NoError(t, err)
	return events
}

// createAppKey creates a key in teamID, or with its personal team when teamID is empty.
func createAppKey(t *testing.T, s *Server, id, teamID string) string {
	t.Helper()
	body := map[string]any{"id": id, "name": id}
	if teamID != "" {
		body["team_id"], body["team_role"] = teamID, "member"
	}
	created := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/keys", TestAdminKey, body)
	require.Equal(t, http.StatusCreated, created.Code, string(created.Body))
	return peopleResponse[CreateKeyResponse](t, created.Body).RawKey
}

// TestStateResetKeepsTheOwner: the reset removes what its preview counted; a
// removed key stops authenticating at once, the owner's session no longer
// resolves while the password and owner recovery work, and the owner's team
// keeps its policy. The same key and body replay the identical 200 in this
// process and another body is 422. A repeat that is not a replay, as after a
// restart, meets the changed revision with 409 naming store.reset, and exactly
// one store.reset row exists.
func TestStateResetKeepsTheOwner(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeCoordinator)
	owner, person := createBrowserOwner(t, s, endpoint)
	raw := createAppKey(t, s, "reader", person.TeamID) // its team stays, so only the cache could still admit it
	createAppKey(t, s, "solo", "")
	team := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/teams", TestAdminKey, map[string]any{"id": "eng", "name": "Eng"})
	require.Equal(t, http.StatusCreated, team.Code, string(team.Body))
	_, err := s.access.AuthenticateRaw(raw)
	require.NoError(t, err, "the key is in the authentication cache")
	status, _ := owner.call(t, http.MethodGet, "/zzrouter/v1/me", nil, nil)
	require.Equal(t, http.StatusOK, status)
	workspace := s.accessStore.Snapshot().Team(person.TeamID)
	history, _, err := s.accessStore.AuditPage(t.Context(), audit.Filter{}, "", audit.MaxPage)
	require.NoError(t, err)

	backup := createdBackup(t, s)
	preview := previewReset(t, s, backup.ID)
	assert.Equal(t, [3]int{2, 2, 0}, [3]int{preview.Removes.AppKeys, preview.Removes.Teams, preview.Removes.People})
	assert.Equal(t, &resetKeeps{OwnerID: person.ID, OwnerUsername: "owner", TeamID: workspace.ID, TeamName: workspace.Name}, preview.Keeps)
	assert.Equal(t, [2]any{backup.ID, backup.Revision}, [2]any{preview.BackupID, preview.Revision})

	first := resetCall(t, s, resetPath, "reset-1", resetBody(backup.ID))
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	result := peopleResponse[sqlstore.ResetResult](t, first.Body.Bytes())
	assert.Equal(t, preview.Removes, result.Removed)
	assert.Equal(t, [3]any{backup.ID, preview.Revision, preview.Revision + 1}, [3]any{result.BackupID, result.OldRevision, result.NewRevision})

	_, err = s.access.AuthenticateRaw(raw)
	require.Error(t, err, "a removed key stops authenticating at once")
	status, _ = owner.call(t, http.MethodGet, "/zzrouter/v1/me", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, status, "the owner's earlier session no longer resolves")
	peopleCall[oauth.SessionInfo](t, anonymousPeopleBrowser(t, endpoint), "POST", "/session/password", map[string]string{"username": "owner", "password": personPassword})
	recovered := serveLocalGate(s, localGateRequest(http.MethodPost, "/zzrouter/v1/access/recovery/local", readLocalProof(t, s).Proof, "{}"))
	assert.Equal(t, http.StatusOK, recovered.Code, "access recover-owner still works")
	assert.Equal(t, workspace, s.accessStore.Snapshot().Team(person.TeamID), "the owner's team keeps its policy")
	assert.Nil(t, s.accessStore.Snapshot().Team("eng"))

	again := resetCall(t, s, resetPath, "reset-1", resetBody(backup.ID))
	require.Equal(t, http.StatusOK, again.Code)
	assert.Equal(t, first.Body.String(), again.Body.String(), "the same key and body replay in this process")
	conflict := resetCall(t, s, resetPath, "reset-1", map[string]string{"backup_id": "another body"})
	require.Equal(t, http.StatusUnprocessableEntity, conflict.Code)
	assert.Equal(t, "idempotency_key_conflict", problemCode(t, conflict.Body.Bytes()))
	for _, key := range []string{"", "after a restart"} {
		repeat := resetCall(t, s, resetPath, key, resetBody(backup.ID))
		require.Equal(t, http.StatusConflict, repeat.Code, repeat.Body.String())
		assert.Equal(t, string(httperr.CodeStateRevisionChanged), problemCode(t, repeat.Body.Bytes()))
		assert.Contains(t, repeat.Body.String(), "it shows in the access history as store.reset")
	}

	rows := resetRows(t, s)
	require.Len(t, rows, 1)
	assert.Equal(t, [3]any{audit.ActorLocalOperator, result.OperationID, backup.ID}, [3]any{rows[0].Actor, rows[0].Metadata["operation_id"], rows[0].Metadata["backup_id"]})
	all, _, err := s.accessStore.AuditPage(t.Context(), audit.Filter{}, "", audit.MaxPage)
	require.NoError(t, err)
	assert.Equal(t, history, all[len(all)-len(history):], "the history before the reset is intact")
}

// TestStateResetRefusals: a malformed body answers 400 before any backup is
// judged; an unknown backup is 404; a backup still listed but past its
// protected_until is 409 state_backup_unprotected at preview and at reset; a
// backup of another revision, at preview or after it, is 409
// state_revision_changed. None of them resets anything.
func TestStateResetRefusals(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	createAppKey(t, s, "reader", "")
	backup := createdBackup(t, s)
	previewReset(t, s, backup.ID)
	refused := func(name string, w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		require.Equal(t, status, w.Code, "%s: %s", name, w.Body.String())
		assert.Equal(t, code, problemCode(t, w.Body.Bytes()), name)
	}

	missing := "0f6b5f7e-3f0e-4d56-9a43-2f4f5b0f6c11"
	for _, path := range []string{resetPath + "/preview", resetPath} {
		shape := resetCall(t, s, path, "", map[string]string{"backup_id": missing, "confirmation_token": "x"})
		require.Equal(t, http.StatusBadRequest, shape.Code, "%s: an unknown field is refused before the unknown backup", path)
		shape = resetCall(t, s, path, "", map[string]string{})
		require.Equal(t, http.StatusBadRequest, shape.Code, "%s: backup_id is required", path)
		refused(path+" unknown backup", resetCall(t, s, path, "", resetBody(missing)), http.StatusNotFound, string(httperr.CodeStateBackupNotFound))
	}

	restore := utils.SetClock(utils.FixedClock(backup.ProtectedUntil.Add(time.Hour)))
	listed := makeAuthRequest(t, s, http.MethodGet, backupsPath, TestAdminKey, nil)
	require.Contains(t, string(listed.Body), backup.ID, "still listed")
	for _, path := range []string{resetPath + "/preview", resetPath} {
		w := resetCall(t, s, path, "", resetBody(backup.ID))
		refused(path+" past protected_until", w, http.StatusConflict, string(httperr.CodeStateBackupUnprotected))
		assert.Contains(t, w.Body.String(), "Take a new backup", path)
		assert.NotContains(t, w.Body.String(), "use one of their IDs", path)
	}
	restore()

	newer := createdBackup(t, s)
	createAppKey(t, s, "writer", "")
	refused("revision changed after the preview", resetCall(t, s, resetPath, "moved", resetBody(newer.ID)), http.StatusConflict, string(httperr.CodeStateRevisionChanged))
	refused("backup older than the records", resetCall(t, s, resetPath+"/preview", "", resetBody(newer.ID)), http.StatusConflict, string(httperr.CodeStateRevisionChanged))
	assert.Empty(t, resetRows(t, s))
	assert.NotNil(t, s.accessStore.Snapshot().Key("reader"), "nothing was reset")
}

// TestStateResetGate: both routes answer the local proof only; an
// administrator key alone is 403 and a worker 421. The catalog marks them
// local, so neither discovery nor zz_api offers them
// (TestLocalOperatorRoutesStayOutOfAgentSurfaces walks every local route).
func TestStateResetGate(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	worker, _ := oauthTestNode(t, config.ClusterModeWorker)
	backup := createdBackup(t, s)
	for _, path := range []string{resetPath + "/preview", resetPath} {
		admin := makeRequest(t, s, TestRequest{Method: http.MethodPost, Path: path, Body: resetBody(backup.ID),
			Headers: map[string]string{"X-API-Key": TestAdminKey, idempotencyHeader: "admin"}})
		assert.Equal(t, http.StatusForbidden, admin.Code, path)
		w := serveLocalGate(worker, localGateRequest(http.MethodPost, path, "proof", "{}"))
		assert.Equal(t, http.StatusMisdirectedRequest, w.Code, path)
		assert.Equal(t, authLocal, s.authTier(http.MethodPost, path))
	}
	assert.Empty(t, resetRows(t, s))
	assert.Equal(t, backup.ID, previewReset(t, s, backup.ID).BackupID, "the local operator is admitted")
}

// TestBacksResetJudgesTheBackupItself: a backup backs a reset only when it is
// this store's, of this generation and revision, and still inside its
// protected window; the window ends at protected_until itself.
func TestBacksResetJudgesTheBackupItself(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	t.Cleanup(utils.SetClock(utils.FixedClock(now)))
	live := sqlstore.Meta{StoreID: "store", Generation: "generation", Revision: 7}
	backup := func(edit func(*recovery.OnlineBackup)) recovery.OnlineBackup {
		b := recovery.OnlineBackup{ProtectedUntil: now.Add(time.Second)}
		b.StoreMeta = &recovery.StoreMeta{StoreID: live.StoreID, Generation: live.Generation, Revision: live.Revision}
		if edit != nil {
			edit(&b)
		}
		return b
	}
	require.NoError(t, backsReset(backup(nil), nil, live))
	for name, tc := range map[string]struct {
		edit func(*recovery.OnlineBackup)
		want error
	}{
		"another store":              {func(b *recovery.OnlineBackup) { b.StoreID = "other" }, errStateRevisionChanged},
		"another generation":         {func(b *recovery.OnlineBackup) { b.Generation = "earlier" }, errStateRevisionChanged},
		"another revision":           {func(b *recovery.OnlineBackup) { b.Revision = 6 }, errStateRevisionChanged},
		"exactly at protected_until": {func(b *recovery.OnlineBackup) { b.ProtectedUntil = now }, errBackupUnprotected},
		"past protected_until":       {func(b *recovery.OnlineBackup) { b.ProtectedUntil = now.Add(-time.Hour) }, errBackupUnprotected},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, backsReset(backup(tc.edit), nil, live), tc.want)
		})
	}
	require.ErrorIs(t, backsReset(backup(nil), recovery.ErrBackupNotFound, live), recovery.ErrBackupNotFound)
}
