// Package server provides integration tests for internal API endpoints (/zzrouter/internal/*).
// These endpoints are used for cluster-internal communication and require cluster key authentication.
// All subtests share a single server instance for fast execution.
package integration_test

import (
	"net/http"
	"strings"
	"testing"

	srv "github.com/stperic/zzrouter/internal/server"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInternalAPI(t *testing.T) {
	server := srv.NewTestNode(t, srv.TestNodeConfig{
		AdminKey:   srv.TestAdminKey,
		ClusterKey: srv.TestClusterKey,
	})

	// Drives the server's internal engine directly (the mTLS cluster
	// port's handler). Bypasses the transport-layer auth because no
	// TLS handshake is done — that is authoritatively tested
	// end-to-end by the two-node mTLS harness (when it lands).
	internal := func(method, path string, body any) *srv.TestResponse {
		return srv.MakeInternalRequest(t, server, srv.TestRequest{
			Method: method,
			Path:   path,
			Body:   body,
		})
	}

	// =========================================================================
	// SYSTEM
	// =========================================================================

	t.Run("Health", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/health", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
		assert.Contains(t, string(resp.Body), "status")
	})

	t.Run("Version", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/version", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
		assert.Contains(t, string(resp.Body), "version")
	})

	t.Run("SystemInfo", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/system", nil)
		require.Equal(t, http.StatusOK, resp.Code)
		assert.Contains(t, string(resp.Body), "hostname")
	})

	t.Run("Resources", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/resources", nil)
		require.Equal(t, http.StatusOK, resp.Code)
	})

	// =========================================================================
	// MODELS
	// =========================================================================

	t.Run("ListModels", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/models", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusOK, http.StatusNoContent})
	})

	t.Run("ListModels_WithFilters", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/models?node=localhost&repository=ollama", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusOK, http.StatusNoContent})
	})

	t.Run("ShowModel_MissingName", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/models/show", nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("ShowModel_NotFound", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/models/show?model=nonexistent-model-12345", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusNotFound, http.StatusBadRequest})
	})

	t.Run("DeleteModels_MissingBody", func(t *testing.T) {
		resp := internal("DELETE", "/zzrouter/v1/internal/models", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusBadRequest, http.StatusUnprocessableEntity})
	})

	t.Run("DeleteModels_NonexistentModel", func(t *testing.T) {
		body := map[string]any{
			"models": []map[string]string{
				{"name": "nonexistent-model", "node": "localhost"},
			},
		}
		resp := internal("DELETE", "/zzrouter/v1/internal/models", body)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusOK, http.StatusNotFound, http.StatusBadRequest})
	})

	t.Run("RefreshCache", func(t *testing.T) {
		resp := internal("POST", "/zzrouter/v1/internal/models/refresh", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent})
	})

	// =========================================================================
	// RUNS
	// =========================================================================

	t.Run("ListRuns", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/runs", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
		body := string(resp.Body)
		assert.True(t, strings.Contains(body, "data") || strings.Contains(body, "instances"))
	})

	t.Run("LaunchRun_MissingModel", func(t *testing.T) {
		resp := internal("POST", "/zzrouter/v1/internal/runs", map[string]any{})
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("LaunchRun_InvalidModel", func(t *testing.T) {
		body := map[string]any{"model_name": "nonexistent-model-12345"}
		resp := internal("POST", "/zzrouter/v1/internal/runs", body)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity})
	})

	t.Run("LoadModel_MissingModel", func(t *testing.T) {
		resp := internal("POST", "/zzrouter/v1/internal/runs/load", map[string]any{})
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("LoadModel_InvalidModel", func(t *testing.T) {
		body := map[string]any{"model_name": "nonexistent-model-12345"}
		resp := internal("POST", "/zzrouter/v1/internal/runs/load", body)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity})
	})

	t.Run("PreviewRun_MissingModel", func(t *testing.T) {
		resp := internal("POST", "/zzrouter/v1/internal/runs/preview", map[string]any{})
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("GetRun_NotFound", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/runs/nonexistent-id-12345", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})

	t.Run("StopRun_NotFound", func(t *testing.T) {
		resp := internal("DELETE", "/zzrouter/v1/internal/runs/nonexistent-id-12345", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})

	t.Run("RestartRun_NotFound", func(t *testing.T) {
		resp := internal("POST", "/zzrouter/v1/internal/runs/nonexistent-id-12345/restart", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})

	t.Run("GetRunHealth_NotFound", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/runs/nonexistent-id-12345/health", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})

	t.Run("GetRunLogs_NotFound", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/runs/nonexistent-id-12345/logs", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})

	// =========================================================================
	// PULLS
	// =========================================================================

	t.Run("Pull_MissingModel", func(t *testing.T) {
		resp := internal("POST", "/zzrouter/v1/internal/deployments", map[string]any{})
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("ListPulls", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/deployments", nil)
		assert.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("StopAllPulls", func(t *testing.T) {
		resp := internal("DELETE", "/zzrouter/v1/internal/deployments/all", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusOK, http.StatusNoContent})
	})

	t.Run("StopPull_NotFound", func(t *testing.T) {
		resp := internal("DELETE", "/zzrouter/v1/internal/deployments/stop?key=nonexistent-id-12345", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusNotFound, http.StatusOK})
	})

	// =========================================================================
	// APPS
	// =========================================================================

	t.Run("ListApps", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/providers", nil)
		require.Equal(t, http.StatusOK, resp.Code)
		body := string(resp.Body)
		assert.True(t, strings.Contains(body, "data") || strings.Contains(body, "providers"))
	})

	t.Run("GetApp_NotFound", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/providers/nonexistent-app-12345", nil)
		assert.Equal(t, http.StatusNotFound, resp.Code)
	})

	t.Run("UpdateApp_NotFound", func(t *testing.T) {
		body := map[string]any{"enabled": true}
		resp := internal("PATCH", "/zzrouter/v1/internal/providers/nonexistent-app-12345", body)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusNotFound, http.StatusBadRequest})
	})

	// =========================================================================
	// NODES
	// =========================================================================

	t.Run("ListNodes", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/nodes", nil)
		require.Equal(t, http.StatusOK, resp.Code)
		assert.Contains(t, string(resp.Body), "data")
	})

	t.Run("ListCompatibleNodes_MissingModel", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/nodes/compatible", nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("ListCompatibleNodes_WithModel", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/nodes/compatible?model=llama3", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusOK, http.StatusNoContent, http.StatusBadRequest})
	})

	// =========================================================================
	// SYNC
	// =========================================================================

	t.Run("SyncManifest_MissingModel", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/sync/manifest", nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("SyncManifest_NotFound", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/sync/manifest?model=nonexistent-model-12345", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusNotFound, http.StatusBadRequest})
	})

	t.Run("SyncFile_MissingPath", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/sync/file", nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("SyncFile_PathTraversal", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/sync/file?path=../../../etc/passwd", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusBadRequest, http.StatusForbidden})
	})

	t.Run("SyncExists_MissingModel", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/sync/exists", nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("SyncExists_NotFound", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/sync/exists?model=nonexistent-model-12345", nil)
		require.Equal(t, http.StatusOK, resp.Code)
		assert.Contains(t, string(resp.Body), "exists")
	})

	t.Run("SyncPull_MissingBody", func(t *testing.T) {
		resp := internal("POST", "/zzrouter/v1/internal/sync/deploy", nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusBadRequest, http.StatusUnprocessableEntity})
	})

	// =========================================================================
	// OLLAMA COMPAT INTERNAL
	// =========================================================================

	// /zzrouter/v1/internal/ollama/tags removed in M2.B: /api/tags now flows
	// through ModelService directly, so the broadcast endpoint is gone.

	t.Run("OllamaPs", func(t *testing.T) {
		resp := internal("GET", "/zzrouter/v1/internal/ollama/ps", nil)
		require.Equal(t, http.StatusOK, resp.Code)
		assert.Contains(t, string(resp.Body), "models")
	})
}
