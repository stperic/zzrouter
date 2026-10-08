package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountModelGroupsTestRouter wires a ModelGroupsController onto a fresh
// gin engine under the public API prefix. The store persists to a
// per-test temp file so the Save() round-trip in each handler succeeds.
func mountModelGroupsTestRouter(t *testing.T, yaml string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	store := modelgroup.NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(yaml)))
	store.SetPath(filepath.Join(t.TempDir(), "model_groups.yaml"))

	ctrl := NewModelGroupsController(store, nil, nil, nil, nil, nil, nil, nil, "/model-groups/")

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
		c.Next()
	})
	api := r.Group("/zzrouter/v1")
	ctrl.RegisterPublicRoutes(api)
	return r
}

const idemTestYAML = `
version: "1"
model_groups:
  fast-chat:
    strategy: priority
    replicas:
      - name: r1
        model: m
        provider: ollama
        priority: 1
      - name: r2
        model: m
        provider: vllm
        priority: 2
`

func TestModelGroups_PATCH_IdempotencyReplay(t *testing.T) {
	r := mountModelGroupsTestRouter(t, idemTestYAML)

	patch := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/fast-chat",
			bytes.NewBufferString(`{"description":"agent-renamed"}`))
		req.Header.Set("Content-Type", "application/merge-patch+json")
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w1 := patch("retry-1")
	require.Equal(t, http.StatusOK, w1.Code, "first patch should succeed: body=%s", w1.Body.String())

	w2 := patch("retry-1")
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, w1.Body.String(), w2.Body.String(),
		"replay with same Idempotency-Key + same body must return the recorded response")
}

func TestModelGroups_PATCH_IdempotencyConflictOnDifferentBody(t *testing.T) {
	r := mountModelGroupsTestRouter(t, idemTestYAML)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/fast-chat", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/merge-patch+json")
		req.Header.Set("Idempotency-Key", "k")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w1 := post(`{"description":"first"}`)
	require.Equal(t, http.StatusOK, w1.Code)
	w2 := post(`{"description":"second"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, w2.Code,
		"different body under the same Idempotency-Key must conflict")
}

func TestModelGroups_DeleteReplica_LastInGroup409(t *testing.T) {
	r := mountModelGroupsTestRouter(t, idemTestYAML)
	// delete r1 — leaves r2
	req := httptest.NewRequest("DELETE", "/zzrouter/v1/model-groups/fast-chat/replicas/r1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "first delete should succeed: %s", w.Body.String())

	// delete r2 — would leave the group empty
	req = httptest.NewRequest("DELETE", "/zzrouter/v1/model-groups/fast-chat/replicas/r2", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusConflict, w.Code,
		"deleting the last replica must 409: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"replica_last_in_group"`)
}

func TestModelGroups_PATCH_ReplicaUnknown404(t *testing.T) {
	r := mountModelGroupsTestRouter(t, idemTestYAML)
	req := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/fast-chat/replicas/nope",
		bytes.NewBufferString(`{"priority":99}`))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), `"replica_unknown"`)
}

func TestModelGroups_PATCH_IdempotencyScopesByResolvedPath(t *testing.T) {
	yaml := `
version: "1"
model_groups:
  fast-chat:
    replicas: [{name: r1, model: m, provider: ollama, priority: 1}]
  code-gen:
    replicas: [{name: r1, model: m, provider: ollama, priority: 1}]
`
	r := mountModelGroupsTestRouter(t, yaml)
	patch := func(group string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/"+group,
			bytes.NewBufferString(`{"description":"renamed"}`))
		req.Header.Set("Content-Type", "application/merge-patch+json")
		req.Header.Set("Idempotency-Key", "shared")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	wa := patch("fast-chat")
	wb := patch("code-gen")
	require.Equal(t, http.StatusOK, wa.Code)
	require.Equal(t, http.StatusOK, wb.Code,
		"same Idempotency-Key on a different resource path must not collide: %s", wb.Body.String())
	assert.Contains(t, wa.Body.String(), `"name":"fast-chat"`)
	assert.Contains(t, wb.Body.String(), `"name":"code-gen"`,
		"each PATCH must produce its own response, not replay the other")
}

func TestModelGroups_PATCH_GroupUnknown404(t *testing.T) {
	r := mountModelGroupsTestRouter(t, idemTestYAML)
	req := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/missing",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), `"route_unknown"`)
}
