package server

import (
	"fmt"
	"net/http"
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
				id := items[0].(map[string]any)["job_id"].(string)
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
