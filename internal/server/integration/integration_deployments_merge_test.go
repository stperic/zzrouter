package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	srv "github.com/stperic/zzrouter/internal/server"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createDeployment is a small helper that POSTs a deployment and returns its ID.
func createDeployment(t *testing.T, server *srv.Server, model string, nodes []string) string {
	t.Helper()

	body, _ := json.Marshal(map[string]any{
		"model": model,
		"nodes": nodes,
	})
	req := httptest.NewRequest("POST", "/zzrouter/v1/deployments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code, "deploy failed: %s", w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data := resp["data"].(map[string]any) //nolint:errcheck // test asserts on known fixture shape
	return data["id"].(string)            //nolint:errcheck // test asserts on known fixture shape
}

// seedLiveDownload injects a known download entry into the node's
// DownloadTracker so mergeProgress has something authoritative to merge back
// into the tracker snapshot. Returns the download key used.
func seedLiveDownload(t *testing.T, server *srv.Server, modelName string, downloaded, total int64) string {
	t.Helper()

	dt := srv.TestDownloadTracker(server)
	require.NotNil(t, dt, "server has no DownloadTracker")

	// Use the same key shape the executor uses: host/registry/model.
	key := "test-host/huggingface/" + modelName
	gen := dt.StartDownload(key, modelName)
	dt.UpdateDownloadFullGen(
		key, gen,
		string(constants.StatusDownloading),
		"model.safetensors",
		1, 2,
		float64(downloaded)/float64(total)*100,
		downloaded, total, 1024*1024,
	)
	t.Cleanup(func() { dt.RemoveDownload(key) })
	return key
}

// setNodeDownloadID rewrites the tracker's DeploymentNode.DownloadID so
// mergeProgress can find the live entry. Deployments are created with the
// model name at the end of the key, but the test server's deploy path runs
// through its compatibility gate which rejects most models — instead of
// fighting that, we seed the tracker state directly.
func setNodeDownloadID(t *testing.T, server *srv.Server, id, node, downloadID string) {
	t.Helper()
	require.NoError(t, srv.TestDeploymentsService(server).GetTracker().UpdateNodeDownloadID(id, node, downloadID))
	require.NoError(t, srv.TestDeploymentsService(server).GetTracker().UpdateNodeStatus(id, node, constants.StatusDownloading, ""))
}

// TestDeployments_ModelSuffixExtraction verifies that a POST body with the
// canonical `<model>[:tag][#file][@node]` form has the `@node` and `#file`
// suffixes extracted server-side when the explicit fields are empty. This is
// defense-in-depth — the CLI pre-strips client-side, but direct API consumers
// (curl, non-CLI clients) need the server to handle it too.
func TestDeployments_ModelSuffixExtraction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := srv.NewTestNode(t, srv.TestNodeConfig{AdminKey: srv.TestAdminKey})

	// POST with model carrying both '#file' and '@node' suffixes, leaving the
	// `file` and `nodes` fields empty on the request body.
	body, _ := json.Marshal(map[string]any{
		"model": "suffix-test#Q4_K_M@test-host",
	})
	req := httptest.NewRequest("POST", "/zzrouter/v1/deployments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code, "deploy failed: %s", w.Body.String())

	var resp struct {
		Data struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			File  string `json:"file"`
			Nodes []struct {
				Node string `json:"node"`
			} `json:"nodes"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// Model should be the clean name, without the '#file' or '@node' suffix.
	assert.Equal(t, "suffix-test", resp.Data.Model, "model should be stripped of '#file' and '@node' suffixes")
	assert.Equal(t, "Q4_K_M", resp.Data.File, "file should be extracted from '#file' suffix")
	require.Len(t, resp.Data.Nodes, 1, "node should be extracted from '@node' suffix")
	assert.Equal(t, "test-host", resp.Data.Nodes[0].Node)

	// Tracker should carry the same clean values — confirms the downstream
	// internal call body would be built from the stripped model too.
	tracked := srv.TestDeploymentsService(server).GetTracker().GetDeployment(resp.Data.ID)
	require.NotNil(t, tracked)
	assert.Equal(t, "suffix-test", tracked.Model)
	assert.Equal(t, "Q4_K_M", tracked.File)
}

// TestDeployments_MergeProgress_Get verifies GET /:id overlays live bytes/
// progress/speed from the node's DownloadTracker onto the tracker snapshot.
func TestDeployments_MergeProgress_Get(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := srv.NewTestNode(t, srv.TestNodeConfig{AdminKey: srv.TestAdminKey})

	id := createDeployment(t, server, "merge-test", []string{"test-host"})

	// Seed a live download entry and wire it to the tracker's node slice.
	key := seedLiveDownload(t, server, "merge-test", 512, 1024)
	setNodeDownloadID(t, server, id, "test-host", key)

	req := httptest.NewRequest("GET", "/zzrouter/v1/deployments/"+id, nil)
	req.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Nodes  []struct {
				Node            string  `json:"node"`
				BytesDownloaded int64   `json:"bytes_downloaded"`
				BytesTotal      int64   `json:"bytes_total"`
				Progress        float64 `json:"progress"`
				Speed           int64   `json:"speed"`
				Status          string  `json:"status"`
				DownloadID      string  `json:"download_id"`
			} `json:"nodes"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Data.Nodes, 1)
	n := resp.Data.Nodes[0]
	assert.Equal(t, "test-host", n.Node)
	assert.Equal(t, int64(512), n.BytesDownloaded, "bytes_downloaded should be merged from live tracker")
	assert.Equal(t, int64(1024), n.BytesTotal)
	assert.InDelta(t, 50.0, n.Progress, 0.01)
	assert.Greater(t, n.Speed, int64(0))
	assert.Equal(t, "downloading", n.Status)
	assert.Equal(t, "downloading", resp.Data.Status)
}

// TestDeployments_MergeProgress_List verifies the list endpoint also merges.
func TestDeployments_MergeProgress_List(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := srv.NewTestNode(t, srv.TestNodeConfig{AdminKey: srv.TestAdminKey})

	id := createDeployment(t, server, "list-merge", []string{"test-host"})
	key := seedLiveDownload(t, server, "list-merge", 750, 1000)
	setNodeDownloadID(t, server, id, "test-host", key)

	req := httptest.NewRequest("GET", "/zzrouter/v1/deployments", nil)
	req.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data []struct {
			ID    string `json:"id"`
			Nodes []struct {
				Progress float64 `json:"progress"`
			} `json:"nodes"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	var found bool
	for _, d := range resp.Data {
		if d.ID == id {
			require.Len(t, d.Nodes, 1)
			assert.InDelta(t, 75.0, d.Nodes[0].Progress, 0.01)
			found = true
		}
	}
	assert.True(t, found, "created deployment missing from list")
}

// TestDeployments_FilterByNode verifies ?node= filters the list.
func TestDeployments_FilterByNode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := srv.NewTestNode(t, srv.TestNodeConfig{AdminKey: srv.TestAdminKey})

	idA := createDeployment(t, server, "filter-a", []string{"test-host"})
	idB := createDeployment(t, server, "filter-b", []string{"other-host"})

	req := httptest.NewRequest("GET", "/zzrouter/v1/deployments?node=test-host", nil)
	req.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data []struct {
			ID    string `json:"id"`
			Nodes []struct {
				Node string `json:"node"`
			} `json:"nodes"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	var sawA, sawB bool
	for _, d := range resp.Data {
		if d.ID == idA {
			sawA = true
		}
		if d.ID == idB {
			sawB = true
		}
	}
	assert.True(t, sawA, "test-host deployment should be in filtered list")
	assert.False(t, sawB, "other-host deployment should be filtered out")
}

// TestDeployments_CancelPerNode verifies DELETE /:id/nodes/:node cancels
// only that node and preserves the rest of the deployment.
func TestDeployments_CancelPerNode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := srv.NewTestNode(t, srv.TestNodeConfig{AdminKey: srv.TestAdminKey})

	svc := srv.TestDeploymentsService(server)
	// Seed a multi-node deployment via the tracker to avoid compatibility
	// gating on cluster-member nodes from the public deploy path.
	d := svc.GetTracker().CreateDeployment("cancel-node-test", "gguf", "huggingface", "",
		[]string{"test-host", "worker-1"})
	require.NoError(t, svc.GetTracker().UpdateNodeStatus(d.ID, "test-host", constants.StatusDownloading, ""))
	require.NoError(t, svc.GetTracker().UpdateNodeStatus(d.ID, "worker-1", constants.StatusDownloading, ""))
	require.NoError(t, svc.GetTracker().UpdateDeploymentStatus(d.ID, constants.StatusDownloading, "downloading"))

	req := httptest.NewRequest("DELETE", "/zzrouter/v1/deployments/"+d.ID+"/nodes/test-host", nil)
	req.Header.Set("X-API-Key", srv.TestAdminKey)

	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	// Re-read deployment and verify only test-host was cancelled.
	got := svc.GetDeployment(context.Background(), d.ID)
	require.NotNil(t, got)
	var testHost, worker srv.DeploymentNode
	for _, n := range got.Nodes {
		switch n.Node {
		case "test-host":
			testHost = n
		case "worker-1":
			worker = n
		}
	}
	assert.Equal(t, constants.StatusFailed, testHost.Status)
	assert.Equal(t, "cancelled by user", testHost.Error)
	assert.Equal(t, constants.StatusDownloading, worker.Status,
		"other node must not be affected by per-node cancel")
}

// TestDeployments_CancelPerNode_NotFound covers the 404 branches.
func TestDeployments_CancelPerNode_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := srv.NewTestNode(t, srv.TestNodeConfig{AdminKey: srv.TestAdminKey})

	// Unknown deployment ID.
	req := httptest.NewRequest("DELETE", "/zzrouter/v1/deployments/bogus/nodes/test-host", nil)
	req.Header.Set("X-API-Key", srv.TestAdminKey)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)

	// Known deployment, unknown node.
	svc := srv.TestDeploymentsService(server)
	d := svc.GetTracker().CreateDeployment("nf", "gguf", "huggingface", "", []string{"test-host"})
	req2 := httptest.NewRequest("DELETE", "/zzrouter/v1/deployments/"+d.ID+"/nodes/nope", nil)
	req2.Header.Set("X-API-Key", srv.TestAdminKey)
	w2 := httptest.NewRecorder()
	server.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusNotFound, w2.Code)
}

// TestDeployments_StopAll_HostScoped verifies DELETE /deployments?node=X
// cancels only the slices targeting X across all active deployments.
func TestDeployments_StopAll_HostScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := srv.NewTestNode(t, srv.TestNodeConfig{AdminKey: srv.TestAdminKey})

	svc := srv.TestDeploymentsService(server)
	dA := svc.GetTracker().CreateDeployment("stop-a", "gguf", "huggingface", "",
		[]string{"test-host", "worker-1"})
	dB := svc.GetTracker().CreateDeployment("stop-b", "gguf", "huggingface", "",
		[]string{"worker-1"})
	for _, pair := range []struct{ id, node string }{
		{dA.ID, "test-host"}, {dA.ID, "worker-1"}, {dB.ID, "worker-1"},
	} {
		require.NoError(t, svc.GetTracker().UpdateNodeStatus(pair.id, pair.node, constants.StatusDownloading, ""))
	}
	require.NoError(t, svc.GetTracker().UpdateDeploymentStatus(dA.ID, constants.StatusDownloading, ""))
	require.NoError(t, svc.GetTracker().UpdateDeploymentStatus(dB.ID, constants.StatusDownloading, ""))

	req := httptest.NewRequest("DELETE", "/zzrouter/v1/deployments?node=worker-1", nil)
	req.Header.Set("X-API-Key", srv.TestAdminKey)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp struct {
		Data struct {
			Node      string `json:"node"`
			Cancelled int    `json:"cancelled"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "worker-1", resp.Data.Node)
	assert.Equal(t, 2, resp.Data.Cancelled)

	// Verify test-host slice of dA was untouched.
	gotA := svc.GetDeployment(context.Background(), dA.ID)
	require.NotNil(t, gotA)
	for _, n := range gotA.Nodes {
		if n.Node == "test-host" {
			assert.Equal(t, constants.StatusDownloading, n.Status,
				"test-host slice must not be touched by host-scoped cancel of worker-1")
		}
		if n.Node == "worker-1" {
			assert.Equal(t, constants.StatusFailed, n.Status)
		}
	}
}
