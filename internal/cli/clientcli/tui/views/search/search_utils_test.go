package search

import (
	"context"
	"testing"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
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
		require.Empty(t, m.searchQuery().Sort, "an omitted choice must not depend on metadata arrival")
		m.providerConfigs = map[string]searchProviderConfig{provider: {SortOptions: []string{"popular"}}}
		require.Empty(t, m.searchQuery().Sort)
		m.Provider = "ollama"
		m.sortBy = "popular"
		require.Equal(t, "popular", m.searchQuery().Sort, "saved sorts remain valid before metadata arrives")
		m.providerConfigs["ollama"] = searchProviderConfig{ClientSorts: map[string]bool{"popular": true}}
		require.Empty(t, m.searchQuery().Sort, "loaded metadata retains client sorting")
		for _, tag := range []string{"local", "cloud"} {
			m.tagFilter = []string{tag}
			require.Empty(t, m.searchQuery().Tags, "Ollama view filters stay client-side")
		}
		m.Provider = "huggingface"
		m.tagFilter = []string{"gguf", "mlx"}
		m.tagLogic = "AND"
		require.Equal(t, "gguf,mlx", m.searchQuery().Tags)
		require.Equal(t, "AND", m.searchQuery().Logic)
	}
}

func TestOllamaViewTagsCaptureInputsWithoutUpstreamTags(t *testing.T) {
	fixture := &discoveryFixture{search: func(_ context.Context, q metadata.SearchQuery) (*metadata.SearchPage, error) {
		require.Empty(t, q.Tags)
		require.Empty(t, q.Sort)
		return &metadata.SearchPage{Results: []metadata.DiscoveryResult{
			{Discovery: metadata.Discovery{Provider: "ollama", APIID: "local-model", DisplayName: "Local model"}},
			{Discovery: metadata.Discovery{Provider: "ollama", APIID: "cloud-model", DisplayName: "Cloud model", HasCloud: true}},
		}}, nil
	}}
	m := activeSearch(t, fixture)
	m.Provider = "ollama"
	m.tagFilter, m.tagLogic = []string{"cloud"}, "AND"
	cmd := m.fetchResults()
	m.tagFilter[0] = "local"
	response := cmd().(searchCompleteMsg)
	require.NoError(t, response.err)
	require.Len(t, response.results, 1)
	require.Equal(t, "cloud-model", response.results[0].RawID)
	_, _ = m.Update(response)
	require.Len(t, m.results, 1)
	require.Equal(t, "cloud-model", m.results[0].RawID)
}

func TestSearchQueryCloudClientFilterFetchesCompleteCatalog(t *testing.T) {
	m := SearchTUIModel{Provider: "openai", query: "user filter", providerConfigs: map[string]searchProviderConfig{"openai": {ClientFilter: true}}}
	require.Empty(t, m.searchQuery().Query)
}
