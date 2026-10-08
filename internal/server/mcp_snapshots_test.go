package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPSnapshotCatalogModes(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		allowed            bool
	}{
		{"POST", "/v1/chat/completions", `{"stream":false}`, true},
		{"POST", "/v1/chat/completions", `{"stream":true}`, false},
		{"POST", "/v1/chat/completions", `{}`, false},
		{"POST", "/v1/chat/completions", `{"stream":null}`, false},
		{"POST", "/v1/chat/completions", `{"stream":"false"}`, false},
		{"POST", "/v1/chat/completions", `not json`, false},
		{"GET", "/v1/chat/completions", `{"stream":false}`, false},
		{"POST", "/v1/completions", `{"stream":false}`, false},
		{"GET", "/zzrouter/v1/runs/r/logs?follow=false", "", true},
		{"GET", "/zzrouter/v1/runs/r/logs?follow=true", "", false},
		{"GET", "/zzrouter/v1/runs/r/logs", "", false},
		{"POST", "/zzrouter/v1/runs/r/logs?follow=false", "", false},
		{"GET", "/zzrouter/v1/jobs/j/stream?follow=false", "", false},
		{"GET", "/zzrouter/v1/inference-logs/stream?follow=false", "", false},
		{"GET", "/zzrouter/v1/internal/runs/r/logs?follow=false", "", false},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			r, err := http.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			require.NoError(t, err)
			require.Equal(t, tc.allowed, snapshotAllowed(r))
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, tc.body, string(body), "snapshot check consumed the dispatched request")
		})
	}
	require.Empty(t, streamingForPath(http.MethodGet, "/zzrouter/v1/server/logs"))
}

func TestIntegrationMCPRunLogSnapshot(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	id := newLogsTestInstance(t, s, "mcp-log-snapshot", []string{"first", "last"})
	r := mcpWireRequest(t, "tools/call", "2026-07-28", map[string]any{"name": "zz_logs", "arguments": map[string]any{"run_id": id, "lines": 1}})
	result := mcpWireResponse(t, s, r, http.StatusOK)["result"].(map[string]any)
	require.NotEqual(t, true, result["isError"], result)
	data := result["structuredContent"].(map[string]any)
	require.Equal(t, []any{"last"}, data["lines"])
	require.EqualValues(t, 2, data["total_lines"])
	require.NotEmpty(t, data["log_file"])
	require.NotContains(t, data, "truncated")
	require.NotContains(t, data, "count")
}

func TestIntegrationMCPUpdateDefaultStatus(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	installTestUpdateOwner(t, s)
	r := mcpWireRequest(t, "tools/call", "2026-07-28", map[string]any{"name": "zz_update_status", "arguments": map[string]any{}})
	result := mcpWireResponse(t, s, r, http.StatusOK)["result"].(map[string]any)
	require.NotEqual(t, true, result["isError"], result)
	data := result["structuredContent"].(map[string]any)
	require.Contains(t, data, "state")
	require.Contains(t, data, "current_version")
}
