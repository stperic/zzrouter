package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/recovery"
)

const backupsPath = statePath + "/backups"

// backupAnswer reads an answer of the backup routes, checks it names no path
// and no secret, and returns its data checked against the published schema.
func backupAnswer(t *testing.T, s *Server, code int, body []byte, secrets ...string) json.RawMessage {
	t.Helper()
	for _, forbidden := range append([]string{s.configDir, "access/backups", ".tar.gz", "access.sqlite"}, secrets...) {
		require.NotContains(t, string(body), forbidden, "no path or secret")
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope), string(body))
	if code >= 300 {
		return nil
	}
	var items []any
	if json.Unmarshal(envelope.Data, &items) != nil {
		items = []any{nil}
		require.NoError(t, json.Unmarshal(envelope.Data, &items[0]))
	}
	for _, item := range items {
		require.NoError(t, browserSchema(t, "StateBackup").Validate(item), string(body))
	}
	return envelope.Data
}

func decodeBackup(t *testing.T, data json.RawMessage) recovery.OnlineBackup {
	t.Helper()
	var backup recovery.OnlineBackup
	require.NoError(t, json.Unmarshal(data, &backup))
	return backup
}

func createBackupAs(t *testing.T, s *Server, key, idempotency string, body any) *TestResponse {
	t.Helper()
	headers := map[string]string{"X-API-Key": key}
	if idempotency != "" {
		headers[idempotencyHeader] = idempotency
	}
	return makeRequest(t, s, TestRequest{Method: http.MethodPost, Path: backupsPath, Headers: headers, Body: body})
}

// TestStateBackupsCreateListReadReplay: an administrator creates a backup,
// lists and reads it, and the status reports it; the same Idempotency-Key and
// body replay the original 201, another body is refused with 422, and an
// unknown ID is 404. zz_api and discovery reach all three routes. No answer
// names a path or carries a secret.
func TestStateBackupsCreateListReadReplay(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	key := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/keys", TestAdminKey, map[string]any{"id": "reader", "name": "Reader"})
	require.Equal(t, http.StatusCreated, key.Code, string(key.Body))
	var raw struct {
		Data CreateKeyResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(key.Body, &raw))
	secret := raw.Data.RawKey

	created := createBackupAs(t, s, TestAdminKey, "backup-1", map[string]any{})
	require.Equal(t, http.StatusCreated, created.Code, string(created.Body))
	assert.Equal(t, "no-store", created.Headers.Get("Cache-Control"))
	backup := decodeBackup(t, backupAnswer(t, s, created.Code, created.Body, secret))
	meta := s.stateStore.Meta()
	assert.Equal(t, meta.Revision, backup.Revision)
	assert.Equal(t, meta.Sequence-1, backup.Sequence, "its own audit row came after the copy")
	assert.True(t, backup.Readable)
	assert.Equal(t, backup.CreatedAt.Add(24*time.Hour), backup.ProtectedUntil)

	replayed := createBackupAs(t, s, TestAdminKey, "backup-1", map[string]any{})
	require.Equal(t, http.StatusCreated, replayed.Code)
	assert.Equal(t, string(created.Body), string(replayed.Body), "the original answer, the same ID")
	conflict := createBackupAs(t, s, TestAdminKey, "backup-1", map[string]any{"note": "other"})
	require.Equal(t, http.StatusUnprocessableEntity, conflict.Code, string(conflict.Body))
	assert.Equal(t, "idempotency_key_conflict", problemCode(t, conflict.Body))
	for _, body := range []any{map[string]any{"note": "x"}, []any{}, "text", nil} {
		refused := createBackupAs(t, s, TestAdminKey, "", body)
		require.Equal(t, http.StatusBadRequest, refused.Code, "%v: %s", body, refused.Body)
	}
	require.Len(t, s.backups.List(), 1, "neither replay nor refusal created another")

	listed := makeAuthRequest(t, s, http.MethodGet, backupsPath, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, listed.Code, string(listed.Body))
	var list []recovery.OnlineBackup
	require.NoError(t, json.Unmarshal(backupAnswer(t, s, listed.Code, listed.Body, secret), &list))
	require.Len(t, list, 1)
	assert.Equal(t, backup.ID, list[0].ID)
	read := makeAuthRequest(t, s, http.MethodGet, backupsPath+"/"+backup.ID, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, read.Code, string(read.Body))
	assert.JSONEq(t, string(backupAnswer(t, s, created.Code, created.Body)), string(backupAnswer(t, s, read.Code, read.Body, secret)))
	missing := makeAuthRequest(t, s, http.MethodGet, backupsPath+"/0f6b5f7e-3f0e-4d56-9a43-2f4f5b0f6c11", TestAdminKey, nil)
	require.Equal(t, http.StatusNotFound, missing.Code, string(missing.Body))
	assert.Equal(t, string(httperr.CodeStateBackupNotFound), problemCode(t, missing.Body))
	assert.Contains(t, string(missing.Body), "zz_api operation listStateBackups", "the next step names the tool")
	backupAnswer(t, s, missing.Code, missing.Body, secret)

	state, raw2 := readStoreState(t, s)
	assert.Equal(t, []retainedCopy{{Kind: "online_backup", ID: backup.ID, CreatedAt: backup.CreatedAt, SizeBytes: backup.SizeBytes}}, state.RetainedCopies)
	assert.Equal(t, backup.SizeBytes, state.Size.BackupBytes)
	assert.NotContains(t, fmt.Sprint(raw2), secret)

	discovered := map[string]bool{}
	for _, routes := range collectAPIDiscoveryGroups(s) {
		for _, route := range routes {
			discovered[route] = true
		}
	}
	for method, operations := range map[string]map[string]string{
		http.MethodGet:  {backupsPath: "listStateBackups", backupsPath + "/" + backup.ID: "getStateBackup"},
		http.MethodPost: {backupsPath: "createStateBackup"},
	} {
		for path, want := range operations {
			operation, err := s.eligibleAPIRoute(method, path)
			require.NoError(t, err, "zz_api reaches %s %s", method, path)
			assert.Equal(t, want, operation)
			assert.True(t, discovered[method+" "+strings.Replace(path, backup.ID, "{id}", 1)], "discovery lists %s %s", method, path)
		}
	}
	assert.Equal(t, "admin", s.authTier(http.MethodPost, backupsPath), "the create keeps its admin tier")
}

// TestStateBackupsAuthorities: the create answers the local proof alone, an
// administrator key alone and an administrator's browser session with its
// CSRF token; a session without the token is refused, and so is a request
// carrying a bad or an empty proof header even with an administrator key.
// The reads refuse the proof alone, a user key is refused everywhere, and a
// worker answers the coordinator-role 421.
func TestStateBackupsAuthorities(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeCoordinator)
	file := readLocalProof(t, s)
	browser := newSessionBrowser(t, endpoint, TestAdminKey)
	status, body := browser.call(t, http.MethodPost, backupsPath, map[string]any{}, nil)
	require.Equal(t, http.StatusCreated, status, string(body))
	status, body = browser.call(t, http.MethodPost, backupsPath, map[string]any{}, map[string]string{"X-CSRF-Token": ""})
	require.Equal(t, http.StatusForbidden, status, string(body))
	require.Len(t, s.backups.List(), 1)

	local := serveLocalGate(s, localGateRequest(http.MethodPost, backupsPath, file.Proof, "{}"))
	require.Equal(t, http.StatusCreated, local.Code, local.Body.String())
	byOperator := decodeBackup(t, backupAnswer(t, s, local.Code, local.Body.Bytes(), file.Proof))
	admin := createBackupAs(t, s, TestAdminKey, "", map[string]any{})
	require.Equal(t, http.StatusCreated, admin.Code, string(admin.Body))
	byAdmin := decodeBackup(t, backupAnswer(t, s, admin.Code, admin.Body))
	events, _, err := s.accessStore.AuditPage(t.Context(), audit.Filter{Action: audit.ActionBackupCreated}, "", audit.MaxPage)
	require.NoError(t, err)
	actors := map[string]string{}
	for _, ev := range events {
		actors[ev.Subject] = ev.Actor
	}
	assert.Equal(t, audit.ActorLocalOperator, actors[byOperator.ID])
	assert.Equal(t, "static:admin", actors[byAdmin.ID])

	for _, proof := range []string{strings.Repeat("x", 43), ""} {
		r := localGateRequest(http.MethodPost, backupsPath, proof, "{}")
		r.Header.Set("X-API-Key", TestAdminKey)
		assert.Equal(t, http.StatusForbidden, serveLocalGate(s, r).Code, "a proof header %q is judged alone", proof)
	}
	for _, path := range []string{backupsPath, backupsPath + "/" + byAdmin.ID} {
		w := serveLocalGate(s, localGateRequest(http.MethodGet, path, file.Proof, ""))
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s refuses the proof alone: %s", path, w.Body.String())
	}

	worker, _ := oauthTestNode(t, config.ClusterModeWorker)
	for _, c := range []struct {
		node         *Server
		method, path string
		key          string
		code         int
	}{
		{s, http.MethodPost, backupsPath, "", http.StatusUnauthorized},
		{s, http.MethodPost, backupsPath, TestUserKey, http.StatusForbidden},
		{s, http.MethodGet, backupsPath, TestUserKey, http.StatusForbidden},
		{s, http.MethodGet, backupsPath + "/" + byAdmin.ID, TestUserKey, http.StatusForbidden},
		{worker, http.MethodPost, backupsPath, TestAdminKey, http.StatusMisdirectedRequest},
		{worker, http.MethodGet, backupsPath, TestAdminKey, http.StatusMisdirectedRequest},
		{worker, http.MethodGet, backupsPath + "/" + byAdmin.ID, TestAdminKey, http.StatusMisdirectedRequest},
	} {
		var body any
		if c.method == http.MethodPost {
			body = map[string]any{}
		}
		result := makeAuthRequest(t, c.node, c.method, c.path, c.key, body)
		assert.Equal(t, c.code, result.Code, "%s %s with %q: %s", c.method, c.path, c.key, result.Body)
		assert.NotContains(t, string(result.Body), c.node.configDir)
	}
	require.Len(t, s.backups.List(), 3, "no refused request created a backup")
}

// TestStateBackupPrincipalsNeverCollide: an app key ID with ':' is refused,
// so no key can take a name of another kind of caller; a key whose ID is
// "admin" neither replays the static administrator's answer nor is audited as
// it, and the local operator and an administrator do not replay each other.
func TestStateBackupPrincipalsNeverCollide(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	for _, body := range []map[string]any{
		{"id": "local:operator", "name": "Twin", "role": "admin"},
		{"id": "static:admin", "name": "Twin", "role": "admin"},
		{"id": "a:b", "name": "Twin", "team_id": "missing"}, // malformed before any record is consulted
	} {
		refused := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/keys", TestAdminKey, body)
		require.Equal(t, http.StatusBadRequest, refused.Code, "%v: %s", body, refused.Body)
	}
	file := readLocalProof(t, s)
	byOperator := func() string {
		r := localGateRequest(http.MethodPost, backupsPath, file.Proof, "{}")
		r.Header.Set(idempotencyHeader, "same")
		w := serveLocalGate(s, r)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		return decodeBackup(t, backupAnswer(t, s, w.Code, w.Body.Bytes())).ID
	}
	byKey := func(key string) string {
		created := createBackupAs(t, s, key, "same", map[string]any{})
		require.Equal(t, http.StatusCreated, created.Code, string(created.Body))
		return decodeBackup(t, backupAnswer(t, s, created.Code, created.Body)).ID
	}
	operator, static := byOperator(), byKey(TestAdminKey)
	require.Equal(t, operator, byOperator(), "the operator replays its own answer")
	require.Equal(t, static, byKey(TestAdminKey))
	key := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/keys", TestAdminKey, map[string]any{"id": "admin", "name": "Twin", "role": "admin"})
	require.Equal(t, http.StatusCreated, key.Code, string(key.Body))
	var raw struct {
		Data CreateKeyResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(key.Body, &raw))
	twin := byKey(raw.Data.RawKey)
	require.NotContains(t, []string{operator, static}, twin, "no replay across callers")
	events, _, err := s.accessStore.AuditPage(t.Context(), audit.Filter{Action: audit.ActionBackupCreated}, "", audit.MaxPage)
	require.NoError(t, err)
	actors := map[string]string{}
	for _, ev := range events {
		actors[ev.Subject] = ev.Actor
	}
	require.Equal(t, map[string]string{operator: audit.ActorLocalOperator, static: "static:admin", twin: "admin"}, actors)
}

// hookedStore is the server's live store with a test's Capture or Record.
type hookedStore struct {
	*sqlstore.Store
	capture func(ctx context.Context, dst string) (sqlstore.Meta, error)
	record  func(ctx context.Context, ev audit.Event) error
}

func (h *hookedStore) Capture(ctx context.Context, dst string) (sqlstore.Meta, error) {
	if h.capture != nil {
		return h.capture(ctx, dst)
	}
	return h.Store.Capture(ctx, dst)
}

func (h *hookedStore) Record(ctx context.Context, ev audit.Event) error {
	if h.record != nil {
		return h.record(ctx, ev)
	}
	return h.Store.Record(ctx, ev)
}

// hookBackups replaces s's online backups with ones over a hooked store.
func hookBackups(t *testing.T, s *Server) *hookedStore {
	t.Helper()
	h := &hookedStore{Store: s.stateStore}
	s.backups.Close()
	var err error
	s.backups, err = recovery.OpenBackups(t.Context(), s.configDir, h)
	require.NoError(t, err)
	return h
}

// TestStateBackupAuditRowBeforeListing: when the "backup created" row is
// committed and readable in the access history, GET /backups does not list
// the backup yet; the create's answer then does.
func TestStateBackupAuditRowBeforeListing(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	h := hookBackups(t, s)
	hooked := 0
	h.record = func(ctx context.Context, ev audit.Event) error {
		err := h.Store.Record(ctx, ev)
		hooked++
		history := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/access/audit?action="+string(audit.ActionBackupCreated), TestAdminKey, nil)
		require.Equal(t, http.StatusOK, history.Code, string(history.Body))
		require.Contains(t, string(history.Body), ev.Subject, "the row is committed")
		listed := makeAuthRequest(t, s, http.MethodGet, backupsPath, TestAdminKey, nil)
		require.Equal(t, http.StatusOK, listed.Code)
		require.NotContains(t, string(listed.Body), ev.Subject, "and the backup is not listed")
		return err
	}
	created := createBackupAs(t, s, TestAdminKey, "", map[string]any{})
	require.Equal(t, http.StatusCreated, created.Code, string(created.Body))
	require.Equal(t, 1, hooked)
	listed := makeAuthRequest(t, s, http.MethodGet, backupsPath, TestAdminKey, nil)
	require.Contains(t, string(listed.Body), decodeBackup(t, backupAnswer(t, s, created.Code, created.Body)).ID)
}

// TestStateBackupFailureAnswers: a full disk answers 507 with capacity disk,
// a copy that fails its check 500 state_image_check_failed, a busy store 503
// access_store_busy; none lists a backup or names a path or database text.
func TestStateBackupFailureAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		capture func(h *hookedStore) func(context.Context, string) (sqlstore.Meta, error)
		code    int
		problem string
		extra   map[string]any
	}{
		"a full disk": {
			capture: func(*hookedStore) func(context.Context, string) (sqlstore.Meta, error) {
				return func(context.Context, string) (sqlstore.Meta, error) {
					errno := syscall.ENOSPC
					if runtime.GOOS == "windows" {
						errno = syscall.Errno(112) // ERROR_DISK_FULL
					}
					return sqlstore.Meta{}, fmt.Errorf("copy /secret/path: %w", errno)
				}
			},
			code: http.StatusInsufficientStorage, problem: string(httperr.CodeStateBackupCapacityExceeded), extra: map[string]any{"capacity": "disk"},
		},
		"a copy that is not the capture's": {
			capture: func(h *hookedStore) func(context.Context, string) (sqlstore.Meta, error) {
				return func(ctx context.Context, dst string) (sqlstore.Meta, error) {
					meta, err := h.Store.Capture(ctx, dst)
					meta.Revision++ // not the state the store published
					return meta, err
				}
			},
			code: http.StatusInternalServerError, problem: string(httperr.CodeStateImageCheckFailed),
		},
		"a busy store": {
			capture: func(*hookedStore) func(context.Context, string) (sqlstore.Meta, error) {
				return func(context.Context, string) (sqlstore.Meta, error) { return sqlstore.Meta{}, sqlstore.ErrBusy }
			},
			code: http.StatusServiceUnavailable, problem: string(httperr.CodeAccessStoreBusy),
		},
		"a stopping coordinator": {
			capture: func(*hookedStore) func(context.Context, string) (sqlstore.Meta, error) {
				return func(context.Context, string) (sqlstore.Meta, error) { return sqlstore.Meta{}, access.ErrClosed }
			},
			code: http.StatusServiceUnavailable, problem: "service_unavailable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
			h := hookBackups(t, s)
			h.capture = tc.capture(h)
			result := createBackupAs(t, s, TestAdminKey, "", map[string]any{})
			require.Equal(t, tc.code, result.Code, string(result.Body))
			assert.Equal(t, tc.problem, problemCode(t, result.Body))
			for _, forbidden := range []string{s.configDir, "/secret/path", "SQL", "sqlite"} {
				assert.NotContains(t, string(result.Body), forbidden)
			}
			var problem map[string]any
			require.NoError(t, json.Unmarshal(result.Body, &problem))
			for key, want := range tc.extra {
				assert.Equal(t, want, problem[key], key)
			}
			assert.Empty(t, s.backups.List())
			if tc.code == http.StatusServiceUnavailable {
				assert.NotEmpty(t, result.Headers.Get("Retry-After"))
			}
		})
	}
}

// TestBackupCapacityAnswerCarriesItsFigures: the byte cap's 507 names the
// cap, what is retained and what the backup needs, and one next step that
// never says to retry at a time; a full disk says to free space.
func TestBackupCapacityAnswerCarriesItsFigures(t *testing.T) {
	wire, ok := knownFailure(fmt.Errorf("create: %w", &recovery.CapacityError{LimitBytes: 100, RetainedBytes: 70, RequiredBytes: 40}))
	require.True(t, ok)
	assert.Equal(t, http.StatusInsufficientStorage, wire.Status)
	assert.Equal(t, string(httperr.CodeStateBackupCapacityExceeded), wire.Code)
	assert.Equal(t, map[string]any{"capacity": "byte_cap", "limit_bytes": int64(100), "retained_bytes": int64(70), "required_bytes": int64(40)}, wire.Extra)
	assert.Contains(t, wire.Message, "expires_at")
	assert.Contains(t, wire.Message, "zzrouter-node recovery backup")
	assert.NotContains(t, strings.ToLower(wire.Message), "retry")
}

// TestStateBackupExampleMatchesSchema: the published example is a valid answer.
func TestStateBackupExampleMatchesSchema(t *testing.T) {
	paths, ok := mapAt(loadSpecTree(t), "paths")
	require.True(t, ok)
	node := paths[backupsPath+"/{id}"]
	for _, key := range []string{"get", "responses", "200", "content", "application/json", "example"} {
		node, ok = mapAt(node, key)
		require.True(t, ok, key)
	}
	data, err := json.Marshal(node.(map[string]any)["data"])
	require.NoError(t, err)
	var example map[string]any
	require.NoError(t, json.Unmarshal(data, &example))
	require.NoError(t, browserSchema(t, "StateBackup").Validate(example))
}

// TestStatePersonAndKeyNeverShareAName: an app key whose ID is a person's ID
// neither replays that person's idempotent answer nor is audited as the
// person, who acts as person:<id>.
func TestStatePersonAndKeyNeverShareAName(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeCoordinator)
	browser, owner := createBrowserOwner(t, s, endpoint)
	byPerson := func() string {
		status, body := browser.call(t, http.MethodPost, backupsPath, map[string]any{}, map[string]string{idempotencyHeader: "same"})
		require.Equal(t, http.StatusCreated, status, string(body))
		return decodeBackup(t, backupAnswer(t, s, status, body)).ID
	}
	person := byPerson()
	require.Equal(t, person, byPerson(), "the person replays its own answer")
	key := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/keys", TestAdminKey, map[string]any{"id": owner.ID, "name": "Twin", "role": "admin"})
	require.Equal(t, http.StatusCreated, key.Code, string(key.Body))
	var raw struct {
		Data CreateKeyResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(key.Body, &raw))
	created := createBackupAs(t, s, raw.Data.RawKey, "same", map[string]any{})
	require.Equal(t, http.StatusCreated, created.Code, string(created.Body))
	twin := decodeBackup(t, backupAnswer(t, s, created.Code, created.Body)).ID
	require.NotEqual(t, person, twin, "the key does not replay the person's answer")
	events, _, err := s.accessStore.AuditPage(t.Context(), audit.Filter{Action: audit.ActionBackupCreated}, "", audit.MaxPage)
	require.NoError(t, err)
	actors := map[string]string{}
	for _, ev := range events {
		actors[ev.Subject] = ev.Actor
	}
	require.Equal(t, map[string]string{person: "person:" + owner.ID, twin: owner.ID}, actors)
}
