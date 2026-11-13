package search

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingListWork captures replies before navigation and delivers them afterward,
// including successful responses that were already queued when cancellation ran.
func pendingListWork(t *testing.T, m *SearchTUIModel, f *discoveryFixture, kind string) (tea.Msg, context.Context) {
	t.Helper()
	if kind == "debounce" {
		require.NotNil(t, m.scheduleDebounceSearch())
		return debounceSearchMsg{activation: m.activation, sequence: m.debounceSeqNo, query: m.inlineFilter}, nil
	}
	var requestCtx context.Context
	f.search = func(ctx context.Context, _ metadata.SearchQuery) (*metadata.SearchPage, error) {
		requestCtx = ctx
		if kind == "error" {
			return nil, errors.New("late list failure")
		}
		return &metadata.SearchPage{Results: []metadata.DiscoveryResult{{Discovery: metadata.Discovery{Provider: "huggingface", APIID: "org/replacement", DisplayName: "org/replacement"}}}}, nil
	}
	cmd := m.fetchResults()
	require.NotNil(t, cmd)
	reply := cmd()
	require.NotNil(t, requestCtx)
	return reply, requestCtx
}

func searchKey(m *SearchTUIModel, code rune) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: code})
	return cmd
}

func selectSearchVariants(t *testing.T, m *SearchTUIModel) tea.Msg {
	t.Helper()
	cmd := searchKey(m, tea.KeyEnter)
	require.NotNil(t, cmd)
	return cmd()
}

func reachSearchDestination(t *testing.T, m *SearchTUIModel, destination string) tea.Msg {
	t.Helper()
	variants := selectSearchVariants(t, m)
	if destination == "variants-loading" {
		return variants
	}
	_, _ = m.Update(variants)
	require.Equal(t, stateVariantSelect, m.State)
	if destination == "variants" {
		return nil
	}
	cmd := searchKey(m, tea.KeyEnter)
	require.NotNil(t, cmd)
	detail := cmd()
	require.Equal(t, StateDetail, m.State)
	if destination == "detail" {
		return detail
	}
	_, _ = m.Update(detail)
	cmd = searchKey(m, 'p')
	require.NotNil(t, cmd)
	_, cmd = m.Update(cmd()) // Validate the selected model, then request node inventory.
	require.NotNil(t, cmd)
	_, _ = m.Update(cmd())
	require.Equal(t, stateNodeSelect, m.State)
	_ = searchKey(m, ' ')
	cmd = searchKey(m, tea.KeyEnter)
	require.NotNil(t, cmd)
	require.Equal(t, statePulling, m.State)
	return cmd
}

func TestPendingListWorkCannotInterruptNewerSelection(t *testing.T) {
	for _, kind := range []string{"debounce", "success", "error"} {
		for _, destination := range []string{"variants-loading", "variants", "detail", "deployment"} {
			for _, journey := range []string{"A", "ABA"} {
				t.Run(kind+"/"+destination+"/"+journey, func(t *testing.T) {
					var selectionCtx context.Context
					deployStarted := make(chan context.Context, 1)
					deployRelease := make(chan struct{})
					t.Cleanup(func() {
						select {
						case <-deployRelease:
						default:
							close(deployRelease)
						}
					})
					deployCalls := 0
					f := &discoveryFixture{
						variants: func(ctx context.Context, id, _ string) (*metadata.HuggingFaceVariantsResponse, error) {
							selectionCtx = ctx
							return &metadata.HuggingFaceVariantsResponse{Variants: []metadata.GGUFVariant{{File: id + "-1.gguf"}, {File: id + "-2.gguf"}}}, nil
						},
						card: func(ctx context.Context, _, id string) (*metadata.DiscoveryResult, error) {
							selectionCtx = ctx
							return &metadata.DiscoveryResult{Discovery: metadata.Discovery{Description: "Details for " + id}}, nil
						},
						inventory: []pkgClient.NodeWithProviders{{Name: "worker"}},
						deploy: func(ctx context.Context, _ pkgClient.DeployRequest) (*pkgClient.Deployment, error) {
							deployCalls++
							deployStarted <- ctx
							select {
							case <-deployRelease:
							case <-ctx.Done():
								return nil, ctx.Err()
							}
							return &pkgClient.Deployment{ID: "accepted-deployment", Nodes: []pkgClient.DeploymentNode{{Node: "worker", JobID: "accepted-job"}}}, nil
						},
					}
					m := activeSearch(t, f)
					m.results = []searchResult{{Provider: "huggingface", Name: "org/A"}, {Provider: "huggingface", Name: "org/B"}}
					var late []tea.Msg
					var searches []context.Context
					for i, name := range journey {
						m.cursor = 0
						if name == 'B' {
							m.cursor = 1
						}
						m.inlineFilter, m.query = string(name), string(name)
						reply, ctx := pendingListWork(t, m, f, kind)
						late = append(late, reply)
						if ctx != nil {
							searches = append(searches, ctx)
						}
						if i < len(journey)-1 {
							_, _ = m.Update(selectSearchVariants(t, m))
							_ = searchKey(m, tea.KeyEscape)
							require.Equal(t, StateList, m.State)
						}
					}
					currentReply := reachSearchDestination(t, m, destination)
					var deployReply <-chan tea.Msg
					if destination == "deployment" {
						command := currentReply.(tea.Cmd)
						replies := make(chan tea.Msg, 1)
						go func() { replies <- command() }()
						selectionCtx = <-deployStarted
						deployReply = replies
					}
					state, generation, identity := m.State, m.selectionGeneration, m.selectionID
					for _, ctx := range searches {
						assert.ErrorIs(t, ctx.Err(), context.Canceled)
					}
					for _, reply := range late {
						_, cmd := m.Update(reply)
						assert.Nil(t, cmd, "retired debounce must not launch another search")
					}
					assert.Equal(t, state, m.State)
					assert.Equal(t, generation, m.selectionGeneration)
					assert.Equal(t, identity, m.selectionID)
					assert.NoError(t, selectionCtx.Err(), "late list work must not cancel the newer operation")
					assert.NoError(t, m.err)
					assert.Equal(t, "org/A", m.selectedItem.APIName())
					require.Len(t, m.results, 2)
					assert.Equal(t, "org/A", m.results[0].Name)
					if deployReply != nil {
						close(deployRelease)
						currentReply = <-deployReply
						assert.ErrorIs(t, selectionCtx.Err(), context.Canceled, "completed deployment releases its request context")
					}
					if currentReply != nil {
						_, _ = m.Update(currentReply)
					}
					if destination == "deployment" {
						require.NotNil(t, m.LastDeployment)
						assert.Equal(t, "accepted-job", m.LastDeployment.Nodes[0].JobID)
						assert.Contains(t, m.PullStatus, "accepted-deployment")
						assert.Equal(t, 1, deployCalls)
					}
				})
			}
		}
	}
}

func TestListOverlaysRetirePendingListWork(t *testing.T) {
	for _, kind := range []string{"debounce", "success", "error"} {
		for _, shortcut := range []rune{'d', 'p', 's', 't'} {
			t.Run(kind+"/ctrl+"+string(shortcut), func(t *testing.T) {
				f := &discoveryFixture{}
				m := activeSearch(t, f)
				m.inlineFilter = "A"
				m.results = []searchResult{{Provider: "huggingface", Name: "org/A"}}
				m.providerKeys = []string{"huggingface"}
				m.providerConfigs = map[string]searchProviderConfig{"huggingface": {Enabled: true, HasTags: true, SortOptions: []string{"name"}}}
				m.tagPresets = []TagPreset{{Tags: []string{"gguf"}}}
				reply, ctx := pendingListWork(t, m, f, kind)
				_, _ = m.Update(tea.KeyPressMsg{Code: shortcut, Mod: tea.ModCtrl})
				state := statePickerOverlay
				if shortcut == 'd' {
					state = stateDirectPull
				}
				require.Equal(t, state, m.State)
				if ctx != nil {
					assert.ErrorIs(t, ctx.Err(), context.Canceled)
				}
				_, cmd := m.Update(reply)
				assert.Nil(t, cmd)
				assert.Equal(t, state, m.State)
				_ = searchKey(m, tea.KeyEscape)
				require.Equal(t, StateList, m.State)
				_, cmd = m.Update(reply)
				assert.Nil(t, cmd, "returning to the same query must not revive retired work")
				assert.NoError(t, m.err)
				require.Len(t, m.results, 1)
				assert.Equal(t, "org/A", m.results[0].Name)
			})
		}
	}
}

func TestLateProviderMetadataCannotRestartListOverSelection(t *testing.T) {
	for name, state := range map[string]viewState{"pending-variants": StateList, "variants": stateVariantSelect, "detail": StateDetail, "deployment": statePulling, "direct-deploy": stateDirectPull} {
		t.Run(name, func(t *testing.T) {
			m := activeSearch(t, &discoveryFixture{})
			ctx, _ := m.beginSelection("huggingface\x00org/A")
			m.State = state
			_, cmd := m.Update(providerConfigsLoadedMsg{
				stamp: requestStamp{activation: m.activation},
				keys:  []string{"openai"},
				configs: map[string]searchProviderConfig{
					"openai": {Enabled: true},
				},
			})
			assert.Nil(t, cmd)
			assert.Equal(t, state, m.State)
			assert.Equal(t, "huggingface", m.Provider)
			assert.NoError(t, ctx.Err())
			assert.Contains(t, m.providerConfigs, "openai")
		})
	}
}

func TestSelectingModelCancelsInFlightListRequest(t *testing.T) {
	started := make(chan struct{})
	reply := make(chan tea.Msg, 1)
	var selectionCtx context.Context
	f := &discoveryFixture{
		search: func(ctx context.Context, _ metadata.SearchQuery) (*metadata.SearchPage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		variants: func(ctx context.Context, _, _ string) (*metadata.HuggingFaceVariantsResponse, error) {
			selectionCtx = ctx
			return &metadata.HuggingFaceVariantsResponse{Variants: []metadata.GGUFVariant{{File: "a.gguf"}, {File: "b.gguf"}}}, nil
		},
	}
	m := activeSearch(t, f)
	m.results = []searchResult{{Provider: "huggingface", Name: "org/A"}}
	cmd := m.fetchResults()
	go func() { reply <- cmd() }()
	<-started
	variants := selectSearchVariants(t, m)
	_, _ = m.Update(variants)
	listReply := (<-reply).(searchCompleteMsg)
	require.ErrorIs(t, listReply.err, context.Canceled)
	_, _ = m.Update(listReply)
	assert.Equal(t, stateVariantSelect, m.State)
	assert.NoError(t, selectionCtx.Err())
	assert.NoError(t, m.err)
}

func TestTypingInListRetiresPendingSelectionBeforeDebounce(t *testing.T) {
	var selectionCtx context.Context
	f := &discoveryFixture{variants: func(ctx context.Context, _, _ string) (*metadata.HuggingFaceVariantsResponse, error) {
		selectionCtx = ctx
		return nil, errors.New("late variant failure")
	}}
	m := activeSearch(t, f)
	m.results = []searchResult{{Provider: "huggingface", Name: "org/A"}}
	variants := selectSearchVariants(t, m)
	require.Equal(t, StateList, m.State)
	require.NotNil(t, searchKey(m, 'B'))
	assert.ErrorIs(t, selectionCtx.Err(), context.Canceled)
	_, _ = m.Update(variants)
	assert.Equal(t, StateList, m.State)
	assert.Equal(t, "B", m.inlineFilter)
	assert.Empty(t, m.PullStatus)
}
