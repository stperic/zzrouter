package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/inferencelog"
)

// usageNode stands up just the inference-log controller over a real store, so
// the assertions cover the HTTP shape without booting a whole node.
func usageNode(t *testing.T) (*httptest.Server, *inferencelog.Store) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	store := inferencelog.NewStore(10, 0)

	engine := gin.New()
	api := engine.Group("/zzrouter/v1")
	NewInferenceLogController(NewInferenceLogService(store), nil).RegisterPublicRoutes(api)

	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv, store
}

func record(store *inferencelog.Store, model, provider string, in, out int64, cost float64, source string) {
	store.Add(inferencelog.LogEntry{
		Model:        model,
		App:          provider,
		Node:         "coord",
		Status:       "success",
		TokensIn:     in,
		TokensOut:    out,
		LatencyMs:    1000,
		TokensPerSec: float64(out),
		Cost:         cost,
		CostSource:   source,
		Timestamp:    time.Now(),
	}, nil)
}

func TestModelUsageEndpointReportsSelfHostedWithoutCost(t *testing.T) {
	srv, store := usageNode(t)
	for range 3 {
		record(store, "qwen2.5-7b", "llamacpp", 100, 50, 0, "")
	}

	status, body := getJSON(t, srv.URL, "/zzrouter/v1/usage/models/qwen2.5-7b")
	require.Equal(t, http.StatusOK, status)

	data := body["data"].(map[string]any)
	assert.NotEmpty(t, data["since"], "the window is node start")
	model := data["model"].(map[string]any)

	assert.Equal(t, float64(3), model["requests"])
	assert.Equal(t, float64(300), model["tokens_in"])
	assert.Equal(t, float64(150), model["tokens_out"])
	// The point of the exercise: usage is real, cost is absent rather than
	// a misleading zero.
	assert.Equal(t, false, model["priced"])
	assert.NotContains(t, model, "cost_usd")
}

func TestModelUsageEndpointReportsCloudCost(t *testing.T) {
	srv, store := usageNode(t)
	record(store, "claude-sonnet", "openrouter", 1000, 500, 0.02, "provider")
	record(store, "claude-sonnet", "openrouter", 1000, 500, 0.03, "provider")

	status, body := getJSON(t, srv.URL, "/zzrouter/v1/usage/models/claude-sonnet")
	require.Equal(t, http.StatusOK, status)

	model := body["data"].(map[string]any)["model"].(map[string]any)
	assert.Equal(t, true, model["priced"])
	assert.InDelta(t, 0.05, model["cost_usd"], 1e-9)
}

// Totals must outlive the ring: this is the difference between "since node
// start" and "over the last N requests".
func TestModelUsageEndpointOutlivesRingEviction(t *testing.T) {
	srv, store := usageNode(t) // capacity 10
	for range 120 {
		record(store, "qwen2.5-7b", "llamacpp", 10, 5, 0, "")
	}

	status, body := getJSON(t, srv.URL, "/zzrouter/v1/usage/models/qwen2.5-7b")
	require.Equal(t, http.StatusOK, status)

	model := body["data"].(map[string]any)["model"].(map[string]any)
	assert.Equal(t, float64(120), model["requests"], "totals must not be capped by ring size")
	assert.Equal(t, float64(1200), model["tokens_in"])
}

func TestModelUsageListEndpoint(t *testing.T) {
	srv, store := usageNode(t)
	record(store, "quiet", "llamacpp", 1, 1, 0, "")
	for range 4 {
		record(store, "busy", "llamacpp", 1, 1, 0, "")
	}

	status, body := getJSON(t, srv.URL, "/zzrouter/v1/usage/models")
	require.Equal(t, http.StatusOK, status)

	models := body["data"].(map[string]any)["models"].([]any)
	require.Len(t, models, 2)
	assert.Equal(t, "busy", models[0].(map[string]any)["model"], "busiest first")
}

func TestModelUsageEndpointUnknownModel(t *testing.T) {
	srv, _ := usageNode(t)
	status, _ := getJSON(t, srv.URL, "/zzrouter/v1/usage/models/never-served")
	assert.Equal(t, http.StatusNotFound, status)
}

// ?provider= scopes to one deployment; without it the name is summed.
func TestModelUsageEndpointProviderScope(t *testing.T) {
	srv, store := usageNode(t)
	record(store, "shared", "llamacpp", 100, 10, 0, "")
	record(store, "shared", "vllm", 200, 20, 0, "")

	_, all := getJSON(t, srv.URL, "/zzrouter/v1/usage/models/shared")
	merged := all["data"].(map[string]any)["model"].(map[string]any)
	assert.Equal(t, float64(2), merged["requests"])
	assert.Equal(t, float64(300), merged["tokens_in"])

	_, scoped := getJSON(t, srv.URL, "/zzrouter/v1/usage/models/shared?provider=vllm")
	one := scoped["data"].(map[string]any)["model"].(map[string]any)
	assert.Equal(t, float64(1), one["requests"])
	assert.Equal(t, float64(200), one["tokens_in"])
}
