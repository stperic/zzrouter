package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
)

const statePath = "/zzrouter/v1/system/state"

// readStoreState reads GET /system/state as s's administrator, checking the
// answer against the published schema and that it names no path.
func readStoreState(t *testing.T, s *Server) (storeState, map[string]any) {
	t.Helper()
	result := makeAuthRequest(t, s, http.MethodGet, statePath, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, result.Code, string(result.Body))
	assert.Equal(t, "no-store", result.Headers.Get("Cache-Control"))
	assert.NotContains(t, string(result.Body), s.configDir, "the answer names no path")
	var typed struct {
		Data storeState `json:"data"`
	}
	var raw struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(result.Body, &typed))
	require.NoError(t, json.Unmarshal(result.Body, &raw))
	require.NoError(t, browserSchema(t, "StoreState").Validate(raw.Data), string(result.Body))
	return typed.Data, raw.Data
}

// TestSystemStateReportsTheStore: the status carries the store's meta, its
// counters advance with writes, setup_required turns ready after the owner
// claim, and zz_api and discovery reach it as an ordinary admin read.
func TestSystemStateReportsTheStore(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeCoordinator)
	fresh, raw := readStoreState(t, s)
	meta := s.stateStore.Meta()
	assert.Equal(t, meta.StoreID, fresh.StoreID)
	assert.Equal(t, meta.Generation, fresh.Generation)
	assert.Equal(t, meta.SchemaVersion, fresh.SchemaVersion)
	assert.Equal(t, meta.Revision, fresh.Revision)
	assert.Equal(t, meta.Sequence, fresh.Sequence)
	assert.Equal(t, "setup_required", fresh.Status)
	assert.Equal(t, storeIntegrity{CheckedAt: fresh.Integrity.CheckedAt, Check: "quick_check", Outcome: "ok"}, fresh.Integrity)
	assert.False(t, fresh.Integrity.CheckedAt.IsZero())
	assert.False(t, fresh.ObservedAt.Before(fresh.Integrity.CheckedAt))
	require.NotNil(t, fresh.Size.DatabaseBytes)
	assert.Positive(t, *fresh.Size.DatabaseBytes)
	require.NotNil(t, fresh.Size.JournalBytes)
	assert.Zero(t, *fresh.Size.JournalBytes)
	assert.NotNil(t, fresh.Size.FreePageBytes)
	assert.Equal(t, []any{}, raw["retained_copies"], "an empty list, never null")

	created := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/teams", TestAdminKey, map[string]string{"id": "eng", "name": "Engineering"})
	require.Equal(t, http.StatusCreated, created.Code, string(created.Body))
	written, _ := readStoreState(t, s)
	assert.Equal(t, fresh.Revision+1, written.Revision)
	assert.Equal(t, fresh.Sequence+1, written.Sequence)
	assert.Equal(t, fresh.Integrity, written.Integrity, "the check is the one run at startup")

	createBrowserOwner(t, s, endpoint)
	claimed, _ := readStoreState(t, s)
	assert.Equal(t, "ready", claimed.Status)
	assert.Greater(t, claimed.Revision, written.Revision)

	operation, err := s.eligibleAPIRoute(http.MethodGet, statePath)
	require.NoError(t, err, "zz_api reaches the route")
	assert.Equal(t, "getStoreState", operation)
	var discovered []string
	for _, routes := range collectAPIDiscoveryGroups(s) {
		discovered = append(discovered, routes...)
	}
	assert.True(t, slices.Contains(discovered, http.MethodGet+" "+statePath), "discovery lists the route")
}

// TestSystemStateGating: no key is 401, a user key 403, and a worker answers
// the coordinator-role 421; no answer names a path.
func TestSystemStateGating(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	worker, _ := oauthTestNode(t, config.ClusterModeWorker)
	for _, c := range []struct {
		name string
		node *Server
		key  string
		code int
	}{
		{"no key", s, "", http.StatusUnauthorized},
		{"user key", s, TestUserKey, http.StatusForbidden},
		{"worker", worker, TestAdminKey, http.StatusMisdirectedRequest},
	} {
		result := makeAuthRequest(t, c.node, http.MethodGet, statePath, c.key, nil)
		assert.Equal(t, c.code, result.Code, "%s: %s", c.name, result.Body)
		assert.NotContains(t, string(result.Body), c.node.configDir, c.name)
	}
}

// TestSystemStateOfAFailedStore: once the store cannot vouch for its records,
// the status answers the closed 503 without database text or a path.
func TestSystemStateOfAFailedStore(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	db := liveDatabase(t, s)
	_, err := db.ExecContext(t.Context(), "ALTER TABLE grants RENAME TO hidden")
	require.NoError(t, err)
	release := holdReadLock(t, db)
	failed := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/teams", TestAdminKey, map[string]string{"id": "eng", "name": "Engineering"})
	release()
	require.Equal(t, http.StatusServiceUnavailable, failed.Code, string(failed.Body))
	require.Equal(t, string(httperr.CodeAccessStoreFailed), problemCode(t, failed.Body))

	result := makeAuthRequest(t, s, http.MethodGet, statePath, TestAdminKey, nil)
	require.Equal(t, http.StatusServiceUnavailable, result.Code, string(result.Body))
	assert.Equal(t, string(httperr.CodeAccessStoreFailed), problemCode(t, result.Body))
	assert.NotContains(t, string(result.Body), "SQL", "no database text")
	assert.NotContains(t, string(result.Body), s.configDir)
}

// TestSystemStateExampleMatchesSchema: the published example is a valid answer.
func TestSystemStateExampleMatchesSchema(t *testing.T) {
	paths, ok := mapAt(loadSpecTree(t), "paths")
	require.True(t, ok)
	node := paths[statePath]
	for _, key := range []string{"get", "responses", "200", "content", "application/json", "example"} {
		node, ok = mapAt(node, key)
		require.True(t, ok, key)
	}
	data, err := json.Marshal(node.(map[string]any)["data"])
	require.NoError(t, err)
	var example map[string]any
	require.NoError(t, json.Unmarshal(data, &example))
	require.NoError(t, browserSchema(t, "StoreState").Validate(example))
}
