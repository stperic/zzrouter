package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/jobs"
	provapps "github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stretchr/testify/require"
)

func TestProviderTemplateQuery(t *testing.T) {
	h := newProviderModelHarness(t)
	var reads atomic.Int32
	// Synthetic dates, missing metadata and mixed case exercise the query boundaries.
	installTemplateSource(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		_, _ = io.WriteString(w, `[{"templates":[
		{"name":"z","title":"Alpha","date":"2026-11-06","openSource":true},
		{"name":"b","title":"beta","date":"2026-11-05","openSource":false},
		{"name":"a","title":"Beta","date":"2026-11-05","openSource":true},
		{"name":"old","title":"Old","date":"2026-10-13","openSource":true},
		{"name":"unknown","title":"Unknown"}]}]`)
	}))
	for _, tc := range []struct {
		query string
		ids   []string
		total int
		more  bool
	}{
		{"", []string{"a", "b", "old", "unknown", "z"}, 5, false},
		{"&sort=date&order=desc&limit=2", []string{"z", "a"}, 5, true},
		{"&sort=date&order=desc&limit=1&offset=2", []string{"b"}, 5, true},
		{"&sort=date&order=asc", []string{"old", "a", "b", "z", "unknown"}, 5, false},
		{"&sort=date&order=desc", []string{"z", "a", "b", "old", "unknown"}, 5, false},
		{"&sort=title&order=asc", []string{"z", "a", "b", "old", "unknown"}, 5, false},
		{"&sort=id&order=desc&limit=1", []string{"z"}, 5, true},
		{"&since=2026-11-05&open_source=true&sort=date&order=desc&limit=1&offset=1", []string{"a"}, 2, false},
		{"&open_source=false", []string{"b"}, 1, false},
		{"&q=BETA&search=BETA&sort=date&order=desc", []string{"a", "b"}, 2, false},
		{"&since=2026-11-07", []string{}, 0, false},
		{"&offset=20", []string{}, 5, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			r := h.request("GET", "studio/templates?node=worker"+tc.query, "", "")
			require.Equal(t, 200, r.Code, r.Body.String())
			var out struct {
				Data  []provapps.WorkflowTemplate `json:"data"`
				Total int                         `json:"total"`
				More  bool                        `json:"has_more"`
			}
			require.NoError(t, json.Unmarshal(r.Body.Bytes(), &out))
			ids := make([]string, 0, len(out.Data))
			for _, entry := range out.Data {
				ids = append(ids, entry.ID)
			}
			require.Equal(t, tc.ids, ids)
			require.Equal(t, tc.total, out.Total)
			require.Equal(t, tc.more, out.More)
			require.NotNil(t, out.Data)
		})
	}
	before := reads.Load()
	for _, query := range []string{"sort=likes", "order=down", "since=2026-02-30", "since=2026-11-05T00:00:00Z", "open_source=maybe", "open_source=", "sort=", "order=", "since=", "sort=date&sort=id", "open_source=true&open_source=false", "q=a&search=b", "q=a&q=b"} {
		r := h.request("GET", "remote/templates?node=worker&"+query, "", "")
		require.Equal(t, 400, r.Code, r.Body.String())
		require.NotContains(t, r.Body.String(), "provider_model_store_unsupported", "validate before the worker store read")
	}
	require.Equal(t, before, reads.Load(), "invalid queries do not fetch upstream")
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload))

	server := templateMCPServer(h)
	result := callMCPWireTool(t, server, "zz_api_get", map[string]any{"path": "/zzrouter/v1/providers/studio/templates", "query": map[string]string{"node": "worker", "sort": "date", "order": "desc", "limit": "1", "open_source": "true"}})
	require.NotEqual(t, true, result["isError"], result)
	raw, err := json.Marshal(result["structuredContent"])
	require.NoError(t, err)
	require.Contains(t, string(raw), `"id":"z"`)
	require.Contains(t, string(raw), `"date":"2026-11-06"`)
	require.NotContains(t, string(raw), `"id":"a"`)
	for _, operation := range []string{"listProviderTemplates", "getSearch"} {
		ops, err := searchOpenAPI(operation, 10)
		require.NoError(t, err)
		require.Len(t, ops, 1)
		names := []string{}
		for _, p := range ops[0]["parameters"].([]map[string]any) {
			names = append(names, p["name"].(string))
		}
		for _, name := range []string{"q", "search", "sort", "order", "limit", "offset"} {
			require.Contains(t, names, name)
		}
		require.Contains(t, ops[0]["summary"], "sort=date&order=desc&limit=10")
	}
}
