package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/modelregistry/search"
	"github.com/stretchr/testify/require"
)

func TestModelSearchQuery(t *testing.T) {
	const provider search.Provider = "query-fixture"
	var reads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		require.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Empty(t, r.URL.Query(), "retrieve the complete catalog before local filtering, sorting and pagination")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "unknown", "name": "No date image"},
			map[string]any{"id": "old", "name": "Alpha image", "created_at": "2026-10-01"},
			map[string]any{"id": "new", "name": "Zebra image", "created_at": "2026-11-05T00:00:00Z"},
			map[string]any{"id": "middle", "name": "Beta image", "created": float64(1791072000)},
			map[string]any{"id": "excluded", "name": "Other modality", "created_at": "2026-11-06"},
		}})
	}))
	t.Cleanup(upstream.Close)
	cfg := discoveryConfig(t, upstream.URL, "fixture")
	require.NoError(t, cfg.AddApp(string(provider), config.ServiceConfig{Enabled: new(true), Mode: "cloud", Protocol: config.ProtocolOpenAI, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}, Runtime: &config.AppRuntimeConfig{Endpoint: upstream.URL, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "fixture"}}}))
	resolver := backend.NewResolver(func() *config.AppsConfig { return cfg })
	search.RegisterCloudProvidersFromConfig(func() *config.AppsConfig { return cfg }, resolver)
	s := NewModelSearchService(func() *config.AppsConfig { return cfg }, nil, resolver)
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
	target, ok := resolver.Target("huggingface")
	require.True(t, ok)
	s.searchCache.Set(search.SearchCacheKey(params, "AND", false, target.Revision), gin.H{"results": []any{}, "sentinel": "default-query"})
	r := get("provider=huggingface&search=cached")
	require.Equal(t, 200, r.Code, r.Body.String())
	require.Contains(t, r.Body.String(), "default-query")
}
