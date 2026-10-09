package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRunRestartIdempotencyReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	internal := gin.New()
	calls := 0
	internal.POST("/zzrouter/v1/internal/runs/:id/restart", func(c *gin.Context) {
		calls++
		c.JSON(http.StatusAccepted, RestartRunResponse{
			Accepted: true, OldID: c.Param("id"), ID: fmt.Sprintf("run-%d", calls),
			JobID: fmt.Sprintf("job-%d", calls), Status: "starting",
		})
	})
	ctrl := NewRunsController(NewRunsService(&lifecycleRouter{engine: internal}), nil, nil, nil, nil)
	s := &Server{idempotency: newIdempotencyStore()}
	public := gin.New()
	public.Use(func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "agent"}})
	})
	ctrl.RegisterPublicRoutes(public.Group("/zzrouter/v1"), s.idempotent)
	restart := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/runs/old/restart?node=worker", nil)
		request.Header.Set("Idempotency-Key", "restart-retry")
		recorder := httptest.NewRecorder()
		public.ServeHTTP(recorder, request)
		return recorder
	}
	first, replay := restart(), restart()
	require.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
	require.Equal(t, first.Code, replay.Code)
	require.Equal(t, first.Body.String(), replay.Body.String())
	var response struct {
		Data RestartRunResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(replay.Body.Bytes(), &response))
	require.Equal(t, "job-1", response.Data.JobID)
	require.Equal(t, "run-1", response.Data.ID)
	require.Equal(t, 1, calls, "restart handler must create only one job")
}
