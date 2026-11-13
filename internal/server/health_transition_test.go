package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stretchr/testify/require"
)

func TestHealthReadinessDuringRoleTransition(t *testing.T) {
	manager, err := role.NewManager(role.RoleWorker)
	require.NoError(t, err)
	s := &Server{role: manager}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	manager.Subscribe(func(role.Transition) { close(entered); <-release })
	go func() { defer close(done); _ = manager.Set(t.Context(), role.RoleCoordinator, "test") }()
	<-entered
	engine := gin.New()
	handler := NewHealthHandler(nil, s.readinessUnavailable, nil)
	engine.GET("/health/ready", handler.GetHealthReady)
	check := func(code int, status string) {
		t.Helper()
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
		require.Equal(t, code, w.Code)
		var response HealthResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.Equal(t, status, response.Status)
	}
	check(http.StatusServiceUnavailable, "transitioning")
	once.Do(func() { close(release) })
	<-done
	check(http.StatusOK, "ready")
	s.draining.Store(true)
	check(http.StatusServiceUnavailable, "draining")
}
