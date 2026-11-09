package search

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSearchQueryHuggingFaceOffset(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		require.Equal(t, "createdAt", q.Get("sort"))
		require.Equal(t, "-1", q.Get("direction"))
		require.Equal(t, "Qwen", q.Get("search"))
		require.Equal(t, "org", q.Get("author"))
		switch calls {
		case 1:
			require.Equal(t, "3", q.Get("limit"))
			require.Empty(t, q.Get("cursor"))
			w.Header().Set("Link", `<https://huggingface.co/api/models?cursor=next>; rel="next"`)
			_, _ = w.Write([]byte(`[{"id":"skip1"},{"id":"skip2"}]`))
		case 2:
			require.Equal(t, "1", q.Get("limit"))
			require.Equal(t, "next", q.Get("cursor"))
			w.Header().Set("Link", `<https://huggingface.co/api/models?cursor=final>; rel="next"`)
			_, _ = w.Write([]byte(`[{"id":"skip3"}]`))
		case 3:
			require.Equal(t, "1", q.Get("limit"))
			require.Equal(t, "final", q.Get("cursor"))
			w.Header().Set("Link", `<https://huggingface.co/api/models?cursor=continue>; rel="next"`)
			_, _ = w.Write([]byte(`[{"id":"chosen","createdAt":"2026-11-05T00:00:00Z"}]`))
		default:
			t.Fatal("unexpected request")
		}
	}))
	t.Cleanup(upstream.Close)
	client := NewClient(ClientOptions{BaseURL: upstream.URL, HTTPClient: upstream.Client()})
	result, err := client.SearchModels(t.Context(), SearchParams{Provider: ProviderHuggingFace, Search: "Qwen", Author: "org", Sort: "date", Direction: -1, Pagination: PaginationParams{Offset: 3, Limit: 1}})
	require.NoError(t, err)
	require.Equal(t, "chosen", result.Results[0].ID)
	require.True(t, result.HasMore)
	require.False(t, result.IsTotalExact)
	require.Equal(t, "continue", result.Cursor.Next)
	require.Equal(t, 3, calls)
}

func TestSearchQueryHuggingFacePopularAlias(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "trendingScore", r.URL.Query().Get("sort"))
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(upstream.Close)
	client := NewClient(ClientOptions{BaseURL: upstream.URL, HTTPClient: upstream.Client()})
	for _, provider := range []Provider{ProviderHuggingFace, ProviderAll} {
		_, err := client.SearchModels(t.Context(), SearchParams{Provider: provider, Sort: "popular", Pagination: PaginationParams{Limit: 10}})
		require.NoError(t, err)
	}
}

func TestSearchQueryCacheIsolation(t *testing.T) {
	base := SearchParams{Provider: ProviderHuggingFace, Search: "a&author=b", Sort: "date", Direction: -1, Pagination: PaginationParams{Limit: 10}}
	key := SearchCacheKey(base, "AND", false)
	for _, mutate := range []func(*SearchParams){
		func(p *SearchParams) { p.Author = "org" }, func(p *SearchParams) { p.Full = true },
		func(p *SearchParams) { p.Pagination.Cursor = "next" }, func(p *SearchParams) { p.Pagination.Offset = 10 },
		func(p *SearchParams) { p.Direction = 1 }, func(p *SearchParams) { p.Filter = []string{"gguf"} },
		func(p *SearchParams) { p.Search = "a"; p.Author = "b" },
	} {
		changed := base
		mutate(&changed)
		require.NotEqual(t, key, SearchCacheKey(changed, "AND", false))
	}
	require.NotEqual(t, key, SearchCacheKey(base, "OR", false))
	require.NotEqual(t, key, SearchCacheKey(base, "AND", true))
	base.Filter = []string{"gguf", "mlx"}
	other := base
	other.Filter = []string{"mlx", "gguf"}
	require.Equal(t, SearchCacheKey(base, "AND", false), SearchCacheKey(other, "AND", false))
}

func TestSearchQueryCloudDates(t *testing.T) {
	for _, direction := range []int{-1, 1} {
		var entries []any
		require.NoError(t, json.Unmarshal([]byte(`[{"id":"unknown"},{"id":"b","created_at":"2026-11-05T01:00:00+01:00"},{"id":"a","created_at":"2026-11-05T00:00:00Z"},{"id":"old","created":1}]`), &entries))
		NormalizeCloudResults(entries)
		SortCloudResults(entries, "date", direction)
		ids := []string{}
		for _, entry := range entries {
			ids = append(ids, entry.(map[string]any)["api_id"].(string))
		}
		want := []string{"a", "b", "old", "unknown"}
		if direction == 1 {
			want = []string{"old", "a", "b", "unknown"}
		}
		require.Equal(t, want, ids)
	}
}

type queryTransport func(*http.Request) (*http.Response, error)

func (f queryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSearchQueryOllamaSortBeforePage(t *testing.T) {
	// The ordinary library fetch reads the real captured page, without public network I/O.
	fixture := loadLibraryFixture(t)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = queryTransport(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "https://ollama.com/library", r.URL.String())
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fixture)), Header: http.Header{}}, nil
	})
	page, err := SearchOllamaModelsPaginated(SearchParams{Provider: ProviderOllama, Search: "nomic", Sort: "downloads", Direction: 1, Pagination: PaginationParams{Limit: 1, Offset: 1}})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, "nomic-embed-text", page.Results[0].Name)
	require.Equal(t, 2, page.Total)
	require.False(t, page.HasMore)
}
