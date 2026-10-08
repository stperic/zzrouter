package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/fallback"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountModelGroupsRouterWithTrackers wires a ModelGroupsController with
// real load + latency + health trackers so the /state endpoint can
// reflect actual snapshot values.
func mountModelGroupsRouterWithTrackers(t *testing.T, yaml string) (*gin.Engine, *fallback.CooldownManager, *fallback.LoadTracker, *fallback.LatencyTracker, *fallback.HealthChecker) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	store := modelgroup.NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(yaml)))
	store.SetPath(filepath.Join(t.TempDir(), "model_groups.yaml"))

	cooldowns := fallback.NewCooldownManager()
	t.Cleanup(cooldowns.Stop)
	load := fallback.NewLoadTracker()
	latency := fallback.NewLatencyTracker(10)
	health := fallback.NewHealthChecker([]fallback.HealthTarget{
		{DeploymentName: "r1", URL: "http://stub", Path: "/health"},
	})

	ctrl := NewModelGroupsController(store, cooldowns, load, latency, health, nil, nil, nil, "/model-groups/")

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
		c.Next()
	})
	api := r.Group("/zzrouter/v1")
	ctrl.RegisterPublicRoutes(api)
	return r, cooldowns, load, latency, health
}

func TestGetModelGroupState_GroupNotFound(t *testing.T) {
	r, _, _, _, _ := mountModelGroupsRouterWithTrackers(t, idemTestYAML)
	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/nope/state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetModelGroupState_LiveFieldsReflectTrackers(t *testing.T) {
	r, cooldowns, load, latency, _ := mountModelGroupsRouterWithTrackers(t, idemTestYAML)

	// Seed live state.
	cooldowns.SetCooldown("r1", 30*time.Second, fallback.ReasonRateLimit)
	load.Acquire("r1")
	load.Acquire("r1")
	latency.Record("r2", 120*time.Millisecond)
	latency.Record("r2", 80*time.Millisecond)

	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/fast-chat/state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	var envelope struct {
		Data modelGroupStateResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	state := envelope.Data
	assert.Equal(t, "fast-chat", state.Name)
	require.Len(t, state.Replicas, 2)

	byName := map[string]replicaState{}
	for _, r := range state.Replicas {
		byName[r.Name] = r
	}

	r1 := byName["r1"]
	assert.Equal(t, "rate_limit", r1.CooldownReason)
	assert.InDelta(t, 30.0, r1.CooldownRemainingSeconds, 1.0,
		"cooldown should report ~30s remaining (with timing jitter tolerance)")
	assert.EqualValues(t, 2, r1.InFlight)
	assert.Equal(t, "healthy", r1.HealthState, "r1 has a HealthTarget so health_state is reported")

	r2 := byName["r2"]
	assert.EqualValues(t, 100, r2.LatencyAvgMs, "average of 120ms + 80ms")
	assert.Equal(t, 2, r2.LatencySamples)
	assert.Empty(t, r2.CooldownReason, "no cooldown set on r2")
	assert.Equal(t, "unknown", r2.HealthState, "r2 has no HealthTarget configured")
}

func TestGetModelGroupState_NilTrackersOmitFields(t *testing.T) {
	// Mount without trackers — the minimal-router test helper from
	// idempotency tests uses this shape; verify /state degrades clean.
	r := mountModelGroupsTestRouter(t, idemTestYAML)
	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/fast-chat/state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	// Body should have a `replicas` array with names but no live fields.
	assert.Contains(t, w.Body.String(), `"name":"r1"`)
	assert.NotContains(t, w.Body.String(), `"latency_avg_ms"`)
	assert.NotContains(t, w.Body.String(), `"health_state"`)
}
