package search

import (
	"context"
	"errors"
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/pkg/ui"

	"charm.land/bubbles/v2/cursor"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type discoveryFixture struct {
	search    func(context.Context, metadata.SearchQuery) (*metadata.SearchPage, error)
	variants  func(context.Context, string, string) (*metadata.HuggingFaceVariantsResponse, error)
	card      func(context.Context, string, string) (*metadata.DiscoveryResult, error)
	inventory []pkgClient.NodeWithProviders
	capacity  *pkgClient.CompatibleNodes
	deploy    func(context.Context, pkgClient.DeployRequest) (*pkgClient.Deployment, error)
}

func (f *discoveryFixture) SearchModels(ctx context.Context, q metadata.SearchQuery) (*metadata.SearchPage, error) {
	return f.search(ctx, q)
}
func (f *discoveryFixture) GetSearchProviders(context.Context) ([]metadata.SearchProvider, error) {
	return nil, nil
}
func (f *discoveryFixture) GetModelCard(ctx context.Context, p, id string) (*metadata.DiscoveryResult, error) {
	return f.card(ctx, p, id)
}
func (f *discoveryFixture) GetHuggingFaceVariants(ctx context.Context, id, p string) (*metadata.HuggingFaceVariantsResponse, error) {
	return f.variants(ctx, id, p)
}
func (f *discoveryFixture) GetOllamaTags(context.Context, string) (*metadata.OllamaTags, error) {
	panic("unexpected Ollama call")
}
func (f *discoveryFixture) GetNodeInventory(context.Context) ([]pkgClient.NodeWithProviders, error) {
	return f.inventory, nil
}
func (f *discoveryFixture) GetCompatibleNodes(context.Context, string, string, string) (*pkgClient.CompatibleNodes, error) {
	return f.capacity, nil
}
func (f *discoveryFixture) Deploy(ctx context.Context, req pkgClient.DeployRequest) (*pkgClient.Deployment, error) {
	return f.deploy(ctx, req)
}

func activeSearch(t *testing.T, f *discoveryFixture) *SearchTUIModel {
	t.Helper()
	m := &SearchTUIModel{filterCursor: cursor.New(), client: f, Provider: "huggingface", State: StateList, TermWidth: 80, TermHeight: 24}
	_ = m.Activate()
	t.Cleanup(m.Deactivate)
	return m
}

func TestSearchCommandsCaptureInputsAndRejectSlowABA(t *testing.T) {
	entered := make(chan metadata.SearchQuery, 3)
	replies := make(chan searchCompleteMsg, 3)
	release := make(chan struct{})
	f := &discoveryFixture{search: func(ctx context.Context, q metadata.SearchQuery) (*metadata.SearchPage, error) {
		entered <- q
		<-release
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &metadata.SearchPage{Results: []metadata.DiscoveryResult{{Discovery: metadata.Discovery{Provider: q.Provider, APIID: q.Query, DisplayName: q.Query}}}}, nil
	}}
	m := activeSearch(t, f)
	m.query = "A"
	m.inlineFilter = "A"
	m.tagFilter = []string{"gguf"}
	cmdA := m.fetchResults()
	go func() { replies <- cmdA().(searchCompleteMsg) }()
	first := <-entered
	m.tagFilter[0] = "mlx"
	m.query = "B"
	m.inlineFilter = "B"
	cmdB := m.fetchResults()
	go func() { replies <- cmdB().(searchCompleteMsg) }()
	second := <-entered
	m.query = "A"
	m.inlineFilter = "A"
	cmdA2 := m.fetchResults()
	go func() { replies <- cmdA2().(searchCompleteMsg) }()
	third := <-entered
	assert.Equal(t, "gguf", first.Tags)
	assert.Equal(t, "mlx", second.Tags)
	assert.Equal(t, []string{"A", "B", "A"}, []string{first.Query, second.Query, third.Query})
	close(release)
	var old []searchCompleteMsg
	for range 3 {
		msg := <-replies
		if msg.stamp.generation == m.searchGeneration {
			_, _ = m.Update(msg)
		} else {
			old = append(old, msg)
		}
	}
	require.Len(t, m.results, 1)
	assert.Equal(t, "A", m.results[0].Name)
	for _, msg := range old {
		_, _ = m.Update(msg)
	}
	assert.NoError(t, m.err)
	assert.Equal(t, "A", m.results[0].Name)
}

func TestSelectionGenerationRejectsABAAndStaleErrors(t *testing.T) {
	calls := 0
	f := &discoveryFixture{variants: func(_ context.Context, id, provider string) (*metadata.HuggingFaceVariantsResponse, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("old selection failed")
		}
		return &metadata.HuggingFaceVariantsResponse{ModelID: id, Variants: []metadata.GGUFVariant{{File: id + "-1", Shards: 2, Recommended: true}, {File: id + "-2"}}, Features: []metadata.FeatureFile{{Feature: "vision", File: "mmproj.gguf"}}}, nil
	}}
	m := activeSearch(t, f)
	m.selectedItem = &searchResult{Provider: "huggingface", Name: "org/A"}
	a := m.loadVariants()
	m.selectedItem = &searchResult{Provider: "huggingface", Name: "org/B"}
	b := m.loadVariants()
	m.selectedItem = &searchResult{Provider: "huggingface", Name: "org/A"}
	a2 := m.loadVariants()
	oldA, oldB, current := a(), b(), a2()
	_, _ = m.Update(current)
	require.Len(t, m.variants, 2)
	require.Len(t, m.availableFeatures, 1)
	assert.Equal(t, 2, m.variants[0].Shards)
	_, _ = m.Update(oldA)
	_, _ = m.Update(oldB)
	assert.Equal(t, "org/A-1", m.variants[0].Filename)
	assert.Equal(t, stateVariantSelect, m.State)
	assert.Empty(t, m.PullStatus)
}

func TestDeactivateCancelsRequestsAndReentryRejectsOldReplies(t *testing.T) {
	started := make(chan struct{})
	done := make(chan modelDetailsMsg, 1)
	f := &discoveryFixture{card: func(ctx context.Context, _, _ string) (*metadata.DiscoveryResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	m := activeSearch(t, f)
	m.selectedItem = &searchResult{Provider: "huggingface", Name: "org/A", Description: "unchanged"}
	cmd := m.fetchModelDetails()
	go func() { done <- cmd().(modelDetailsMsg) }()
	<-started
	firstEpoch := m.activation
	m.Deactivate()
	reply := <-done
	assert.ErrorIs(t, reply.err, context.Canceled)
	_ = m.Activate()
	assert.Greater(t, m.activation, firstEpoch)
	_, _ = m.Update(reply)
	assert.Equal(t, "unchanged", m.selectedItem.Description)
	assert.Empty(t, m.PullStatus)
}

func TestDeploymentSubmitsOnceAndRetainsAcceptedJobsAfterLeaving(t *testing.T) {
	calls := 0
	f := &discoveryFixture{deploy: func(_ context.Context, req pkgClient.DeployRequest) (*pkgClient.Deployment, error) {
		calls++
		assert.Equal(t, []string{"node-a", "node-b"}, req.Nodes)
		assert.Equal(t, "org/model", req.Model)
		assert.Equal(t, "weights.gguf", req.File)
		assert.Equal(t, "llamacpp", req.Provider)
		return &pkgClient.Deployment{ID: "deployment-1", Nodes: []pkgClient.DeploymentNode{{Node: "node-a", JobID: "job-a"}, {Node: "node-b", JobID: "job-b"}}}, nil
	}}
	m := activeSearch(t, f)
	m.State = statePulling
	m.selectedItem = &searchResult{Provider: "huggingface", Name: "org/model"}
	m.selectedVariant = &ModelVariant{Filename: "weights.gguf"}
	m.selectedProvider = "llamacpp"
	nodes := []string{"node-a", "node-b"}
	cmd := m.deployModelToNodes(nodes)
	nodes[0] = "other"
	m.selectedItem.Name = "other"
	m.selectedVariant.Filename = "other"
	m.selectedProvider = "other"
	reply := cmd()
	m.Deactivate()
	_, _ = m.Update(reply)
	require.Equal(t, 1, calls)
	require.NotNil(t, m.LastDeployment)
	assert.Equal(t, "deployment-1", m.LastDeployment.ID)
	assert.Equal(t, "job-b", m.LastDeployment.Nodes[1].JobID)
	assert.Equal(t, statePulling, m.State)
}

func TestExplicitDownloadInventoryPreservesUnknownCapacity(t *testing.T) {
	capacity := &pkgClient.CompatibleNodes{}
	capacity.Metadata.Assessments = []pkgClient.NodeAssessment{{Node: "worker", Provider: "llamacpp", Status: "unknown", UnknownCause: "memory_evidence_unavailable", Reason: "No current measurement"}}
	f := &discoveryFixture{inventory: []pkgClient.NodeWithProviders{{Name: "worker"}, {Name: "other"}}, capacity: capacity}
	m := activeSearch(t, f)
	m.selectedItem = &searchResult{Provider: "huggingface", Name: "org/model"}
	_, _ = m.Update(m.loadAvailableNodes()())
	require.Len(t, m.availableNodes, 2)
	assert.Equal(t, "other", m.availableNodes[0].name)
	assert.Equal(t, "unknown", m.availableNodes[1].capacity)
	assert.Equal(t, "memory_evidence_unavailable", m.availableNodes[1].assessments[0].UnknownCause)
	assert.False(t, m.selectedNodes["worker"])
	assert.Equal(t, stateNodeSelect, m.State)
}

func TestNarrowNodePickerKeepsCapacityAndCurrentRowVisible(t *testing.T) {
	m := activeSearch(t, &discoveryFixture{})
	m.styles = ui.NewStyles(ui.CatppuccinMocha())
	m.State = stateNodeSelect
	m.selectedItem = &searchResult{Provider: "huggingface", Name: "org/model"}
	for i := range 20 {
		m.availableNodes = append(m.availableNodes, hostWithProviders{name: fmt.Sprintf("node-%02d", i), capacity: "unknown", assessments: []pkgClient.NodeAssessment{{Provider: "llamacpp", Status: "unknown", UnknownCause: "memory_evidence_unavailable", Reason: "GPU measurement unavailable"}}})
	}
	m.hostCursor = 19
	_, _ = m.Update(tea.WindowSizeMsg{Width: 42, Height: 18})
	view := m.viewNodeSelect()
	assert.Contains(t, view, "node-19")
	assert.Contains(t, view, "CAPACITY")
	assert.Contains(t, view, "unknown")
	assert.Contains(t, view, "GPU measurement unavailable")
	assert.Greater(t, m.hostOffset, 0)
}

func TestMouseBackFromDetailKeepsVariantSelectionIdentity(t *testing.T) {
	m := activeSearch(t, &discoveryFixture{})
	m.State = StateDetail
	m.prevState = stateVariantSelect
	m.selectedItem = &searchResult{Provider: "huggingface", Name: "org/model"}
	m.variants = []ModelVariant{{Filename: "a.gguf"}, {Filename: "b.gguf"}}
	m.selectedVariant = &m.variants[0]
	_, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseRight})
	assert.Equal(t, stateVariantSelect, m.State)
	require.NotNil(t, m.selectedItem)
	assert.Equal(t, "org/model", m.selectedItem.APIName())
	assert.Nil(t, m.selectedVariant)
}
