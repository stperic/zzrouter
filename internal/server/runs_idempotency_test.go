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

func TestRunStartIdempotencyHashesReplace(t *testing.T) {
	worker := gin.New()
	calls := 0
	worker.POST("/zzrouter/v1/internal/runs", func(c *gin.Context) {
		calls++
		var req LaunchRunRequest
		require.True(t, BindJSONStrict(c, &req))
		require.Equal(t, map[string]string{"DEVICE": "0"}, req.Environment)
		if calls == 1 {
			RespondToError(c, &prov_apps.RunSettingsDifferError{RunID: "old"})
			return
		}
		require.True(t, req.Replace)
		c.JSON(http.StatusAccepted, LaunchRunResponse{Accepted: true, ID: "new", JobID: "job", Node: "worker"})
	})
	ctrl := NewRunsController(NewRunsService(&lifecycleRouter{engine: worker}), nil, nil, nil, nil)
	s := &Server{idempotency: newIdempotencyStore()}
	public := gin.New()
	public.Use(func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "agent"}})
	})
	ctrl.RegisterPublicRoutes(public.Group("/zzrouter/v1"), s.idempotent)
	post := func(replace bool) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"provider":"studio","model":"m","node":"worker-alias","environment":{"DEVICE":"0"},"replace":%t}`, replace)
		req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/runs", bytes.NewBufferString(body))
		req.Header.Set(idempotencyHeader, "start-key")
		w := httptest.NewRecorder()
		public.ServeHTTP(w, req)
		return w
	}
	refusal := post(false)
	require.Equal(t, http.StatusConflict, refusal.Code, refusal.Body.String())
	require.Contains(t, refusal.Body.String(), `"run_id":"old"`)
	accepted := post(true)
	require.Equal(t, http.StatusAccepted, accepted.Code, accepted.Body.String())
	require.Contains(t, accepted.Body.String(), `"node":"worker"`)
	require.Equal(t, accepted.Body.String(), post(true).Body.String())
	w := post(false)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"code":"idempotency_key_conflict"`)
	require.Equal(t, 2, calls)
}

func TestRunWarmReuseIdempotencyRetention(t *testing.T) {
	fc := clocktest.NewFakeClock(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	registry, err := jobs.NewRegistry(jobs.Config{NodeName: "worker", Clock: fc})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	mgr, cfg := nodeRunFixture(t, prov_apps.WithJobsRegistry(registry))
	executor := NewRunsExecutor(mgr, func() string { return "worker" }, func() *config.AppsConfig { return cfg }, nil, nil, nil)
	worker := gin.New()
	worker.POST("/zzrouter/v1/internal/runs", executor.HandleInternalLaunchRun)
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
		req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/runs", bytes.NewBufferString(`{"provider":"studio","launch_mode":"native"}`))
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
}
