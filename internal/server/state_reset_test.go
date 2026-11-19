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

func confirmReset(backupID, token string) map[string]string {
	return map[string]string{"backup_id": backupID, "confirmation_token": token}
}

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

// TestStateResetKeepsTheOwnerAndReplaysItsReceipt: the reset removes what its
// preview counted; a removed key stops authenticating at once, the owner's
// session no longer resolves while the password and owner recovery work, and
// the owner's team keeps its policy. The same key and body replay the identical
// 200 in this process and, from the receipt, after the replay window and the
// token key are gone; another body is 422; one audit row in all.
func TestStateResetKeepsTheOwnerAndReplaysItsReceipt(t *testing.T) {
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
	assert.WithinDuration(t, utils.NowUTC().Add(resetConfirmationTTL), preview.ExpiresAt, 2*time.Second)

	body := confirmReset(backup.ID, preview.ConfirmationToken)
	first := resetCall(t, s, resetPath, "reset-1", body)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	receipt := peopleResponse[sqlstore.Receipt](t, first.Body.Bytes())
	assert.Equal(t, preview.Removes, receipt.Removed)
	assert.Equal(t, [3]any{backup.ID, preview.Revision, preview.Revision + 1}, [3]any{receipt.BackupID, receipt.OldRevision, receipt.NewRevision})

	_, err = s.access.AuthenticateRaw(raw)
	require.Error(t, err, "a removed key stops authenticating at once")
	status, _ = owner.call(t, http.MethodGet, "/zzrouter/v1/me", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, status, "the owner's earlier session no longer resolves")
	peopleCall[oauth.SessionInfo](t, anonymousPeopleBrowser(t, endpoint), "POST", "/session/password", map[string]string{"username": "owner", "password": personPassword})
	recovered := serveLocalGate(s, localGateRequest(http.MethodPost, "/zzrouter/v1/access/recovery/local", readLocalProof(t, s).Proof, "{}"))
	assert.Equal(t, http.StatusOK, recovered.Code, "access recover-owner still works")
	assert.Equal(t, workspace, s.accessStore.Snapshot().Team(person.TeamID), "the owner's team keeps its policy")
	assert.Nil(t, s.accessStore.Snapshot().Team("eng"))

	again := resetCall(t, s, resetPath, "reset-1", body)
	require.Equal(t, http.StatusOK, again.Code)
	assert.Equal(t, first.Body.String(), again.Body.String())
	other := confirmReset(backup.ID, "another body")
	conflict := resetCall(t, s, resetPath, "reset-1", other)
	require.Equal(t, http.StatusUnprocessableEntity, conflict.Code)
	assert.Equal(t, "idempotency_key_conflict", problemCode(t, conflict.Body.Bytes()))

	restore := utils.SetClock(utils.FixedClock(utils.NowUTC().Add(25 * time.Hour)))
	defer restore()
	s.resetKey = []byte("the key of a later process")
	conflict = resetCall(t, s, resetPath, "reset-1", other)
	require.Equal(t, http.StatusUnprocessableEntity, conflict.Code, conflict.Body.String())
	assert.Equal(t, "idempotency_key_conflict", problemCode(t, conflict.Body.Bytes()), "the receipt holds the original body's hash")
	stored := resetCall(t, s, resetPath, "reset-1", body)
	require.Equal(t, http.StatusOK, stored.Code, stored.Body.String())
	assert.Equal(t, first.Body.String(), stored.Body.String(), "the receipt answers with the original body")

	rows := resetRows(t, s)
	require.Len(t, rows, 1)
	assert.Equal(t, [2]any{audit.ActorLocalOperator, receipt.OperationID}, [2]any{rows[0].Actor, rows[0].Metadata["operation_id"]})
	all, _, err := s.accessStore.AuditPage(t.Context(), audit.Filter{}, "", audit.MaxPage)
	require.NoError(t, err)
	assert.Equal(t, history, all[len(all)-len(history):], "the history before the reset is intact")
}

// TestStateResetRefusals: request-shape refusals answer 400 before any state
// is judged; an unknown or unprotected backup is 404; a backup of another
// revision, at preview or after it, is 409 state_revision_changed; a token
// that is not this process's, is expired or names another backup is 409
// state_reset_confirmation_invalid, and one of an older generation 409
// state_revision_changed. None of them resets anything.
func TestStateResetRefusals(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	createAppKey(t, s, "reader", "")
	backup := createdBackup(t, s)
	preview := previewReset(t, s, backup.ID)
	refused := func(name string, w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		require.Equal(t, status, w.Code, "%s: %s", name, w.Body.String())
		assert.Equal(t, code, problemCode(t, w.Body.Bytes()), name)
	}

	refused("no Idempotency-Key", resetCall(t, s, resetPath, "", confirmReset(backup.ID, preview.ConfirmationToken)), http.StatusBadRequest, string(httperr.CodeRequired))
	missing := "0f6b5f7e-3f0e-4d56-9a43-2f4f5b0f6c11"
	shape := resetCall(t, s, resetPath, "shape", map[string]string{"backup_id": missing})
	require.Equal(t, http.StatusBadRequest, shape.Code, "a missing token is refused before the unknown backup")
	shape = resetCall(t, s, resetPath+"/preview", "", map[string]string{"backup_id": backup.ID, "confirmation_token": "x"})
	require.Equal(t, http.StatusBadRequest, shape.Code, "a preview takes no token")

	refused("unknown backup", resetCall(t, s, resetPath+"/preview", "", map[string]string{"backup_id": missing}), http.StatusNotFound, string(httperr.CodeStateBackupNotFound))
	refused("unknown backup at reset", resetCall(t, s, resetPath, "unknown", confirmReset(missing, preview.ConfirmationToken)), http.StatusNotFound, string(httperr.CodeStateBackupNotFound))
	restore := utils.SetClock(utils.FixedClock(backup.ProtectedUntil.Add(-time.Minute)))
	assert.Equal(t, time.Unix(backup.ProtectedUntil.Unix(), 0).UTC(), previewReset(t, s, backup.ID).ExpiresAt, "a token never outlives the backup's protection")
	restore()
	restore = utils.SetClock(utils.FixedClock(backup.ProtectedUntil))
	refused("backup past protected_until", resetCall(t, s, resetPath+"/preview", "", map[string]string{"backup_id": backup.ID}), http.StatusNotFound, string(httperr.CodeStateBackupNotFound))
	restore()

	newer := createdBackup(t, s)
	state := resetState{preview.StoreID, preview.Generation, preview.Revision}
	future := utils.NowUTC().Add(time.Minute).Unix()
	tampered := []byte(preview.ConfirmationToken)
	tampered[3] ^= 1
	for name, tc := range map[string]struct {
		backupID, token, code string
	}{
		"garbage":         {backup.ID, "not-a-token", string(httperr.CodeStateResetConfirmationInvalid)},
		"tampered":        {backup.ID, string(tampered), string(httperr.CodeStateResetConfirmationInvalid)},
		"another process": {backup.ID, (&Server{resetKey: []byte("another process")}).signConfirmation(confirmation{state, backup.ID, future}), string(httperr.CodeStateResetConfirmationInvalid)},
		"expired":         {backup.ID, s.signConfirmation(confirmation{state, backup.ID, utils.NowUTC().Unix() - 1}), string(httperr.CodeStateResetConfirmationInvalid)},
		"other backup":    {newer.ID, preview.ConfirmationToken, string(httperr.CodeStateResetConfirmationInvalid)},
		"older generation": {backup.ID, s.signConfirmation(confirmation{resetState{state.StoreID, "an earlier generation", state.Revision}, backup.ID, future}),
			string(httperr.CodeStateRevisionChanged)},
	} {
		refused(name, resetCall(t, s, resetPath, name, confirmReset(tc.backupID, tc.token)), http.StatusConflict, tc.code)
	}

	createAppKey(t, s, "writer", "")
	refused("revision changed after the preview", resetCall(t, s, resetPath, "moved", confirmReset(backup.ID, preview.ConfirmationToken)), http.StatusConflict, string(httperr.CodeStateRevisionChanged))
	refused("backup older than the records", resetCall(t, s, resetPath+"/preview", "", map[string]string{"backup_id": newer.ID}), http.StatusConflict, string(httperr.CodeStateRevisionChanged))
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
		admin := makeRequest(t, s, TestRequest{Method: http.MethodPost, Path: path, Body: confirmReset(backup.ID, "x"),
			Headers: map[string]string{"X-API-Key": TestAdminKey, idempotencyHeader: "admin"}})
		assert.Equal(t, http.StatusForbidden, admin.Code, path)
		w := serveLocalGate(worker, localGateRequest(http.MethodPost, path, "proof", "{}"))
		assert.Equal(t, http.StatusMisdirectedRequest, w.Code, path)
		assert.Equal(t, authLocal, s.authTier(http.MethodPost, path))
	}
	assert.Empty(t, resetRows(t, s))
	assert.NotEmpty(t, previewReset(t, s, backup.ID).ConfirmationToken, "the local operator is admitted")
}
