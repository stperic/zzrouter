package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mountModelGroupsWithLogStore(t *testing.T, yaml string) (*gin.Engine, *inferencelog.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	store := modelgroup.NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(yaml)))
	store.SetPath(filepath.Join(t.TempDir(), "model_groups.yaml"))

	logStore := inferencelog.NewStore(100, 0)
	t.Cleanup(logStore.Stop)

	ctrl := NewModelGroupsController(store, nil, nil, nil, nil, logStore, nil, nil, "/model-groups/")

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
		c.Next()
	})
	api := r.Group("/zzrouter/v1")
	ctrl.RegisterPublicRoutes(api)
	return r, logStore
}

func TestGetModelGroupHistory_FiltersOnGroupName(t *testing.T) {
	r, logStore := mountModelGroupsWithLogStore(t, idemTestYAML)
	now := time.Now()
	logStore.Add(inferencelog.LogEntry{ID: "a", Timestamp: now.Add(-2 * time.Minute), GroupName: "fast-chat", Model: "m", Status: "success", FallbackCount: 0}, nil)
	logStore.Add(inferencelog.LogEntry{ID: "b", Timestamp: now.Add(-1 * time.Minute), GroupName: "other-group", Model: "m", Status: "success"}, nil)
	logStore.Add(inferencelog.LogEntry{ID: "c", Timestamp: now, GroupName: "fast-chat", Model: "m", Status: "error", FallbackCount: 2, FallbackFrom: "r1"}, nil)

	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/fast-chat/history", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var envelope struct {
		Data modelGroupHistoryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	assert.Equal(t, "fast-chat", envelope.Data.Name)
	assert.Equal(t, 2, envelope.Data.Count, "must include FallbackCount=0 rows; only the other-group entry filtered out")
	// Newest-first ordering.
	assert.Equal(t, "c", envelope.Data.Entries[0].ID)
	assert.Equal(t, 2, envelope.Data.Entries[0].FallbackCount)
	assert.Equal(t, "r1", envelope.Data.Entries[0].FallbackFrom)
}

func TestGetModelGroupHistory_SinceFilter(t *testing.T) {
	r, logStore := mountModelGroupsWithLogStore(t, idemTestYAML)
	now := time.Now()
	logStore.Add(inferencelog.LogEntry{ID: "old", Timestamp: now.Add(-1 * time.Hour), GroupName: "fast-chat", Status: "success"}, nil)
	logStore.Add(inferencelog.LogEntry{ID: "new", Timestamp: now, GroupName: "fast-chat", Status: "success"}, nil)

	since := now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/fast-chat/history?since="+since, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var envelope struct {
		Data modelGroupHistoryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Entries, 1)
	assert.Equal(t, "new", envelope.Data.Entries[0].ID)
}

func TestGetModelGroupHistory_LimitParam(t *testing.T) {
	r, logStore := mountModelGroupsWithLogStore(t, idemTestYAML)
	for i := 0; i < 10; i++ {
		logStore.Add(inferencelog.LogEntry{
			ID: string(rune('a' + i)), Timestamp: time.Now(),
			GroupName: "fast-chat", Status: "success",
		}, nil)
	}
	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/fast-chat/history?limit=3", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var envelope struct {
		Data modelGroupHistoryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	assert.Len(t, envelope.Data.Entries, 3)
}

func TestGetModelGroupHistory_InvalidSinceParam(t *testing.T) {
	r, _ := mountModelGroupsWithLogStore(t, idemTestYAML)
	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/fast-chat/history?since=bogus", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetModelGroupHistory_UnknownGroup404(t *testing.T) {
	r, _ := mountModelGroupsWithLogStore(t, idemTestYAML)
	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/nope/history", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetModelGroupHistory_NoLogStore503(t *testing.T) {
	// Mount without log store.
	r := mountModelGroupsTestRouter(t, idemTestYAML)
	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/fast-chat/history", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"history degrades to 503 when the node has no log store wired")
}
