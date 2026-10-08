package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { gin.SetMode(gin.TestMode) }

// newGateTestEngine returns a gin engine with the given ClusterModeGate
// mounted on GET /probe. No responder is attached, so the gate falls
// back to a plain 503.
func newGateTestEngine(rm *role.Manager, allowed ...role.Role) *gin.Engine {
	e := gin.New()
	e.GET("/probe", ClusterModeGate(rm, allowed...), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	return e
}

func TestClusterModeGate_Allowed_PassesThrough(t *testing.T) {
	t.Parallel()
	rm, _ := role.NewManager(role.RoleCoordinator)
	e := newGateTestEngine(rm, role.RoleCoordinator)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", w.Body.String())
}

func TestClusterModeGate_Blocked_Returns503(t *testing.T) {
	t.Parallel()
	rm, _ := role.NewManager(role.RoleUnclaimed)
	e := newGateTestEngine(rm, role.RoleCoordinator)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestClusterModeGate_RoleFlip_ChangesGateLive(t *testing.T) {
	t.Parallel()
	rm, _ := role.NewManager(role.RoleUnclaimed)
	e := newGateTestEngine(rm, role.RoleCoordinator)

	// Blocked initially.
	w1 := httptest.NewRecorder()
	e.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/probe", nil))
	require.Equal(t, http.StatusServiceUnavailable, w1.Code)

	// Promote and re-request on the same engine — no rebuild.
	require.NoError(t, rm.Set(context.Background(), role.RoleCoordinator, "test promote"))

	w2 := httptest.NewRecorder()
	e.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/probe", nil))
	assert.Equal(t, http.StatusOK, w2.Code)

	// Demote and re-request — should block again.
	require.NoError(t, rm.Set(context.Background(), role.RoleDisabled, "test demote"))

	w3 := httptest.NewRecorder()
	e.ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/probe", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w3.Code)
}

func TestClusterModeGate_MultipleAllowed(t *testing.T) {
	t.Parallel()
	rm, _ := role.NewManager(role.RoleWorker)
	e := newGateTestEngine(rm, role.RoleWorker, role.RoleCoordinator)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestClusterModeGate_PanicsOnNilManager(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() {
		ClusterModeGate(nil, role.RoleCoordinator)
	})
}

func TestClusterModeGate_PanicsOnEmptyAllowed(t *testing.T) {
	t.Parallel()
	rm, _ := role.NewManager(role.RoleDisabled)
	assert.Panics(t, func() {
		ClusterModeGate(rm)
	})
}

func TestClusterModeGate_WithResponder_UsesDialect(t *testing.T) {
	t.Parallel()
	rm, _ := role.NewManager(role.RoleUnclaimed)

	responders := newResponderSet()
	e := gin.New()
	e.GET("/probe",
		httperr.AttachResponder(responders.problem),
		ClusterModeGate(rm, role.RoleCoordinator),
		func(c *gin.Context) { c.String(http.StatusOK, "ok") },
	)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	// Problem Details responder is the fallback for unregistered paths.
	assert.Contains(t, w.Header().Get("Content-Type"), "application/problem+json")
}
