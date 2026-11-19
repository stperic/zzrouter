package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/recovery"
	"github.com/stperic/zzrouter/pkg/utils"
)

// exportRequest asks for a backup's content as the local operator.
func exportRequest(t *testing.T, s *Server, id string) *http.Request {
	t.Helper()
	return localGateRequest(http.MethodGet, backupsPath+"/"+id+"/content", readLocalProof(t, s).Proof, "")
}

func exportRows(t *testing.T, s *Server) []audit.Event {
	t.Helper()
	events, _, err := s.accessStore.AuditPage(t.Context(), audit.Filter{Action: audit.ActionBackupExported}, "", audit.MaxPage)
	require.NoError(t, err)
	return events
}

func createdBackup(t *testing.T, s *Server) recovery.OnlineBackup {
	t.Helper()
	created := createBackupAs(t, s, TestAdminKey, "", map[string]any{})
	require.Equal(t, http.StatusCreated, created.Code, string(created.Body))
	return decodeBackup(t, backupAnswer(t, s, created.Code, created.Body))
}

// TestStateBackupExport: the local operator receives the archive as it is on
// disk, whole even when asking for a Range, with its length, digest and an
// attachment name of the node's own; the copy passes recovery check, and each
// export records exactly one row as the local operator. An unknown or expired
// backup is 404.
func TestStateBackupExport(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	backup := createdBackup(t, s)
	onDisk, err := os.ReadFile(filepath.Join(s.configDir, "access", "backups", backup.ID+".tar.gz"))
	require.NoError(t, err)
	digest := sha256.Sum256(onDisk)
	for _, ranged := range []bool{false, true} {
		r := exportRequest(t, s, backup.ID)
		if ranged {
			r.Header.Set("Range", "bytes=0-9")
		}
		w := serveLocalGate(s, r)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, onDisk, w.Body.Bytes(), "the whole archive, Range %v", ranged)
		assert.Equal(t, map[string]string{
			"Content-Type":           "application/gzip",
			"Content-Disposition":    `attachment; filename="zzrouter-access-` + backup.CreatedAt.UTC().Format("20060102T150405Z") + "-" + backup.ID[:8] + `.tar.gz"`,
			"Content-Length":         strconv.Itoa(len(onDisk)),
			"Repr-Digest":            "sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":",
			"Accept-Ranges":          "none",
			"Cache-Control":          "no-store",
			"X-Content-Type-Options": "nosniff",
		}, map[string]string{
			"Content-Type": w.Header().Get("Content-Type"), "Content-Disposition": w.Header().Get("Content-Disposition"),
			"Content-Length": w.Header().Get("Content-Length"), "Repr-Digest": w.Header().Get("Repr-Digest"),
			"Accept-Ranges": w.Header().Get("Accept-Ranges"), "Cache-Control": w.Header().Get("Cache-Control"),
			"X-Content-Type-Options": w.Header().Get("X-Content-Type-Options"),
		})
		copied := filepath.Join(t.TempDir(), "copy.tar.gz")
		require.NoError(t, os.WriteFile(copied, w.Body.Bytes(), 0o600))
		manifest, err := recovery.Check(t.Context(), copied, "")
		require.NoError(t, err)
		require.Equal(t, *backup.StoreMeta, *manifest.Store)
	}
	rows := exportRows(t, s)
	require.Len(t, rows, 2, "one row per export")
	for _, row := range rows {
		assert.Equal(t, [3]any{backup.ID, audit.ActorLocalOperator, backup.SHA256}, [3]any{row.Subject, row.Actor, row.Metadata["sha256"]})
	}

	missing := serveLocalGate(s, exportRequest(t, s, "0f6b5f7e-3f0e-4d56-9a43-2f4f5b0f6c11"))
	require.Equal(t, http.StatusNotFound, missing.Code)
	assert.Equal(t, string(httperr.CodeStateBackupNotFound), problemCode(t, missing.Body.Bytes()))
	restore := utils.SetClock(utils.FixedClock(utils.NowUTC().Add(8 * 24 * time.Hour)))
	expired := serveLocalGate(s, exportRequest(t, s, backup.ID))
	restore()
	require.Equal(t, http.StatusNotFound, expired.Code, expired.Body.String())
	require.Len(t, exportRows(t, s), 2)
}

// TestStateBackupExportGate: the content answers the local proof only. An
// administrator key alone is refused, as is a valid proof from another peer,
// with an Origin, a cookie or a forwarding header, each with the gate's one
// answer; a worker answers 421. Neither MCP server can reach the route.
func TestStateBackupExportGate(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	backup := createdBackup(t, s)
	type refusal struct{ Type, Title, Detail, Code string }
	answer := func(body []byte) (got refusal) {
		require.NoError(t, json.Unmarshal(body, &got))
		return got
	}
	admin := makeAuthRequest(t, s, http.MethodGet, backupsPath+"/"+backup.ID+"/content", TestAdminKey, nil)
	require.Equal(t, http.StatusForbidden, admin.Code, string(admin.Body))
	for name, mutate := range map[string]func(*http.Request){
		"another peer":    func(r *http.Request) { r.RemoteAddr = "203.0.113.20:50123" },
		"origin":          func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:9090") },
		"cookie":          func(r *http.Request) { r.Header.Set("Cookie", "unrelated=1") },
		"x-forwarded-for": func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.9") },
	} {
		r := exportRequest(t, s, backup.ID)
		mutate(r)
		w := serveLocalGate(s, r)
		require.Equal(t, http.StatusForbidden, w.Code, name)
		require.Equal(t, answer(admin.Body), answer(w.Body.Bytes()), "%s: the gate's one answer", name)
	}
	require.Empty(t, exportRows(t, s))
	worker, _ := oauthTestNode(t, config.ClusterModeWorker)
	w := serveLocalGate(worker, localGateRequest(http.MethodGet, backupsPath+"/"+backup.ID+"/content", "proof", ""))
	require.Equal(t, http.StatusMisdirectedRequest, w.Code)

	assert.Equal(t, authLocal, s.authTier(http.MethodGet, backupsPath+"/:id/content"))
	_, err := s.eligibleAPIRoute(http.MethodGet, backupsPath+"/"+backup.ID+"/content")
	require.Error(t, err, "zz_api cannot reach it")
	r := mcpWireRequest(t, "tools/list", "2026-07-28", nil)
	r.URL.Path = "/inference/mcp"
	r.Header.Set("X-API-Key", TestUserKey)
	for _, tool := range mcpWireResponse(t, s, r, http.StatusOK)["result"].(map[string]any)["tools"].([]any) {
		properties, _ := tool.(map[string]any)["inputSchema"].(map[string]any)["properties"].(map[string]any)
		require.NotContains(t, properties, "path", "no inference tool takes a path")
	}
}

// TestStateBackupExportFailures: an archive that fails its check answers 500
// and a failing audit row 503, both before any archive byte and with no row;
// the row is committed before the first byte.
func TestStateBackupExportFailures(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	h := hookBackups(t, s)
	backup := createdBackup(t, s)
	w := httptest.NewRecorder()
	h.record = func(ctx context.Context, ev audit.Event) error {
		require.Zero(t, w.Body.Len(), "no byte before the row")
		require.Empty(t, w.Header().Get("Content-Type"))
		return h.Store.Record(ctx, ev)
	}
	s.engine.ServeHTTP(w, exportRequest(t, s, backup.ID))
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, exportRows(t, s), 1)

	h.record = func(context.Context, audit.Event) error { return sqlstore.ErrBusy }
	busy := serveLocalGate(s, exportRequest(t, s, backup.ID))
	require.Equal(t, http.StatusServiceUnavailable, busy.Code)
	assert.Equal(t, string(httperr.CodeAccessStoreBusy), problemCode(t, busy.Body.Bytes()), "no archive byte")
	h.record = nil

	path := filepath.Join(s.configDir, "access", "backups", backup.ID+".tar.gz")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)/2] ^= 0xff
	require.NoError(t, os.WriteFile(path, data, 0o600))
	damaged := serveLocalGate(s, exportRequest(t, s, backup.ID))
	require.Equal(t, http.StatusInternalServerError, damaged.Code)
	assert.Equal(t, string(httperr.CodeStateImageCheckFailed), problemCode(t, damaged.Body.Bytes()), "no archive byte")
	assert.Contains(t, damaged.Body.String(), "Take a new backup and use that one", "the next step")
	assert.NotContains(t, damaged.Body.String(), "recovery inventory", "the live database is not implicated")
	require.Len(t, exportRows(t, s), 1, "a refused export records nothing")
}
