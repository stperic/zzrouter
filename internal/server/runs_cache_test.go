package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunCacheSurvivesRESTAndMCP(t *testing.T) {
	for _, location := range []string{"local", "worker"} {
		for _, cacheJSON := range []string{
			`{"kv_tokens":814421,"context_tokens":32768,"max_concurrency":24.85}`,
			`{"kv_tokens":32768,"kv_pools":1,"sequence_slots":4,"context_tokens":32768,"sharing":"shared"}`,
			`{"kv_tokens":32768,"kv_pools":4,"sequence_slots":4,"context_tokens":32768,"sharing":"partitioned"}`,
			`null`,
		} {
			t.Run(location+"/"+cacheJSON, func(t *testing.T) {
				s := createTestNodeWithDefaults(t)
				var want any
				require.NoError(t, json.Unmarshal([]byte(cacheJSON), &want))
				run := instance.NewInstance("fixture", "llamacpp", "model", 8080, 0, 0)
				loaded := instance.Loaded{ContextLength: 32768, Size: 100, VRAM: 100}
				if want != nil {
					require.NoError(t, json.Unmarshal([]byte(cacheJSON), &loaded.Cache))
				}
				run.SetLoaded(loaded)
				run.MarkRunning(false, nil)
				require.NoError(t, s.providers.appMgr.Instances().Register(run))
				for _, single := range []bool{false, true} {
					var expected map[string]any
					path := "/zzrouter/v1/runs"
					args := map[string]any{}
					if single {
						path += "/fixture"
						args["run_id"] = "fixture"
					}
					if location == "worker" {
						// A worker reply without cache (an engine that reported none) decodes
						// through the same typed path as one with observations.
						wire := `{"id":"fixture","provider":"llamacpp","model":"model","node":"worker","status":"running","port":8080,"health_url":"","context_length":32768,"processor":"100% GPU"}`
						var row map[string]any
						require.NoError(t, json.Unmarshal([]byte(wire), &row))
						if want != nil {
							row["cache"] = want
						}
						expected = row
						var payload any = row
						if !single {
							payload = map[string]any{"instances": []any{row}}
						}
						body, err := json.Marshal(payload)
						require.NoError(t, err)
						s.services.Runs.router = &mockRouter{response: &routing.Response{StatusCode: http.StatusOK, Body: body}}
					}
					started := utils.Now()
					rest := makeAuthRequest(t, s, http.MethodGet, path, TestAdminKey, nil)
					require.Equal(t, http.StatusOK, rest.Code, string(rest.Body))
					var body map[string]any
					require.NoError(t, json.Unmarshal(rest.Body, &body))
					var row map[string]any
					if single {
						row = body["data"].(map[string]any)
					} else {
						rows := body["data"].([]any)
						require.Len(t, rows, 1)
						row = rows[0].(map[string]any)
					}
					if expected != nil {
						assert.Equal(t, expected, row)
					}
					assert.NotContains(t, row, "requests_admitted")
					assert.Equal(t, want, row["cache"])
					if want == nil {
						assert.NotContains(t, row, "cache")
					}
					for key, value := range map[string]any{"id": "fixture", "provider": "llamacpp", "model": "model", "status": "running", "context_length": float64(32768), "processor": "100% GPU", "port": float64(8080)} {
						assert.Equal(t, value, row[key], key)
					}
					result := callMCPWireTool(t, s, "zz_runs", args)
					require.NotEqual(t, true, result["isError"], result)
					fields := result["structuredContent"].(map[string]any)
					if !single {
						rows := fields["runs"].([]any)
						require.Len(t, rows, 1)
						fields = rows[0].(map[string]any)
					}
					assert.Equal(t, want, fields["cache"])
					if want == nil {
						assert.NotContains(t, fields, "cache")
					}
					if location == "local" {
						finished := utils.Now()
						assertRunCacheActivity(t, row["activity"], started, finished)
						assertRunCacheActivity(t, fields["activity"], started, finished)
					}
					for key, value := range row {
						if key == "activity" {
							continue
						}
						if key == "id" {
							assert.Equal(t, value, fields["run_id"])
						} else if _, ok := fields[key]; ok {
							assert.Equal(t, value, fields[key], key)
						}
					}
					if expected != nil {
						delete(expected, "health_url")
						delete(expected, "id")
						expected["run_id"] = "fixture"
						assert.Equal(t, expected, fields)
					}
					assert.NotContains(t, fields, "requests_admitted")
					assert.NotContains(t, fields, "id")
				}
			})
		}
	}
}

func assertRunCacheActivity(t *testing.T, value any, started, finished time.Time) {
	t.Helper()
	activity, ok := value.(map[string]any)
	require.True(t, ok, "activity must be present")
	for key, want := range map[string]any{"state": "idle", "active_requests": float64(0), "busy_engines": float64(0), "stale": false} {
		assert.Equal(t, want, activity[key], key)
	}
	require.IsType(t, "", activity["observed_at"])
	observed, err := time.Parse(time.RFC3339Nano, activity["observed_at"].(string))
	require.NoError(t, err)
	assert.WithinRange(t, observed, started, finished)
	require.IsType(t, float64(0), activity["age_seconds"])
	age := activity["age_seconds"].(float64)
	assert.GreaterOrEqual(t, age, float64(0))
	assert.LessOrEqual(t, age, finished.Sub(started).Seconds())
	require.IsType(t, float64(0), activity["valid_for_seconds"])
	validFor := activity["valid_for_seconds"].(float64)
	assert.Positive(t, validFor)
	assert.LessOrEqual(t, validFor, (2 * constants.HealthCheckInterval).Seconds())
}
