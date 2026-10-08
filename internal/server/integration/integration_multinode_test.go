package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	srv "github.com/stperic/zzrouter/internal/server"
	"github.com/stperic/zzrouter/pkg/constants"
	modelsync "github.com/stperic/zzrouter/pkg/model/sync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMultiNodeDeploy_Creation tests that multi-node deploys create a deployment.
func TestMultiNodeDeploy_Creation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := srv.NewTestNode(t, srv.TestNodeConfig{
		AdminKey: srv.TestAdminKey,
	})

	reqBody := map[string]any{
		"model":    "test-model",
		"registry": "huggingface",
		"nodes":    []string{"test-host", "node-2", "node-3"},
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/zzrouter/v1/deployments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)

	var resp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	data, ok := resp["data"].(map[string]any)
	require.True(t, ok, "expected data object in SuccessResponse envelope")

	assert.NotEmpty(t, data["id"])
	assert.Equal(t, "test-model", data["model"])
	assert.NotEmpty(t, data["status"])

	// nodes[] should contain the three requested targets (deduped).
	nodes, ok := data["nodes"].([]any)
	require.True(t, ok)
	assert.Equal(t, 3, len(nodes))
}

// TestMultiNodeDeploy_List tests listing deployments via the unified surface.
func TestMultiNodeDeploy_List(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := srv.NewTestNode(t, srv.TestNodeConfig{
		AdminKey: srv.TestAdminKey,
	})

	reqBody := map[string]any{
		"model": "test-model",
		"nodes": []string{"test-host", "node-2"},
	}
	body, _ := json.Marshal(reqBody)

	createReq := httptest.NewRequest("POST", "/zzrouter/v1/deployments", bytes.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, createReq)
	require.Equal(t, http.StatusAccepted, w.Code)

	listReq := httptest.NewRequest("GET", "/zzrouter/v1/deployments", nil)
	listReq.Header.Set("X-API-Key", srv.TestAdminKey)

	w = httptest.NewRecorder()
	server.ServeHTTP(w, listReq)

	assert.Equal(t, http.StatusOK, w.Code)

	var listResp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &listResp)
	require.NoError(t, err)

	items, ok := listResp["data"].([]any)
	require.True(t, ok, "expected data array in ListResponse envelope")
	assert.GreaterOrEqual(t, len(items), 1)
}

// TestMultiNodeDeploy_Get tests fetching a specific deployment.
func TestMultiNodeDeploy_Get(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := srv.NewTestNode(t, srv.TestNodeConfig{
		AdminKey: srv.TestAdminKey,
	})

	reqBody := map[string]any{
		"model": "llama-test",
		"nodes": []string{"test-host", "worker-1"},
	}
	body, _ := json.Marshal(reqBody)

	createReq := httptest.NewRequest("POST", "/zzrouter/v1/deployments", bytes.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, createReq)
	require.Equal(t, http.StatusAccepted, w.Code)

	var createResp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &createResp)
	require.NoError(t, err)

	createData := createResp["data"].(map[string]any)
	id := createData["id"].(string)

	getReq := httptest.NewRequest("GET", "/zzrouter/v1/deployments/"+id, nil)
	getReq.Header.Set("X-API-Key", srv.TestAdminKey)

	w = httptest.NewRecorder()
	server.ServeHTTP(w, getReq)

	assert.Equal(t, http.StatusOK, w.Code)

	var detailResp map[string]any
	err = json.Unmarshal(w.Body.Bytes(), &detailResp)
	require.NoError(t, err)

	detail, ok := detailResp["data"].(map[string]any)
	require.True(t, ok, "expected data object in SuccessResponse envelope")
	assert.Equal(t, id, detail["id"])
	assert.Equal(t, "llama-test", detail["model"])

	nodes, ok := detail["nodes"].([]any)
	require.True(t, ok)
	assert.Equal(t, 2, len(nodes))
}

// TestMultiNodeDeploy_Cancel tests cancelling via DELETE /deployments/:id.
func TestMultiNodeDeploy_Cancel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := srv.NewTestNode(t, srv.TestNodeConfig{
		AdminKey: srv.TestAdminKey,
	})

	reqBody := map[string]any{
		"model": "cancel-test",
		"nodes": []string{"test-host"},
	}
	body, _ := json.Marshal(reqBody)

	createReq := httptest.NewRequest("POST", "/zzrouter/v1/deployments", bytes.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, createReq)
	require.Equal(t, http.StatusAccepted, w.Code)

	var createResp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &createResp)
	require.NoError(t, err)

	createData := createResp["data"].(map[string]any)
	id := createData["id"].(string)

	cancelReq := httptest.NewRequest("DELETE", "/zzrouter/v1/deployments/"+id, nil)
	cancelReq.Header.Set("X-API-Key", srv.TestAdminKey)

	w = httptest.NewRecorder()
	server.ServeHTTP(w, cancelReq)

	assert.Equal(t, http.StatusOK, w.Code)

	var cancelResp map[string]any
	err = json.Unmarshal(w.Body.Bytes(), &cancelResp)
	require.NoError(t, err)

	cancelData := cancelResp["data"].(map[string]any)
	assert.Equal(t, "cancelled", cancelData["status"])
}

// TestMultiNodeDeploy_NotFound verifies 404 for a bogus deployment id.
func TestMultiNodeDeploy_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := srv.NewTestNode(t, srv.TestNodeConfig{
		AdminKey: srv.TestAdminKey,
	})

	req := httptest.NewRequest("GET", "/zzrouter/v1/deployments/nonexistent_id", nil)
	req.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestDeploymentTracker exercises the tracker directly.
func TestDeploymentTracker(t *testing.T) {
	tracker := srv.NewDeploymentTracker()

	d := tracker.CreateDeployment("test-model", "gguf", "huggingface", "", []string{"master", "worker-1", "worker-2"})

	assert.NotEmpty(t, d.ID)
	assert.Equal(t, "test-model", d.Model)
	assert.Equal(t, constants.StatusPending, d.Status)
	assert.Equal(t, 3, d.NodesTotal)
	assert.Equal(t, 3, d.NodesPending)

	assert.Equal(t, "internet", d.Nodes[0].Source)
	assert.Equal(t, "internet", d.Nodes[1].Source)
	assert.Equal(t, "internet", d.Nodes[2].Source)

	err := tracker.UpdateDeploymentStatus(d.ID, constants.StatusDownloading, "Downloading to master")
	require.NoError(t, err)

	retrieved := tracker.GetDeployment(d.ID)
	assert.Equal(t, constants.StatusDownloading, retrieved.Status)

	err = tracker.UpdateNodeStatus(d.ID, "master", constants.StatusDownloading, "")
	require.NoError(t, err)

	err = tracker.UpdateNodeDownloadID(d.ID, "master", "master/huggingface/test-model")
	require.NoError(t, err)

	retrieved = tracker.GetDeployment(d.ID)
	assert.Equal(t, 1, retrieved.NodesInProgress)
	assert.Equal(t, 2, retrieved.NodesPending)
	assert.Equal(t, "master/huggingface/test-model", retrieved.Nodes[0].DownloadID)

	err = tracker.UpdateNodeProgress(d.ID, "master", 500, 1000, 100)
	require.NoError(t, err)

	retrieved = tracker.GetDeployment(d.ID)
	assert.Equal(t, int64(500), retrieved.Nodes[0].BytesDownloaded)
	assert.Equal(t, int64(1000), retrieved.Nodes[0].BytesTotal)
	assert.Equal(t, float64(50), retrieved.Nodes[0].Progress)

	err = tracker.UpdateNodeStatus(d.ID, "master", constants.StatusCompleted, "")
	require.NoError(t, err)

	retrieved = tracker.GetDeployment(d.ID)
	assert.Equal(t, 1, retrieved.NodesComplete)
	assert.Equal(t, 0, retrieved.NodesInProgress)

	all := tracker.ListDeployments(false)
	assert.Equal(t, 1, len(all))

	err = tracker.CancelDeployment(d.ID)
	require.NoError(t, err)

	retrieved = tracker.GetDeployment(d.ID)
	assert.Equal(t, constants.StatusCancelled, retrieved.Status)
}

// TestSyncSecurity_PathValidation guards against path traversal in sync.
func TestSyncSecurity_PathValidation(t *testing.T) {
	security := modelsync.NewSecurity()

	_, err := security.ValidateModelPath("../../../etc/passwd")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "traversal")

	_, err = security.ValidateModelPath("model/../../secrets")
	assert.Error(t, err)

	_, err = security.ValidateModelPath("")
	assert.Error(t, err)
}

// TestIsModelFile tests model file detection.
func TestIsModelFile(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"model.gguf", true},
		{"model.safetensors", true},
		{"model.bin", true},
		{"config.json", true},
		{"tokenizer.json", true},
		{"README.md", false},
		{"script.py", false},
		{".hidden", false},
	}

	for _, tc := range tests {
		t.Run(tc.filename, func(t *testing.T) {
			result := modelsync.IsModelFile(tc.filename)
			assert.Equal(t, tc.expected, result, "modelsync.IsModelFile(%q)", tc.filename)
		})
	}
}

// TestMultiNodeDeploy_TwoNodes confirms POST /deployments accepts
// multi-node target lists. The dispatch itself is async; this test only
// verifies the deployment record is created on the master, not the
// downstream worker fan-out (that's covered by the two-node mTLS
// harness when it lands).
func TestMultiNodeDeploy_TwoNodes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	masterNode := srv.NewTestNode(t, srv.TestNodeConfig{
		AdminKey: srv.TestAdminKey,
	})

	require.NotNil(t, masterNode)

	reqBody := map[string]any{
		"model": "two-server-test",
		"nodes": []string{"test-host", "worker-node"},
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/zzrouter/v1/deployments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	masterNode.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)

	var resp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	data := resp["data"].(map[string]any)
	assert.NotEmpty(t, data["id"])

	t.Logf("Created multi-node deployment: %s", data["id"])
}
