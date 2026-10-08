package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// getJSON issues an authenticated GET and decodes the standard envelope.
func getJSON(t *testing.T, base, path string) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", TestAdminKey)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var envelope map[string]any
	if len(body) > 0 && resp.Header.Get("Content-Type") != "" {
		_ = json.Unmarshal(body, &envelope)
	}
	return resp.StatusCode, envelope
}

// seedTrackedProvider installs a provider carrying a version_source into the
// server's live config. The route closure reads s.appsConfig on every request,
// so seeding after construction is enough.
func seedTrackedProvider(t *testing.T, s *Server) {
	t.Helper()

	cfg := s.appsConfig
	if cfg == nil {
		cfg = &pkgConfig.AppsConfig{}
		s.appsConfig = cfg
	}
	require.NoError(t, cfg.AddApp("llamacpp", pkgConfig.ServiceConfig{
		Protocol:      pkgConfig.ProtocolOpenAI,
		Mode:          "on-demand",
		PinnedVersion: "b10453",
		VersionSource: &pkgConfig.VersionSource{
			Type:        pkgConfig.VersionSourceGitHubRelease,
			Repo:        "ggml-org/llama.cpp",
			StripPrefix: "b",
			Compare:     pkgConfig.CompareBuildNumber,
		},
		Runtime: &pkgConfig.AppRuntimeConfig{
			BasePort:  8080,
			PortRange: []int{8080, 8089},
			Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "llama-server"},
		},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	// No outbound calls from the test suite.
	disabled := false
	cfg.Settings.Updates.ProviderVersionChecks = &disabled
}

// hasRoute reports whether the engine registered a path.
func hasRoute(s *Server, method, path string) bool {
	for _, r := range s.engine.Routes() {
		if r.Method == method && r.Path == path {
			return true
		}
	}
	return false
}

// The endpoint must be reachable on a coordinator and must answer even when
// no upstream can be consulted: that is a reportable state, not a 5xx, and
// the status must never read "same".
func TestProviderVersionsEndpoint(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	seedTrackedProvider(t, s)

	httpNode := httptest.NewServer(s.engine)
	defer closeHTTPTestServer(httpNode)

	status, envelope := getJSON(t, httpNode.URL, "/zzrouter/v1/providers/llamacpp/versions")
	require.Equal(t, http.StatusOK, status, "an unreachable upstream must not fail the request")

	data, ok := envelope["data"].(map[string]any)
	require.True(t, ok, "envelope: %v", envelope)

	assert.Equal(t, "llamacpp", data["provider"])
	assert.Equal(t, "b10453", data["pinned"])

	// Checks are disabled in this fixture, so the honest answer is unknown
	// with a stated reason — never a silent "same".
	assert.Equal(t, "unknown", data["pinned_status"])
	assert.Contains(t, data["reason"], "disabled")
	assert.Empty(t, data["latest"])
	assert.Contains(t, data, "nodes")
}

func TestProviderVersionsEndpointUnknownProvider(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	seedTrackedProvider(t, s)

	httpNode := httptest.NewServer(s.engine)
	defer closeHTTPTestServer(httpNode)

	status, _ := getJSON(t, httpNode.URL, "/zzrouter/v1/providers/definitely-not-a-provider/versions")
	assert.Equal(t, http.StatusNotFound, status)
}

func TestProviderVersionsEndpointRequiresAuth(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	seedTrackedProvider(t, s)

	httpNode := httptest.NewServer(s.engine)
	defer closeHTTPTestServer(httpNode)

	resp, err := http.Get(httpNode.URL + "/zzrouter/v1/providers/llamacpp/versions")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// A coordinator registers the route; the worker case below is the contrast.
func TestProviderVersionsRouteRegisteredOnCoordinator(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	assert.True(t, hasRoute(s, http.MethodGet, "/zzrouter/v1/providers/:name/versions"))
}

// Workers do not serve the versions route: the check is one outbound call for
// the cluster, made coordinator-side, not one per worker.
func TestProviderVersionsEndpointAbsentOnWorker(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{
		AdminKey:    TestAdminKey,
		ClusterKey:  TestClusterKey,
		ClusterMode: pkgConfig.ClusterModeWorker,
	})

	// Assert on route registration rather than on a status code: a worker
	// 404s unknown providers too, so a bare 404 would not prove the route is
	// absent.
	assert.False(t, hasRoute(s, http.MethodGet, "/zzrouter/v1/providers/:name/versions"),
		"worker must not register the versions route")
}
