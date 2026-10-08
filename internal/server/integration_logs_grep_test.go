package server

// Integration test for GET /zzrouter/v1/runs/:id/logs with server-side
// grep. Spins up a full test server, registers a fake instance whose
// LogFilePath points at a file under the log manager's base directory,
// writes a mix of matching and non-matching lines, and verifies the
// response shape.
//
// Scope: the core grep roundtrip. Cluster query forwarding
// (routeInstanceLogsToCluster) and the inference-logs SSE done/error
// wire-format are covered by their own unit-level tests and would
// require a multi-node harness to test end-to-end — deliberately out
// of scope here.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

// newLogsTestInstance builds and registers a stopped instance whose
// log file lives under the server's log manager base dir, and writes
// the given lines to that file. Returns the run ID.
func newLogsTestInstance(t *testing.T, server *Server, id string, lines []string) string {
	t.Helper()
	require.NotNil(t, server.providers.appMgr, "test server must have providerAppMgr")

	baseDir := server.providers.appMgr.LogManager().GetBaseDir()
	require.NotEmpty(t, baseDir, "log manager base dir must be non-empty")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))

	logPath := filepath.Join(baseDir, id+".log")
	require.NoError(t, os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	inst := instance.NewInstance(id, "mlx", "test-model-"+id, 0, 0, 64)
	inst.SetLogFilePath(logPath)
	// Put the instance in a terminal state so the handler's "wait for
	// log file to appear" polling short-circuits — the file already
	// exists on disk.
	inst.SetStatus(instance.StatusStopped)

	require.NoError(t, server.providers.appMgr.Instances().Register(inst))
	return id
}

// TestIntegration_RunLogsGrepRoundtrip drives the full request path:
// compile filter → serve file → logFilter.Apply → respondSuccess.
func TestIntegration_RunLogsGrepRoundtrip(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	id := newLogsTestInstance(t, server, "it-grep-1", []string{
		"startup ok",
		"request 1",
		"ERROR: boom",
		"request 2",
		"error again",
		"done",
	})

	resp := makeAuthRequest(t, server, "GET",
		"/zzrouter/v1/runs/"+id+"/logs?grep=error",
		TestAdminKey, nil)
	require.Equal(t, 200, resp.Code, "body=%s", string(resp.Body))

	var env struct {
		Success bool `json:"success"`
		Data    struct {
			Lines      []string `json:"lines"`
			Count      int      `json:"count"`
			TotalLines int      `json:"total_lines"`
			Truncated  bool     `json:"truncated"`
			Filtered   bool     `json:"filtered"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))
	require.True(t, env.Success, "success=false, body=%s", string(resp.Body))
	require.True(t, env.Data.Filtered, "want filtered=true")
	require.False(t, env.Data.Truncated, "want truncated=false")
	// Both "ERROR: boom" and "error again" match (case-insensitive
	// grep via the (?i)+QuoteMeta compile path).
	require.Equal(t, []string{"ERROR: boom", "error again"}, env.Data.Lines)
	require.Equal(t, 2, env.Data.Count)
	// total_lines is the size of the pre-filter input.
	require.Equal(t, 6, env.Data.TotalLines)
}

// TestIntegration_RunLogsGrepNoMatches verifies that a query that
// matches nothing still returns 200 with filtered=true and an empty
// lines array — agents rely on that shape to distinguish "no matches"
// from "server error".
func TestIntegration_RunLogsGrepNoMatches(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	id := newLogsTestInstance(t, server, "it-grep-2", []string{
		"hello world",
		"another line",
	})

	resp := makeAuthRequest(t, server, "GET",
		"/zzrouter/v1/runs/"+id+"/logs?grep=nosuchword",
		TestAdminKey, nil)
	require.Equal(t, 200, resp.Code, "body=%s", string(resp.Body))

	var env struct {
		Success bool `json:"success"`
		Data    struct {
			Lines     []string `json:"lines"`
			Count     int      `json:"count"`
			Truncated bool     `json:"truncated"`
			Filtered  bool     `json:"filtered"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))
	require.True(t, env.Success)
	require.True(t, env.Data.Filtered)
	require.False(t, env.Data.Truncated)
	require.Equal(t, 0, env.Data.Count)
	require.Empty(t, env.Data.Lines)
}

// TestIntegration_RunLogsBothGrepAndRegex verifies 400 on mutually
// exclusive filters — compiler-level rejection surfaced as a bad
// request at the HTTP boundary.
func TestIntegration_RunLogsBothGrepAndRegex(t *testing.T) {
	server := createTestNodeWithDefaults(t)
	id := newLogsTestInstance(t, server, "it-grep-3", []string{"x"})

	resp := makeAuthRequest(t, server, "GET",
		"/zzrouter/v1/runs/"+id+"/logs?grep=x&regex=.%2A",
		TestAdminKey, nil)
	require.Equal(t, 400, resp.Code, "body=%s", string(resp.Body))
}
