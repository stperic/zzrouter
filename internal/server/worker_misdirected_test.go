package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorkerMisdirected_PublicManagementReturns421 asserts that a
// /zzrouter/v1/providers hit on a worker-mode node returns 421
// Misdirected Request with Problem Details instead of a bare 404 —
// gives operators a pointer to fix their URL without grepping config.
func TestWorkerMisdirected_PublicManagementReturns421(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{
		AdminKey:    TestAdminKey,
		ClusterKey:  TestClusterKey,
		ClusterMode: pkgConfig.ClusterModeWorker,
	})

	httpNode := httptest.NewServer(s.engine)
	defer closeHTTPTestServer(httpNode)

	req, err := http.NewRequest("PATCH", httpNode.URL+"/zzrouter/v1/providers/vllm", strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMisdirectedRequest, resp.StatusCode,
		"worker public-API hit should 421, not 404")
	assert.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))
}

// TestWorkerMisdirected_InternalPathsFallThrough — /internal/* and
// /cluster/* are legitimately served on workers (over mTLS and for
// join/leave respectively); misses on those paths must fall through
// to the regular 404 handler, not the 421 redirect.
func TestWorkerMisdirected_InternalPathsFallThrough(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{
		AdminKey:    TestAdminKey,
		ClusterKey:  TestClusterKey,
		ClusterMode: pkgConfig.ClusterModeWorker,
	})

	httpNode := httptest.NewServer(s.engine)
	defer closeHTTPTestServer(httpNode)

	for _, path := range []string{
		"/zzrouter/v1/internal/nonexistent",
		"/zzrouter/v1/cluster/nonexistent",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(httpNode.URL + path)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode,
				"worker-served-path miss should 404, not 421")
		})
	}
}

// TestWorkerMisdirected_NonWorkerUnchanged — on a coordinator, misses
// on /zzrouter/v1/* must still get the default 404 Problem Details,
// unchanged from the baseline NoRoute behavior.
func TestWorkerMisdirected_NonWorkerUnchanged(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	httpNode := httptest.NewServer(s.engine)
	defer closeHTTPTestServer(httpNode)

	// Use admin key so we get past the public-API auth middleware and
	// actually hit NoRoute on a coord-mode node.
	req, err := http.NewRequest("GET", httpNode.URL+"/zzrouter/v1/definitely-not-a-route", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", TestAdminKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
