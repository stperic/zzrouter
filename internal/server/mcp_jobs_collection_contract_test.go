package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stretchr/testify/require"
)

func TestMCPJobsCollectionContract(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for range 3 {
		h, err := s.jobs.Start(t.Context(), jobs.KindRun, "test-agent", nil)
		require.NoError(t, err)
		h.Done()
	}
	total := len(s.jobs.List(""))
	for _, version := range []string{"2026-07-28", "2025-06-18"} {
		t.Run(version, func(t *testing.T) {
			seen := map[string]bool{}
			for offset := range total {
				result := mcpWireResponse(t, s, mcpWireRequest(t, "tools/call", version, map[string]any{"name": "zz_job", "arguments": map[string]any{"limit": 1, "offset": offset}}), http.StatusOK)["result"].(map[string]any)
				require.NotEqual(t, true, result["isError"], result)
				data := result["structuredContent"].(map[string]any)
				require.EqualValues(t, total, data["total"])
				require.Equal(t, offset < total-1, data["has_more"])
				require.Equal(t, "test-host", data["node"])
				items := data["jobs"].([]any)
				require.Len(t, items, 1)
				item := items[0].(map[string]any)
				if item["state"] == "done" {
					require.NotEmpty(t, item["expires_at"])
				}
				id := item["job_id"].(string)
				require.False(t, seen[id], fmt.Sprintf("job %s appeared twice", id))
				seen[id] = true
			}
			require.Len(t, seen, total)
		})
	}
	result := mcpWireResponse(t, s, mcpWireRequest(t, "tools/call", "2026-07-28", map[string]any{"name": "zz_job", "arguments": map[string]any{"wait_seconds": 1}}), http.StatusOK)["result"].(map[string]any)
	require.Equal(t, true, result["isError"])
	problem, ok := result["structuredContent"].(map[string]any)
	require.True(t, ok, result)
	require.EqualValues(t, http.StatusBadRequest, problem["status"])
}

func TestMCPJobExpiryAndKindHints(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, tc := range []struct {
		kind jobs.Kind
		tool string
	}{
		{jobs.KindInstall, "zz_providers"}, {jobs.KindService, "zz_providers"},
		{jobs.KindDownload, "zz_models"}, {jobs.KindRun, "zz_runs"}, {jobs.KindUpdate, "zz_update_status"},
		{jobs.KindSync, ""}, {jobs.KindInferenceLog, ""}, {jobs.Kind("unknown"), ""},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			h, err := s.jobs.StartDetached(tc.kind, "", nil)
			require.NoError(t, err)
			h.Done()
			call := func(id string) map[string]any {
				return mcpWireResponse(t, s, mcpWireRequest(t, "tools/call", "2026-07-28", map[string]any{"name": "zz_job", "arguments": map[string]any{"job_id": id}}), http.StatusOK)["result"].(map[string]any)
			}
			result := call(h.ID())
			require.NotEqual(t, true, result["isError"])
			data := result["structuredContent"].(map[string]any)
			require.NotEmpty(t, data["expires_at"])
			slash := strings.IndexByte(h.ID(), '_')
			result = call(h.ID()[:slash+1] + "gone")
			require.Equal(t, true, result["isError"])
			problem := result["structuredContent"].(map[string]any)
			require.Equal(t, "job_not_found", problem["code"])
			issue := problem["errors"].([]any)[0].(map[string]any)
			text := result["content"].([]any)[0].(map[string]any)["text"].(string)
			hint := issue["hint"].(string)
			require.Contains(t, text, hint)
			if tc.tool != "" {
				require.Contains(t, hint, tc.tool)
			} else {
				require.NotContains(t, hint, "zz_")
			}
		})
	}
}
