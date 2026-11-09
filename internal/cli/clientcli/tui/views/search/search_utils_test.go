package search

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSearchQueryProviderSortDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, key, want string
		cfg             searchProviderConfig
	}{
		{"hf-default", "", "", searchProviderConfig{SortOptions: []string{"popular", "newest"}, ClientSorts: map[string]bool{"name": true, "newest": true}}},
		{"ollama-default", "", "", searchProviderConfig{SortOptions: []string{"popular", "newest"}, ClientSorts: map[string]bool{"popular": true, "name": true, "newest": true}}},
		{"cloud-default", "", "", searchProviderConfig{SortOptions: []string{"newest", "name"}, ClientSorts: map[string]bool{"name": true, "newest": true}}},
		{"hf-updated", "newest", "lastModified", searchProviderConfig{}},
		{"explicit-creation", "date", "date", searchProviderConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, mapSortToAPI(tc.key, tc.cfg)) })
	}
}

func TestSearchQueryLocalFiltersAndMetadataArrival(t *testing.T) {
	for _, provider := range []string{"ollama", "openai", "huggingface"} {
		m := SearchTUIModel{Provider: provider}
		require.NotContains(t, m.searchQuery(), "sort", "an omitted choice must not depend on metadata arrival")
		m.providerConfigs = map[string]searchProviderConfig{provider: {SortOptions: []string{"popular"}}}
		require.NotContains(t, m.searchQuery(), "sort")
		m.Provider = "ollama"
		m.sortBy = "popular"
		require.Equal(t, "popular", m.searchQuery().Get("sort"), "saved sorts remain valid before metadata arrives")
		m.providerConfigs["ollama"] = searchProviderConfig{ClientSorts: map[string]bool{"popular": true}}
		require.NotContains(t, m.searchQuery(), "sort", "loaded metadata retains client sorting")
		for _, tag := range []string{"local", "cloud"} {
			m.tagFilter = []string{tag}
			require.NotContains(t, m.searchQuery(), "tags", "Ollama view filters stay client-side")
		}
		m.Provider = "huggingface"
		m.tagFilter = []string{"gguf", "mlx"}
		m.tagLogic = "AND"
		require.Equal(t, "gguf,mlx", m.searchQuery().Get("tags"))
		require.Equal(t, "AND", m.searchQuery().Get("logic"))
	}
}
