// Unit tests for the MCP transport gateway.
//
// Acceptance bar: the route exists and forwards correctly. For HTTP
// transport we verify bytes round-trip through a test backend. For
// stdio transport we verify the bridge spawns a child process and a
// JSON line is echoed back. Protocol-level MCP semantics (tool
// discovery, capabilities negotiation) are out of scope — zzrouter is
// a transport proxy.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createMCPTestNode builds a zzrouter test server with the given MCP
// server configs pre-wired. Callers pass a ready MCPServerConfig map
// (keyed by mount name) so each test can declare its own transports.
func createMCPTestNode(t *testing.T, servers map[string]pkgConfig.MCPServerConfig) *Server {
	t.Helper()

	hostCfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: getNextTestPort(),
			Name: "test-mcp",
		},
		Cluster: pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeDisabled},
		Auth:    pkgConfig.AuthConfig{AdminKey: TestAdminKey},
		MCP: pkgConfig.MCPConfig{
			Servers: servers,
		},
	}

	server, err := NewServerWithOptions(hostCfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })
	return server
}

// TestMCP_NoServers_Returns404 verifies that without any configured MCP
// servers, the /mcp/* prefix is not registered at all and requests fall
// through to the NoRoute handler.
func TestMCP_NoServers_Returns404(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeRequest(t, server, TestRequest{
		Method: "POST",
		Path:   "/mcp/github/tools/list",
	})
	require.Equal(t, http.StatusNotFound, resp.Code)
}

// TestMCP_HTTPTransport_ForwardsRequest verifies that an HTTP-transport
// MCP mount forwards method/path/body to the configured backend and
// returns the backend's response body unchanged.
func TestMCP_HTTPTransport_ForwardsRequest(t *testing.T) {
	backend, recorded, mu := newBackendRecorder(t,
		http.StatusOK,
		[]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
		"application/json",
	)

	server := createMCPTestNode(t, map[string]pkgConfig.MCPServerConfig{
		"github": {
			Transport: "http",
			URL:       backend.URL,
		},
	})

	resp := makeRequest(t, server, TestRequest{
		Method: "POST",
		Path:   "/mcp/github/messages",
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/list",
		},
	})
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)

	// Backend response must pass through unchanged.
	var env map[string]any
	require.NoError(t, json.Unmarshal(resp.Body, &env))
	assert.Equal(t, float64(1), env["id"])

	// Backend should have seen the POST with the mount prefix stripped.
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *recorded, 1)
	assert.Equal(t, "POST", (*recorded)[0].Method)
	assert.Equal(t, "/messages", (*recorded)[0].Path)
	assert.Contains(t, string((*recorded)[0].Body), `"method":"tools/list"`)
}

// TestMCP_HTTPTransport_PreservesQueryString verifies a GET with query
// string reaches the backend intact. MCP HTTP servers frequently take
// connection/session ids on the URL.
func TestMCP_HTTPTransport_PreservesQueryString(t *testing.T) {
	backend, recorded, mu := newBackendRecorder(t,
		http.StatusOK,
		[]byte(`{"ok":true}`),
		"application/json",
	)

	server := createMCPTestNode(t, map[string]pkgConfig.MCPServerConfig{
		"github": {
			Transport: "http",
			URL:       backend.URL,
		},
	})

	resp := makeRequest(t, server, TestRequest{
		Method: "GET",
		Path:   "/mcp/github/stream?session=abc123",
	})
	require.Equal(t, http.StatusOK, resp.Code)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *recorded, 1)
	assert.Equal(t, "/stream", (*recorded)[0].Path)
}

// TestMCP_MissingURL_SkipsRegistration verifies that a config entry
// with transport=http and no URL is skipped (with a log line) rather
// than crashing startup.
func TestMCP_MissingURL_SkipsRegistration(t *testing.T) {
	server := createMCPTestNode(t, map[string]pkgConfig.MCPServerConfig{
		"broken": {
			Transport: "http",
			// URL intentionally empty
		},
	})

	resp := makeRequest(t, server, TestRequest{
		Method: "POST",
		Path:   "/mcp/broken/tools/list",
	})
	// No routes registered for this mount → NoRoute 404.
	require.Equal(t, http.StatusNotFound, resp.Code)
}

// TestMCP_StdioTransport_RoundTripsOneLine verifies the stdio bridge
// spawns a child process, writes one JSON-RPC request line to its
// stdin, and returns one response line read from its stdout. Uses a
// tiny shell-based echo loop portable across macOS / Linux.
func TestMCP_StdioTransport_RoundTripsOneLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stdio echo loop uses POSIX shell")
	}
	// `sh -c 'while read l; do echo "$l"; done'` reads one line and
	// echoes it back, forever. That's enough to exercise the bridge.
	server := createMCPTestNode(t, map[string]pkgConfig.MCPServerConfig{
		"echo": {
			Transport: "stdio",
			Command:   "sh",
			Args:      []string{"-c", "while IFS= read -r l; do echo \"$l\"; done"},
		},
	})

	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      42,
		"method":  "ping",
	}
	resp := makeRequest(t, server, TestRequest{
		Method: "POST",
		Path:   "/mcp/echo/messages",
		Body:   reqBody,
	})
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)

	// The echo loop returns the exact request body. Parse it back.
	var echoed map[string]any
	require.NoError(t, json.Unmarshal(resp.Body, &echoed),
		"response was not valid JSON: %s", resp.Body)
	assert.Equal(t, "2.0", echoed["jsonrpc"])
	assert.Equal(t, float64(42), echoed["id"])
	assert.Equal(t, "ping", echoed["method"])
}

// TestMCP_StdioTransport_ReusesChildAcrossRequests verifies that two
// requests against the same stdio mount share one child process,
// matching how MCP servers expect to run (long-lived, stateful). We
// confirm by counting spawn attempts through a sentinel file the child
// script writes on startup.
func TestMCP_StdioTransport_ReusesChildAcrossRequests(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stdio script uses POSIX shell")
	}
	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "spawn-count")

	// Script appends an 'x' to sentinel on startup, then enters an
	// echo loop. Counting the file's length after the test gives us
	// spawn attempts.
	script := `echo -n x >> ` + sentinel + `; while IFS= read -r l; do echo "$l"; done`
	server := createMCPTestNode(t, map[string]pkgConfig.MCPServerConfig{
		"counted": {
			Transport: "stdio",
			Command:   "sh",
			Args:      []string{"-c", script},
		},
	})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := makeRequest(t, server, TestRequest{
				Method: "POST",
				Path:   "/mcp/counted/messages",
				Body:   map[string]any{"id": i},
			})
			assert.Equal(t, http.StatusOK, resp.Code)
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(sentinel)
	require.NoError(t, err)
	// Exactly one spawn across 5 requests means the bridge was reused.
	assert.Equal(t, 1, strings.Count(string(data), "x"),
		"expected child process to be spawned exactly once, got %d", strings.Count(string(data), "x"))
}

// An MCP server is a third party: it is sent the credential its mount
// declares, and nothing of the caller's unless the mount says so.
func TestMCP_HTTPTransport_SendsTheDeclaredCredential(t *testing.T) {
	cases := []struct {
		name string
		api  *pkgConfig.APIConfig
		want string // Authorization the MCP server receives
	}{
		{"nothing declared", nil, ""},
		{"own token", &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "gh-token"}, "Bearer gh-token"},
		{"caller's key", &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeCaller}, "Bearer " + TestAdminKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, recorded, mu := newBackendRecorder(t, http.StatusOK, []byte(`{}`), "application/json")
			server := createMCPTestNode(t, map[string]pkgConfig.MCPServerConfig{
				"github": {Transport: "http", URL: backend.URL, API: tc.api},
			})

			resp := makeRequest(t, server, TestRequest{
				Method:  "POST",
				Path:    "/mcp/github/messages",
				Headers: map[string]string{"Authorization": "Bearer " + TestAdminKey, "Content-Type": "application/json"},
				Body:    map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
			})
			require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, *recorded, 1)
			assert.Equal(t, tc.want, (*recorded)[0].Headers.Get("Authorization"))
			assert.Empty(t, (*recorded)[0].Headers.Get("X-API-Key"))
		})
	}
}

// A mount that spends this node's own credential does not do it for a
// caller with no key.
func TestMCP_KeylessCallerCannotSpendTheNodesCredential(t *testing.T) {
	backend, recorded, mu := newBackendRecorder(t, http.StatusOK, []byte(`{}`), "application/json")
	server := createMCPTestNode(t, map[string]pkgConfig.MCPServerConfig{
		"github": {Transport: "http", URL: backend.URL,
			API: &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "gh-token"}},
	})

	resp := makeRequest(t, server, TestRequest{Method: "POST", Path: "/mcp/github/messages", Body: map[string]any{}})

	assert.Equal(t, http.StatusUnauthorized, resp.Code, "body=%s", resp.Body)
	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, *recorded)
}

// A mount forwards tool calls it does not read, so it holds a key to the
// limits that need neither model nor price, and refuses one whose other
// limits would pass unenforced.
func TestMCP_HoldsCallersToTheirLimits(t *testing.T) {
	backend, _, _ := newBackendRecorder(t, http.StatusOK, []byte(`{}`), "application/json")
	server := createMCPTestNode(t, map[string]pkgConfig.MCPServerConfig{
		"github": {Transport: "http", URL: backend.URL},
	})
	call := func(key string) *TestResponse {
		return makeAuthRequest(t, server, http.MethodPost, "/mcp/github/messages", key, map[string]any{"id": 1})
	}
	createKey := func(body map[string]any) string {
		resp := makeAuthRequest(t, server, http.MethodPost, "/zzrouter/v1/keys", TestAdminKey, body)
		require.Equal(t, http.StatusCreated, resp.Code, string(resp.Body))
		var out struct {
			Data struct {
				Key string `json:"key"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(resp.Body, &out))
		return out.Data.Key
	}

	rpmKey := createKey(map[string]any{"id": "rate", "name": "rate", "rpm_limit": 1})
	assert.Equal(t, http.StatusOK, call(rpmKey).Code)
	assert.Equal(t, http.StatusTooManyRequests, call(rpmKey).Code, "request rate is enforced on a mount")

	spend := call(createKey(map[string]any{"id": "spender", "name": "spender", "spend_limit": 5}))
	assert.Equal(t, http.StatusForbidden, spend.Code)
	assert.Contains(t, string(spend.Body), "spend limit")
}
