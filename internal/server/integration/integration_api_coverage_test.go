package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"

	srv "github.com/stperic/zzrouter/internal/server"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublicAPI covers public API endpoints using a single shared server.
func TestPublicAPI(t *testing.T) {
	server := srv.NewTestNode(t, srv.TestNodeConfig{
		AdminKey: srv.TestAdminKey,
	})

	// =========================================================================
	// HEALTH & VERSION
	// =========================================================================

	t.Run("Health", func(t *testing.T) {
		for _, ep := range []string{"/health", "/health/live", "/health/ready"} {
			resp := srv.MakeRequest(t, server, srv.TestRequest{Method: "GET", Path: ep})
			assert.Equal(t, http.StatusOK, resp.Code)
		}
	})

	t.Run("Version", func(t *testing.T) {
		for _, ep := range []string{"/zzrouter/v1/server/version", "/zzrouter/v1/server/version/compatibility"} {
			resp := srv.MakeAuthRequest(t, server, "GET", ep, srv.TestAdminKey, nil)
			assert.Equal(t, http.StatusOK, resp.Code)
		}
	})

	// =========================================================================
	// APPS
	// =========================================================================

	t.Run("Apps_List", func(t *testing.T) {
		resp := srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/providers", srv.TestAdminKey, nil)
		assert.Equal(t, http.StatusOK, resp.Code)
		var result map[string]any
		require.NoError(t, json.Unmarshal(resp.Body, &result))
		_, ok := result["data"].([]any)
		require.True(t, ok, "Expected data array")
	})

	t.Run("Apps_ListRunning", func(t *testing.T) {
		resp := srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/providers?running=true", srv.TestAdminKey, nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("Apps_GetNonExistent", func(t *testing.T) {
		resp := srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/providers/nonexistent-app", srv.TestAdminKey, nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusOK, http.StatusNotFound})
	})

	t.Run("Apps_EnableDisable", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "PATCH", "/zzrouter/v1/providers/mlx", srv.TestAdminKey, map[string]any{"enabled": true})
		srv.MakeAuthRequest(t, server, "PATCH", "/zzrouter/v1/providers/mlx", srv.TestAdminKey, map[string]any{"enabled": false})
	})

	// =========================================================================
	// DISCOVERY (excluding /discover which does 30s mDNS scan)
	// =========================================================================

	t.Run("Discovery", func(t *testing.T) {
		for _, ep := range []string{
			"/zzrouter/v1/discover/providers",
			"/zzrouter/v1/discover/hardware",
			"/zzrouter/v1/discover/hardware/gpus",
			"/zzrouter/v1/discover/software",
			"/zzrouter/v1/discover/providers/host",
		} {
			resp := srv.MakeAuthRequest(t, server, "GET", ep, srv.TestAdminKey, nil)
			t.Logf("%s → %d", ep, resp.Code)
		}
	})

	// =========================================================================
	// SYSTEM
	// =========================================================================

	t.Run("System_Info", func(t *testing.T) {
		resp := srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/system", srv.TestAdminKey, nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("Server_Identity", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/server/identity", srv.TestAdminKey, nil)
	})

	t.Run("Server_Logs", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/server/logs", srv.TestAdminKey, nil)
	})

	// =========================================================================
	// NODES
	// =========================================================================

	t.Run("Nodes_List", func(t *testing.T) {
		resp := srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/nodes", srv.TestAdminKey, nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("Nodes_CompatibleMissingModel", func(t *testing.T) {
		resp := srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/nodes/compatible", srv.TestAdminKey, nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	// =========================================================================
	// CACHE
	// =========================================================================

	t.Run("Cache_Stats", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/cache/stats", srv.TestAdminKey, nil)
	})

	// =========================================================================
	// RUNS
	// =========================================================================

	t.Run("Runs_Capabilities", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/runs/capabilities", srv.TestAdminKey, nil)
	})

	t.Run("Runs_Preview", func(t *testing.T) {
		body := map[string]any{"provider": "mlx", "launch_mode": "native", "model_name": "test/model"}
		srv.MakeAuthRequest(t, server, "POST", "/zzrouter/v1/runs/preview", srv.TestAdminKey, body)
	})

	t.Run("Runs_ValidateFiles", func(t *testing.T) {
		body := map[string]any{
			"files": []map[string]string{{"name": "model", "host_path": "/tmp/test.gguf"}},
		}
		srv.MakeAuthRequest(t, server, "POST", "/zzrouter/v1/runs/validate/files", srv.TestAdminKey, body)
	})

	// =========================================================================
	// MODELS
	// =========================================================================

	t.Run("Models_Stats", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/models/stats", srv.TestAdminKey, nil)
	})

	t.Run("Models_Scan", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "POST", "/zzrouter/v1/models/scan", srv.TestAdminKey, nil)
	})

	t.Run("Models_ListWithFilters", func(t *testing.T) {
		for _, ep := range []string{
			"/zzrouter/v1/models",
			"/zzrouter/v1/models?app=mlx",
			"/zzrouter/v1/models?repo=huggingface",
			"/zzrouter/v1/models?limit=10&offset=0",
		} {
			resp := srv.MakeAuthRequest(t, server, "GET", ep, srv.TestAdminKey, nil)
			assert.Equal(t, http.StatusOK, resp.Code)
		}
	})

	// =========================================================================
	// SEARCH
	// =========================================================================

	t.Run("Search", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/search?q=llama", srv.TestAdminKey, nil)
	})

	// =========================================================================
	// OLLAMA COMPAT
	// =========================================================================

	// /api/* is gated on the ollama provider being enabled in the cluster.
	// The test server has no ollama configured, so every /api/* request
	// returns 503 with an install hint — the honest contract for an
	// Ollama-less cluster. (Pre-gate behavior synthesized version/tags/ps
	// responses; that masked install gaps from agents.)
	t.Run("Ollama_Version_Gated", func(t *testing.T) {
		resp := srv.MakeRequest(t, server, srv.TestRequest{Method: "GET", Path: "/api/version"})
		assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
		assert.Contains(t, string(resp.Body), "no Ollama backend available")
	})

	t.Run("Ollama_Tags_Gated", func(t *testing.T) {
		resp := srv.MakeRequest(t, server, srv.TestRequest{Method: "GET", Path: "/api/tags"})
		assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
	})

	t.Run("Ollama_Ps_Gated", func(t *testing.T) {
		resp := srv.MakeRequest(t, server, srv.TestRequest{Method: "GET", Path: "/api/ps"})
		assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
	})

	t.Run("Ollama_Show_Gated", func(t *testing.T) {
		resp := srv.MakeRequest(t, server, srv.TestRequest{Method: "POST", Path: "/api/show", Body: map[string]string{"model": "test:model"}})
		assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
	})

	// =========================================================================
	// OPENAI COMPAT
	// =========================================================================

	t.Run("OpenAI_Models", func(t *testing.T) {
		resp := srv.MakeRequest(t, server, srv.TestRequest{Method: "GET", Path: "/v1/models"})
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	// =========================================================================
	// CLUSTER
	// =========================================================================

	t.Run("Cluster_ListNodes", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/nodes", srv.TestAdminKey, nil)
	})

	t.Run("Cluster_TestConnection", func(t *testing.T) {
		body := map[string]string{"address": "localhost:9090"}
		srv.MakeAuthRequest(t, server, "POST", "/zzrouter/v1/cluster/connect", srv.TestAdminKey, body)
	})

	// =========================================================================
	// PULLS
	// =========================================================================

	t.Run("Pulls_List", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/deployments", srv.TestAdminKey, nil)
	})

	t.Run("Pulls_StopNonExistent", func(t *testing.T) {
		srv.MakeAuthRequest(t, server, "DELETE", "/zzrouter/v1/deployments/nonexistent-deploy-id", srv.TestAdminKey, nil)
	})
}

// TestInternalAPI_ReachableViaInternalEngine confirms the /internal
// routes are served by the cluster-port engine (mTLS-protected in
// production; bypassed here by driving the engine directly). The
// end-to-end transport-layer auth is covered by the two-node harness
// in integration_mtls_harness_test.go when Tier 2 lands.
func TestInternalAPI_ReachableViaInternalEngine(t *testing.T) {
	server := srv.NewTestNode(t, srv.TestNodeConfig{
		AdminKey: srv.TestAdminKey,
	})

	internal := func(method, path string) *srv.TestResponse {
		return srv.MakeInternalRequest(t, server, srv.TestRequest{
			Method: method,
			Path:   path,
		})
	}

	t.Run("Resources", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/resources")
		t.Logf("Internal resources → %d", resp.Code)
	})

	t.Run("Health", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/health")
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("Version", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/version")
		assert.Equal(t, http.StatusOK, resp.Code)
	})
}
