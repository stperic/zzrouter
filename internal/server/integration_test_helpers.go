// Package server provides integration test helpers for the zzrouter host server.
// This file contains common test utilities and server factory functions.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stretchr/testify/require"
)

// Integration test keys. Role names are "coord/reader/peer" instead of
// admin/user/cluster so the strings don't trip pkgConfig.ValidateAPIKey's
// weak-pattern check.
const (
	TestAdminKey   = "mx-zzr-it-coord-Kp9vB4xRmT2LqJfNdY"
	TestUserKey    = "mx-zzr-it-reader-Hw5nQ8yVcPj3LsBtXk"
	TestClusterKey = "mx-zzr-it-peer-Fg7rZxMb6kDpC2NvHw"
)

// testNodePort tracks the next available port for test servers.
// Uses atomic operations for thread-safe access in parallel tests.
var testNodePort int64 = 9200

// getNextTestPort returns the next available port for test servers.
// Thread-safe via atomic increment.
func getNextTestPort() int {
	return int(atomic.AddInt64(&testNodePort, 1) - 1)
}

// TestNodeConfig contains configuration options for creating test servers.
type TestNodeConfig struct {
	// AdminKey is the admin API key. Empty = no admin auth configured.
	AdminKey string
	// UserKey is the user API key. Empty = no user auth configured.
	UserKey string
	// ClusterKey is the cluster API key. Empty = no cluster auth configured.
	ClusterKey string
	// ClusterMode is the cluster mode (disabled, master, worker).
	ClusterMode pkgConfig.ClusterMode
	// NodeName overrides the default "test-host" name. Used by multi-node
	// tests that need distinguishable identities on master vs worker.
	NodeName string
	// SeedProvidersDir installs the default providers/ tree in the test
	// home before the server is built (an empty dir is rejected by the
	// loader), so NewAppsConfigStoreFromStandardLocations finds it and
	// the server gets a non-nil configStore. Routes gated on configStore
	// (POST /providers/instances) register only when this is set.
	SeedProvidersDir bool
}

// DefaultTestNodeConfig returns a test server config with default values.
func DefaultTestNodeConfig() TestNodeConfig {
	return TestNodeConfig{
		AdminKey:    TestAdminKey,
		ClusterMode: pkgConfig.ClusterModeDisabled,
	}
}

// BuildTestServer is the testing.T-free core of createTestNode. Returns
// the constructed *Server, a cleanup function the caller must invoke
// (defer or t.Cleanup), and any construction error.
//
// Reused by the e2e harness in-process backend (test/e2e/harness/inproc)
// so the harness library can stand up real Server instances without
// taking a transitive dep on testing.T.
//
// CONCURRENCY CONSTRAINT: BuildTestServer mutates the process-global
// ZZROUTER_TEST_HOME env var (PathResolver reads it lazily for every
// path lookup). Callers MUST serialize concurrent BuildTestServer
// invocations within a single process — overlapping calls cross-poison
// each other's path roots. The harness inproc backend serializes via a
// per-Backend mutex; integration tests within this package serialize
// via Go's default per-package test scheduling.
//
// Caller responsibilities:
//   - Invoke cleanup() exactly once. It tears down background goroutines,
//     closes idle connections, and removes the temp config dir.
//   - BuildTestServer always allocates its own temp home and overwrites
//     ZZROUTER_TEST_HOME for the server's lifetime; a pre-set value is
//     saved and restored on cleanup, not honored.
func BuildTestServer(cfg TestNodeConfig) (*Server, func(), error) {
	testHome, err := os.MkdirTemp("", "zzrouter-test-")
	if err != nil {
		return nil, nil, fmt.Errorf("create test home: %w", err)
	}
	prevHome, hadHome := os.LookupEnv("ZZROUTER_TEST_HOME")
	if err := os.Setenv("ZZROUTER_TEST_HOME", testHome); err != nil { // lint:allow os.Setenv
		_ = os.RemoveAll(testHome)
		return nil, nil, fmt.Errorf("set ZZROUTER_TEST_HOME: %w", err)
	}

	if cfg.SeedProvidersDir {
		dir := pkgConfig.NewConfigManager("zzrouter").GetAppsConfigDir()
		if err := templates.InstallDefaults(dir); err != nil {
			_ = os.RemoveAll(testHome)
			if hadHome {
				_ = os.Setenv("ZZROUTER_TEST_HOME", prevHome) // lint:allow os.Setenv
			} else {
				_ = os.Unsetenv("ZZROUTER_TEST_HOME")
			}
			return nil, nil, fmt.Errorf("seed providers dir: %w", err)
		}
	}

	name := cfg.NodeName
	if name == "" {
		name = "test-host"
	}
	hostCfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: getNextTestPort(),
			Name: name,
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: cfg.ClusterMode,
		},
		Auth: pkgConfig.AuthConfig{
			AdminKey:          cfg.AdminKey,
			UserKey:           cfg.UserKey,
			ClusterNetworkKey: cfg.ClusterKey,
		},
	}
	server, err := NewServerWithOptions(hostCfg)
	if err != nil {
		_ = os.RemoveAll(testHome)
		if hadHome {
			_ = os.Setenv("ZZROUTER_TEST_HOME", prevHome) // lint:allow os.Setenv
		} else {
			_ = os.Unsetenv("ZZROUTER_TEST_HOME")
		}
		return nil, nil, fmt.Errorf("create server: %w", err)
	}
	if server.engine == nil {
		cleanupTestNode(server)
		_ = os.RemoveAll(testHome)
		return nil, nil, fmt.Errorf("test server engine is nil")
	}

	cleanup := func() {
		cleanupTestNode(server)
		_ = os.RemoveAll(testHome)
		if hadHome {
			_ = os.Setenv("ZZROUTER_TEST_HOME", prevHome) // lint:allow os.Setenv
		} else {
			_ = os.Unsetenv("ZZROUTER_TEST_HOME")
		}
	}
	return server, cleanup, nil
}

// createTestNode creates a test server with the given configuration.
// The server is not started; use server.engine.ServeHTTP for requests.
// Cleanup is registered via t.Cleanup; do not invoke manually.
func createTestNode(t *testing.T, cfg TestNodeConfig) *Server {
	t.Helper()
	server, cleanup, err := BuildTestServer(cfg)
	require.NoError(t, err, "build test server")
	t.Cleanup(cleanup)
	return server
}

// cleanupTestNode cleans up test server resources to prevent goroutine leaks.
// Stops every subsystem we know spawns background goroutines that outlive
// shutdownCancel; un-stopped subsystems show up in goleak ignore lists
// (see test/e2e/ring1_contract/contract_test.go). When a leak appears in
// CI, the right move is to add the Stop() call here rather than expand
// the ignore set.
func cleanupTestNode(server *Server) {
	if server == nil {
		return
	}

	// Cancel shutdown context to stop background goroutines that observe it.
	if server.shutdownCancel != nil {
		server.shutdownCancel()
	}

	// Stop subsystems (nil-receiver safe).
	_ = server.providers.appMgr.Stop(context.Background())
	server.providers.cooldowns.Stop()
	server.access.Stop(context.Background())

	// jobs.Registry runs a janitor goroutine that doesn't observe shutdownCtx
	// — it owns its own lifecycle. Stop it explicitly so test runs don't leak.
	if server.jobs != nil {
		server.jobs.Stop()
	}

	// Close idle connections on server's HTTP clients to prevent connection pool leaks
	if server.httpClient != nil {
		if transport, ok := server.httpClient.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	if server.httpStreamingClient != nil {
		if transport, ok := server.httpStreamingClient.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
}

// createTestNodeWithDefaults creates a test server with default configuration.
func createTestNodeWithDefaults(t *testing.T) *Server {
	t.Helper()
	return createTestNode(t, DefaultTestNodeConfig())
}

// createTestNodeWithModelsDir creates a test server and overrides the global
// models root dir for the lifetime of the test. NOTE: the override is global,
// so tests running multiple servers with different roots only see the most
// recent override.
func createTestNodeWithModelsDir(t *testing.T, cfg TestNodeConfig, modelsDir, serverName string) *Server {
	t.Helper()

	modelregistry.SetModelsRootDirOverride(modelsDir)
	t.Cleanup(func() {
		modelregistry.ClearModelsRootDirOverride()
	})

	cfg.NodeName = serverName
	return createTestNode(t, cfg)
}

// TestRequest represents an HTTP request for testing.
type TestRequest struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    any
}

// TestResponse contains the response from a test request.
type TestResponse struct {
	Code    int
	Body    []byte
	Headers http.Header
}

// makeRequest executes an HTTP request against the test server.
// Uses t.Fatal for errors instead of panic for proper test failure reporting.
func makeRequest(t *testing.T, server *Server, req TestRequest) *TestResponse {
	t.Helper()

	var bodyReader io.Reader
	if req.Body != nil {
		bodyBytes, err := json.Marshal(req.Body)
		require.NoError(t, err, "failed to marshal request body")
		bodyReader = bytes.NewReader(bodyBytes)
	}

	httpReq := httptest.NewRequest(req.Method, req.Path, bodyReader)
	if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}

	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, httpReq)

	return &TestResponse{
		Code:    w.Code,
		Body:    w.Body.Bytes(),
		Headers: w.Header(),
	}
}

// makeAuthRequest executes an HTTP request with an API key.
func makeAuthRequest(t *testing.T, server *Server, method, path, apiKey string, body any) *TestResponse {
	t.Helper()

	headers := map[string]string{}
	if apiKey != "" {
		headers["X-API-Key"] = apiKey
	}

	return makeRequest(t, server, TestRequest{
		Method:  method,
		Path:    path,
		Headers: headers,
		Body:    body,
	})
}

// assertStatusOneOf asserts that the response status code is one of the expected values.
// Use this for endpoints where multiple status codes are valid (e.g., optional features
// that may return 400 or 501, or resources that may or may not exist).
func assertStatusOneOf(t *testing.T, resp *TestResponse, expected []int, msgAndArgs ...any) {
	t.Helper()

	if slices.Contains(expected, resp.Code) {
		return
	}

	msg := fmt.Sprintf("expected one of %v, got %d", expected, resp.Code)
	if len(msgAndArgs) > 0 {
		format, _ := msgAndArgs[0].(string)
		msg = fmt.Sprintf(format, msgAndArgs[1:]...) + ": " + msg
	}
	t.Error(msg)
}

// assertStatusBadRequestOrNotImplemented asserts that the response is either
// 400 Bad Request or 501 Not Implemented. Use for optional features that may
// not be implemented yet.
func assertStatusBadRequestOrNotImplemented(t *testing.T, resp *TestResponse, feature string) {
	t.Helper()
	assertStatusOneOf(t, resp, []int{http.StatusBadRequest, http.StatusNotImplemented},
		"feature %q", feature)
}

// assertStatusBadRequestOrNotFound asserts that the response is either
// 400 Bad Request or 404 Not Found. Use for endpoints where the distinction
// between "invalid request" and "resource not found" varies by implementation.
func assertStatusBadRequestOrNotFound(t *testing.T, resp *TestResponse, resource string) {
	t.Helper()
	assertStatusOneOf(t, resp, []int{http.StatusBadRequest, http.StatusNotFound},
		"resource %q", resource)
}

// testHTTPClient returns an HTTP client configured for tests.
// It disables keep-alives to prevent goroutine leaks from connection pooling.
func testHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
}

// closeHTTPTestServer properly closes an httptest.Server by first closing all
// client connections, then closing the server itself. This prevents goroutine
// leaks from lingering connection reader goroutines (connReader.backgroundRead).
// Usage: Replace `defer server.Close()` with `defer closeHTTPTestServer(server)`
func closeHTTPTestServer(server *httptest.Server) {
	if server == nil {
		return
	}
	// CloseClientConnections forcibly closes all tracked client connections,
	// which causes server-side connection reader goroutines to terminate.
	server.CloseClientConnections()
	server.Close()
}

// init sets Gin to test mode for all tests in this package.
func init() {
	gin.SetMode(gin.TestMode)
}

// TestDownloadTracker exposes the server's internal DownloadTracker for tests
// that need to seed live download state (e.g., to verify mergeProgress merges
// authoritative per-node progress into tracked Deployments). Returns nil when
// the tracker has not been initialized on this server instance.
func TestDownloadTracker(s *Server) *modelregistry.DownloadTracker {
	if s == nil {
		return nil
	}
	return s.model.Downloads
}

// TestDeploymentsService exposes the server's DeploymentsService so sibling
// test packages can exercise the service API directly.
func TestDeploymentsService(s *Server) *DeploymentsService {
	if s == nil {
		return nil
	}
	return s.services.Deployments
}

// makeInternalHTTPServer wraps the server's internal engine in an
// httptest.NewServer so tests that need to fetch `/zzrouter/v1/internal/*`
// over real HTTP (e.g. for Range requests, real *http.Client behavior,
// streaming reads) can do so without going through mTLS. Returns the
// httptest.Server; callers register `t.Cleanup(srv.Close)` themselves
// or use closeHTTPTestServer to kill keepalive goroutines.
//
// In production this engine sits behind clusternode's mTLS listener;
// the OU=coordinator client cert is the auth boundary. The harness
// strips that boundary because a single test process is the trust
// boundary instead.
func makeInternalHTTPServer(t *testing.T, server *Server) *httptest.Server {
	t.Helper()
	return httptest.NewServer(buildInternalEngine(server))
}

// makeInternalRequest drives a request against the server's internal
// engine — the /zzrouter/v1/internal/* routes that production mounts
// on the mTLS cluster-port listener. Tests reach those routes by
// building the engine on-demand and serving the request directly; no
// TLS handshake is performed, so the transport-layer mTLS auth is
// bypassed. Use this for any test that used to set
// X-Cluster-API-Key on the public /internal/* mount.
func makeInternalRequest(t *testing.T, server *Server, req TestRequest) *TestResponse {
	t.Helper()

	engine := buildInternalEngine(server)

	var bodyReader io.Reader
	if req.Body != nil {
		bodyBytes, err := json.Marshal(req.Body)
		require.NoError(t, err, "failed to marshal request body")
		bodyReader = bytes.NewReader(bodyBytes)
	}

	httpReq := httptest.NewRequest(req.Method, req.Path, bodyReader)
	if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httpReq)

	return &TestResponse{
		Code:    w.Code,
		Body:    w.Body.Bytes(),
		Headers: w.Header(),
	}
}

// Public aliases for use by sibling test packages (notably
// internal/server/integration). The lowercase originals stay in place so
// existing in-package tests are not churned. Cleanup of the duplicate
// names can happen once all callers have migrated.
var (
	NewTestNode                            = createTestNode
	NewTestNodeWithDefaults                = createTestNodeWithDefaults
	CleanupTestNode                        = cleanupTestNode
	GetNextTestPort                        = getNextTestPort
	MakeRequest                            = makeRequest
	MakeAuthRequest                        = makeAuthRequest
	MakeInternalRequest                    = makeInternalRequest
	MakeInternalHTTPServer                 = makeInternalHTTPServer
	AssertStatusOneOf                      = assertStatusOneOf
	AssertStatusBadRequestOrNotImplemented = assertStatusBadRequestOrNotImplemented
	AssertStatusBadRequestOrNotFound       = assertStatusBadRequestOrNotFound
	TestHTTPClient                         = testHTTPClient
	CloseHTTPTestServer                    = closeHTTPTestServer
)
