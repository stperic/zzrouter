package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/modelregistry/search"
	"github.com/stretchr/testify/require"
)

func TestModelSearchQuery(t *testing.T) {
	const provider search.Provider = "query-fixture"
	var reads atomic.Int32
	search.RegisterCloudProvider(provider, search.CloudProviderMeta{}, func(_ context.Context, query string, limit int) ([]any, error) {
		reads.Add(1)
		require.Zero(t, limit, "retrieve the full catalog before sorting and pagination")
		require.Equal(t, "image", query)
		return []any{
			map[string]any{"id": "unknown", "name": "No date"},
			map[string]any{"id": "old", "name": "Alpha", "created_at": "2026-10-01"},
			map[string]any{"id": "new", "name": "Zebra", "created_at": "2026-11-05T00:00:00Z"},
			map[string]any{"id": "middle", "name": "Beta", "created": float64(1791072000)},
		}, nil
	})
	s := NewModelSearchService(nil, nil)
	t.Cleanup(func() { s.Stop(t.Context()) })
	engine := gin.New()
	engine.GET("/search", s.HandleSearchPublic)
	get := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest("GET", "/search?type=models&"+query, nil))
		return w
	}
	for _, tc := range []struct {
		query, id string
		more      bool
	}{
		{"sort=date&order=desc&limit=1&offset=1", "middle", true},
		{"sort=date&order=asc&limit=1", "old", true},
		{"sort=name&order=desc&limit=1", "new", true},
		{"sort=id&order=asc&limit=1&offset=3", "unknown", false},
	} {
		r := get("provider=query-fixture&search=image&" + tc.query)
		require.Equal(t, 200, r.Code, r.Body.String())
		var out struct {
			Data struct {
				Results    []map[string]any `json:"results"`
				Total      int              `json:"total"`
				Pagination struct {
					More bool `json:"has_next"`
				} `json:"pagination"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &out))
		require.Len(t, out.Data.Results, 1)
		require.Equal(t, tc.id, out.Data.Results[0]["api_id"])
		require.Equal(t, 4, out.Data.Total)
		require.Equal(t, tc.more, out.Data.Pagination.More)
	}
	before := reads.Load()
	for _, query := range []string{
		"provider=query-fixture&q=image&sort=downloads", "provider=query-fixture&q=image&tags=gguf",
		"provider=query-fixture&q=image&author=org", "provider=ollama&sort=date",
		"provider=huggingface&sort=madeup", "provider=huggingface&order=down",
		"provider=huggingface&cursor=abc&offset=1", "provider=ollama&cursor=abc",
		"provider=huggingface&sort=", "provider=huggingface&since=2026-11-01",
		"provider=huggingface&open_source=true", "provider=query-fixture&q=image&search=other",
	} {
		r := get(query)
		require.Equal(t, 400, r.Code, r.Body.String())
	}
	require.Equal(t, before, reads.Load())

	// Cached HF result verifies aliases/defaults and pagination without consulting credentials or network.
	params := search.SearchParams{Provider: search.ProviderHuggingFace, Search: "cached", Sort: "trendingScore", Direction: -1, Pagination: search.PaginationParams{Limit: 20}}
	s.searchCache.Set(search.SearchCacheKey(params, "AND", false), gin.H{"results": []any{}, "sentinel": "default-query"})
	r := get("provider=huggingface&search=cached")
	require.Equal(t, 200, r.Code, r.Body.String())
	require.Contains(t, r.Body.String(), "default-query")
}
