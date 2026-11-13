package search

import (
	"context"
	"testing"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

func TestSearchRequestBroadMatchesSurvivePresentation(t *testing.T) {
	for _, tc := range []struct {
		name, filter, query string
		clientFilter        bool
		want                []string
	}{
		{"broad", "~vision", "vision", false, []string{"vision-model", "lens", "camera"}},
		{"broad whitespace", "  ~vision", "vision", false, []string{"vision-model", "lens", "camera"}},
		{"name", "vision", "vision", false, []string{"vision-model"}},
		{"local name", "vision", "", true, []string{"vision-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &discoveryFixture{search: func(_ context.Context, query metadata.SearchQuery) (*metadata.SearchPage, error) {
				require.Equal(t, tc.query, query.Query, "the broad prefix is view syntax, not an upstream query term")
				return &metadata.SearchPage{Results: []metadata.DiscoveryResult{
					{Discovery: metadata.Discovery{Provider: "huggingface", APIID: "vision-model", DisplayName: "vision-model"}},
					{Discovery: metadata.Discovery{Provider: "huggingface", APIID: "lens", DisplayName: "lens", Description: "A vision model"}},
					{Discovery: metadata.Discovery{Provider: "huggingface", APIID: "camera", DisplayName: "camera", Tags: []string{"vision"}}},
				}}, nil
			}}
			m := activeSearch(t, f)
			m.inlineFilter = tc.filter
			m.providerConfigs = map[string]searchProviderConfig{"huggingface": {ClientFilter: tc.clientFilter}}
			_, command := m.Update(debounceSearchMsg{activation: m.activation, sequence: m.debounceSeqNo, query: tc.filter})
			require.NotNil(t, command)
			m.inlineFilter = "later input"
			_, _ = m.Update(command())
			require.NoError(t, m.err)
			var names []string
			for _, result := range m.results {
				names = append(names, result.Name)
			}
			require.Equal(t, tc.want, names, "the response uses the captured filter, preserving server broad matches")
			require.Len(t, m.allResults, 3, "the cached catalog retains all server results")
		})
	}
}

func TestStaleBroadSearchCannotReplaceCurrentNameResults(t *testing.T) {
	var firstContext context.Context
	f := &discoveryFixture{search: func(ctx context.Context, query metadata.SearchQuery) (*metadata.SearchPage, error) {
		if firstContext == nil {
			firstContext = ctx
		}
		return &metadata.SearchPage{Results: []metadata.DiscoveryResult{{Discovery: metadata.Discovery{
			Provider: "huggingface", APIID: query.Query, DisplayName: query.Query,
		}}}}, nil
	}}
	m := activeSearch(t, f)
	m.inlineFilter, m.query = "~vision", "vision"
	old := m.fetchResults()()
	m.inlineFilter, m.query = "current", "current"
	current := m.fetchResults()
	require.ErrorIs(t, firstContext.Err(), context.Canceled)
	_, _ = m.Update(current())
	require.Len(t, m.results, 1)
	require.Equal(t, "current", m.results[0].Name)
	_, _ = m.Update(old)
	require.Len(t, m.results, 1)
	require.Equal(t, "current", m.results[0].Name, "a queued broad reply retains its original generation")
}

func TestCachedClientNameFilterRemainsLocal(t *testing.T) {
	m := activeSearch(t, &discoveryFixture{search: func(context.Context, metadata.SearchQuery) (*metadata.SearchPage, error) {
		t.Fatal("cached name filtering must not fetch")
		return nil, nil
	}})
	m.providerConfigs = map[string]searchProviderConfig{"huggingface": {ClientFilter: true}}
	m.allResults = []searchResult{{Name: "vision-model"}, {Name: "lens", Description: "A vision model"}}
	m.inlineFilter = "vision"
	_, command := m.Update(debounceSearchMsg{activation: m.activation, sequence: m.debounceSeqNo, query: m.inlineFilter})
	require.Nil(t, command)
	require.Len(t, m.results, 1)
	require.Equal(t, "vision-model", m.results[0].Name)
}
