package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

func coordTestServer(t *testing.T) *Server {
	t.Helper()
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: pkgConfig.ClusterModeCoordinator})
	require.NoError(t, s.cluster.listener.Start(t.Context()))
	t.Cleanup(func() { _ = s.cluster.listener.Stop(context.Background()) })
	s.clusterHandlers.advertiseURL = "https://unreachable-coordinator.example:9091"
	return s
}
func armedWorker(t *testing.T, coordinatorURL string) *clusternode.Node {
	t.Helper()
	dir := t.TempDir()
	n, err := clusternode.New(clusternode.Config{Mode: clusternode.Unclaimed, BindHost: "127.0.0.1", NodeName: "enrolled-worker", IdentityDir: filepath.Join(dir, "identity"), ClusterDir: filepath.Join(dir, "cluster"), PairingPath: filepath.Join(dir, "pairing.txt")})
	require.NoError(t, err)
	require.NoError(t, n.SetAdminAPIHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zzrouter/v1/internal/version" {
			_ = json.NewEncoder(w).Encode(map[string]int{"cluster_protocol": version.ClusterProtocolVersion})
			return
		}
		http.NotFound(w, r)
	})))
	require.NoError(t, n.SetWorkerCompatHandler(http.NotFoundHandler()))
	require.NoError(t, n.Start(t.Context()))
	t.Cleanup(func() { _ = n.Stop(context.Background()) })
	_, err = n.BeginPairing(t.Context(), clusternode.BeginPairingOptions{CoordinatorURL: coordinatorURL})
	require.NoError(t, err)
	return n
}
func postEnrollment(t *testing.T, s *Server, body any, key string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/cluster/pairing/accept", bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-API-Key", key)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	return w
}
func TestClusterPairingAccept_CoordinatorDialsArmedNode(t *testing.T) {
	s := coordTestServer(t)
	worker := armedWorker(t, s.clusterHandlers.advertiseURL)
	window, ok := worker.PairingWindowInfoSnapshot()
	require.True(t, ok)
	result := postEnrollment(t, s, pairingAcceptRequest{Code: window.Code, WorkerURL: window.WorkerURL, WorkerFingerprint: window.Fingerprint}, TestAdminKey)
	require.Equal(t, http.StatusOK, result.Code, result.Body.String())
	require.Equal(t, clusternode.Worker, worker.Mode())
	require.Equal(t, s.clusterHandlers.advertiseURL, worker.CoordinatorURL())
	var out pairingAcceptResponse
	require.NoError(t, json.Unmarshal(result.Body.Bytes(), &out))
	require.Equal(t, window.Fingerprint, out.Fingerprint)
}
func TestClusterPairingAccept_RequiresEnrollmentInputs(t *testing.T) {
	s := coordTestServer(t)
	result := postEnrollment(t, s, map[string]string{"code": "ABCDEFGHIJKLMNOP"}, TestAdminKey)
	require.Equal(t, 400, result.Code)
	result = postEnrollment(t, s, map[string]string{"code": "ABCDEFGHIJKLMNOP"}, "")
	require.Equal(t, 401, result.Code)
}
func TestClusterPairingAccept_WrongFingerprintLeavesNodeUnclaimed(t *testing.T) {
	s := coordTestServer(t)
	worker := armedWorker(t, s.clusterHandlers.advertiseURL)
	window, _ := worker.PairingWindowInfoSnapshot()
	result := postEnrollment(t, s, pairingAcceptRequest{Code: window.Code, WorkerURL: window.WorkerURL, WorkerFingerprint: s.cluster.listener.CAFingerprint()}, TestAdminKey)
	require.Equal(t, 400, result.Code, result.Body.String())
	require.Equal(t, clusternode.Unclaimed, worker.Mode())
}
func TestClusterPairingAccept_RejectsInvalidAdvertiseURL(t *testing.T) {
	s := coordTestServer(t)
	worker := armedWorker(t, s.clusterHandlers.advertiseURL)
	window, _ := worker.PairingWindowInfoSnapshot()
	s.clusterHandlers.advertiseURL = "coordinator-without-scheme"
	result := postEnrollment(t, s, pairingAcceptRequest{Code: window.Code, WorkerURL: window.WorkerURL, WorkerFingerprint: window.Fingerprint}, TestAdminKey)
	require.Equal(t, 503, result.Code, result.Body.String())
	require.Equal(t, clusternode.Unclaimed, worker.Mode())
}
