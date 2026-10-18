package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A run id no node lists answers a 404 that says what the caller can still
// do, and names the live run that evicted it when one did.
func TestGetRunNotFoundExplainsEndedRun(t *testing.T) {
	gin.SetMode(gin.TestMode)
	live := instance.InstanceInfo{ID: "gemma", Model: "google/gemma-4-E2B-it", Node: "localai", Status: instance.StatusRunning,
		EvictedRuns: []instance.EvictedRun{{RunID: "qwen", Model: "Qwen/Qwen2.5-0.5B-Instruct", FreedMiB: 2048}}}
	internal := gin.New()
	internal.GET("/zzrouter/v1/internal/runs", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"instances": []instance.InstanceInfo{live}})
	})
	internal.GET("/zzrouter/v1/internal/runs/:id", func(c *gin.Context) {
		if c.Param("id") != live.ID {
			NotFound(c, "instance not found")
			return
		}
		c.JSON(http.StatusOK, live)
	})
	public := gin.New()
	ctrl := NewRunsController(NewRunsService(&lifecycleRouter{engine: internal}), nil, nil, nil, nil)
	public.GET("/runs/:id", ctrl.GetRun)
	get := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		public.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+id, nil))
		return rec
	}

	rec := get("qwen")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	var problem struct {
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Equal(t, "run not found: qwen was stopped on node localai to free 2048 MiB of GPU memory for run gemma (gemma-4-E2B-it). A pin without a run remains in zz_runs; use zz_run_start to replay it. Read the ended run's log with zz_logs using run_id qwen and node localai", problem.Detail)

	rec = get("typo")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Equal(t, "run not found: typo. A pin without a run remains in zz_runs; use zz_run_start to replay it. Read the ended run's log with zz_logs using run_id typo and the node it ran on", problem.Detail)

	rec = get("gemma")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var found struct {
		Data instance.InstanceInfo `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &found))
	assert.Equal(t, live, found.Data, "a live run answers unchanged, evicted_runs included")

	live.ID, live.Model, live.Provider = "812d79ffcba5", "", "comfyui"
	live.EvictedRuns[0].FreedMiB = 50994
	rec = get("qwen")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Equal(t, "run not found: qwen was stopped on node localai to free 50994 MiB of GPU memory for run 812d79ffcba5 (comfyui). A pin without a run remains in zz_runs; use zz_run_start to replay it. Read the ended run's log with zz_logs using run_id qwen and node localai", problem.Detail)
}
