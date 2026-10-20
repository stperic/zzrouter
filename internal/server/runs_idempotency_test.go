package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRunRestartIdempotencyReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fc := clocktest.NewFakeClock(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	registry, err := jobs.NewRegistry(jobs.Config{NodeName: "worker", Clock: fc})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	arrivedAt := fc.Now()
	internal := gin.New()
	calls := 0
	internal.POST("/zzrouter/v1/internal/runs/:id/restart", func(c *gin.Context) {
		calls++
		h, startErr := registry.StartDetached(jobs.KindRun, "", nil)
		require.NoError(t, startErr)
		h.Done()
		fc.Advance(time.Hour) // completion precedes capture of the response naming the job
		c.JSON(http.StatusAccepted, RestartRunResponse{
			Accepted: true, OldID: c.Param("id"), ID: fmt.Sprintf("run-%d", calls),
			JobID: h.ID(), Status: "starting",
		})
	})
	ctrl := NewRunsController(NewRunsService(&lifecycleRouter{engine: internal}), nil, nil, nil, nil)
	s := &Server{idempotency: newIdempotencyStore()}
	s.idempotency.now = fc.Now
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
	first := restart()
	fc.Advance(arrivedAt.Add(constants.IdempotencyReplayWindow - time.Nanosecond).Sub(fc.Now()))
	replay := restart()
	require.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
	require.Equal(t, first.Code, replay.Code)
	require.Equal(t, first.Body.String(), replay.Body.String())
	var response struct {
		Data RestartRunResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(replay.Body.Bytes(), &response))
	_, err = registry.Get(response.Data.JobID)
	require.NoError(t, err, "a replay near expiry must still name a readable job")
	require.Equal(t, "run-1", response.Data.ID)
	require.Equal(t, 1, calls, "restart handler must create only one job")
	fc.Advance(time.Nanosecond)
	require.Equal(t, http.StatusAccepted, restart().Code)
	require.Equal(t, 2, calls, "capture delay must not extend replay beyond request arrival plus the window")
}

func TestRunWarmReuseIdempotencyRetention(t *testing.T) {
	for _, path := range []string{"/runs", "/runs/load"} {
		t.Run(path, func(t *testing.T) {
			fc := clocktest.NewFakeClock(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
			registry, err := jobs.NewRegistry(jobs.Config{NodeName: "worker", Clock: fc})
			require.NoError(t, err)
			t.Cleanup(registry.Stop)
			mgr, cfg := nodeRunFixture(t, prov_apps.WithJobsRegistry(registry))
			executor := NewRunsExecutor(mgr, func() string { return "worker" }, func() *config.AppsConfig { return cfg }, nil, nil, nil)
			worker := gin.New()
			worker.POST("/zzrouter/v1/internal/runs", executor.HandleInternalLaunchRun)
			worker.POST("/zzrouter/v1/internal/runs/load", func(c *gin.Context) {
				response, loadErr := NewLoadExecutor(mgr, emptyModelStore{}, func() string { return "worker" }).LoadLocalModel(c.Request.Context(), &LoadModelRequest{Provider: "studio"})
				require.NoError(t, loadErr)
				c.JSON(http.StatusAccepted, response)
			})
			result, err := mgr.EnsureRun(context.Background(), prov_apps.EnsureRequest{Provider: "studio"})
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				event, getErr := registry.Get(result.Run.StreamJobID)
				return getErr == nil && event.Phase.IsTerminal()
			}, 10*time.Second, time.Millisecond)
			fc.Advance(23 * time.Hour)
			store := newIdempotencyStore()
			store.now = fc.Now
			public := makeIdemStoreRouter(store, func(c *gin.Context) {
				req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal"+path, bytes.NewBufferString(`{"provider":"studio","launch_mode":"native"}`))
				recorder := httptest.NewRecorder()
				worker.ServeHTTP(recorder, req)
				c.Data(recorder.Code, "application/json", recorder.Body.Bytes())
			})
			first := doPOST(public, "{}", map[string]string{idempotencyHeader: "fresh-key"})
			require.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
			var accepted struct {
				JobID string `json:"job_id"`
			}
			require.NoError(t, json.Unmarshal(first.Body.Bytes(), &accepted))
			require.Equal(t, result.Run.StreamJobID, accepted.JobID)
			fc.Advance(constants.IdempotencyReplayWindow - time.Nanosecond)
			replay := doPOST(public, "{}", map[string]string{idempotencyHeader: "fresh-key"})
			require.Equal(t, first.Body.String(), replay.Body.String())
			_, err = registry.Get(accepted.JobID)
			require.NoError(t, err, "warm reuse must extend retention through the fresh key's replay window")
		})
	}
}
