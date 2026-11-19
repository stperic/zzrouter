package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
)

// liveDatabase opens s's access database as another tool would, on its own connection.
func liveDatabase(t *testing.T, s *Server) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(s.configDir, "access", "access.sqlite")))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// holdReadLock keeps a read transaction open on db until release, so a COMMIT
// waits for it past the store's busy timeout.
func holdReadLock(t *testing.T, db *sql.DB) (release func()) {
	t.Helper()
	reader, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var keys int
	require.NoError(t, reader.QueryRowContext(t.Context(), "SELECT count(*) FROM keys").Scan(&keys))
	return func() { require.NoError(t, reader.Rollback()) }
}

// problemCode returns the closed code of a Problem Details body.
func problemCode(t *testing.T, body []byte) string {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(body, &problem), string(body))
	return problem.Code
}

// TestAccessStoreFailureEndsStart: once the store can no longer vouch for its
// records, the write that found out still answers its 503 over the node's own
// listener, and Start returns through the node's one fatal path after a
// graceful drain. Another tool on the live file causes it: a read lock stalls
// the COMMIT past the busy timeout, and a renamed table fails the read that
// would settle whether the COMMIT happened.
func TestAccessStoreFailureEndsStart(t *testing.T) {
	_, port, err := net.SplitHostPort(browserBindAddress(t))
	require.NoError(t, err)
	cfg := TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: pkgConfig.ClusterModeDisabled}
	cfg.Port, err = strconv.Atoi(port)
	require.NoError(t, err)
	s := createTestNode(t, cfg)
	started := make(chan error, 1)
	go func() { started <- s.Start(context.Background()) }()
	select {
	case <-s.listenerReady:
	case err := <-started:
		t.Fatalf("Start returned before serving: %v", err)
	}

	db := liveDatabase(t, s)
	_, err = db.ExecContext(t.Context(), "ALTER TABLE grants RENAME TO hidden")
	require.NoError(t, err)
	release := holdReadLock(t, db)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+s.httpServer.Addr+"/zzrouter/v1/teams",
		bytes.NewBufferString(`{"id":"eng","name":"Engineering"}`))
	require.NoError(t, err)
	request.Header.Set("X-API-Key", TestAdminKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	release()
	require.NoError(t, err, "the failing request gets its answer before the node stops")
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, string(body))
	require.Equal(t, string(httperr.CodeAccessStoreFailed), problemCode(t, body))
	require.NotContains(t, string(body), "SQL", "the answer carries no database text")

	select {
	case err := <-started:
		require.ErrorIs(t, err, sqlstore.ErrFailed)
	case <-time.After(30 * time.Second):
		t.Fatal("a store that failed after boot must end the node")
	}
}

// TestAccessStoreBusyAnswers503: while another tool holds the live file past
// the busy timeout, team and people writes, and audit reads under a write
// lock, answer 503 with a closed code and Retry-After, change nothing, and
// work again once the lock is gone.
func TestAccessStoreBusyAnswers503(t *testing.T) {
	s, _ := oauthTestNode(t, pkgConfig.ClusterModeCoordinator)
	writes := []struct{ name, path string }{
		{"team", "/zzrouter/v1/teams"},
		{"people", "/zzrouter/v1/access/console-grants"},
	}
	body := func(path string) any {
		if path == "/zzrouter/v1/teams" {
			return map[string]string{"id": "eng", "name": "Engineering"}
		}
		return struct{}{}
	}
	release := holdReadLock(t, liveDatabase(t, s))
	before := s.accessStore.Snapshot()
	for _, write := range writes {
		result := makeAuthRequest(t, s, http.MethodPost, write.path, TestAdminKey, body(write.path))
		require.Equal(t, http.StatusServiceUnavailable, result.Code, "%s: %s", write.name, result.Body)
		require.Equal(t, string(httperr.CodeAccessStoreBusy), problemCode(t, result.Body), write.name)
		require.NotEmpty(t, result.Headers.Get("Retry-After"), write.name)
		require.NotContains(t, string(result.Body), "SQL", "%s: the answer carries no database text", write.name)
	}
	require.Same(t, before, s.accessStore.Snapshot(), "a refused write publishes nothing")
	release()
	for _, write := range writes {
		result := makeAuthRequest(t, s, http.MethodPost, write.path, TestAdminKey, body(write.path))
		require.Equal(t, http.StatusCreated, result.Code, "%s: %s", write.name, result.Body)
	}
	held := liveDatabase(t, s)
	_, err := held.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	result := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/access/audit", TestAdminKey, nil)
	_, err = held.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, result.Code, "audit: %s", result.Body)
	require.Equal(t, string(httperr.CodeAccessStoreBusy), problemCode(t, result.Body), "audit")
	require.NotEmpty(t, result.Headers.Get("Retry-After"), "audit")
	require.Equal(t, http.StatusOK, makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/access/audit", TestAdminKey, nil).Code)
	select {
	case err := <-s.servingErrors:
		t.Fatalf("a busy store must not end the node: %v", err)
	default:
	}
}
