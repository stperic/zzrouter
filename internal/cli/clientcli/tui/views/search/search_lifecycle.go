package search

import (
	"context"
	"slices"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"

	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

type discoveryClient interface {
	SearchModels(context.Context, metadata.SearchQuery) (*metadata.SearchPage, error)
	GetSearchProviders(context.Context) ([]metadata.SearchProvider, error)
	GetModelCard(context.Context, string, string) (*metadata.DiscoveryResult, error)
	GetHuggingFaceVariants(context.Context, string, string) (*metadata.HuggingFaceVariantsResponse, error)
	GetOllamaTags(context.Context, string) (*metadata.OllamaTags, error)
	GetNodeInventory(context.Context) ([]pkgClient.NodeWithProviders, error)
	GetCompatibleNodes(context.Context, string, string, string) (*pkgClient.CompatibleNodes, error)
	Deploy(context.Context, pkgClient.DeployRequest) (*pkgClient.Deployment, error)
}

type requestStamp struct {
	activation uint64
	generation uint64
	identity   string
}

// Activate gives the off-stack singleton a new request lifetime on every entry.
func (m *SearchTUIModel) Activate() tea.Cmd {
	if m.active {
		return nil
	}
	m.active = true
	m.activation++
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return tea.Batch(m.fetchResults(), m.fetchProviderConfigs(), m.filterCursor.Focus())
}

// Deactivate cancels observation only. Accepted server jobs keep running.
func (m *SearchTUIModel) Deactivate() {
	if m == nil {
		return
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.invalidateSelection()
	m.invalidateSearch()
	m.filterCursor.Blur()
	m.active = false
	m.activation++
}

// Cancel satisfies the same teardown contract as stacked terminal views.
func (m *SearchTUIModel) Cancel() { m.Deactivate() }

// invalidateSearch retires both delayed input and in-flight list requests. A
// newer selection must remain authoritative even if canceled work replies late.
func (m *SearchTUIModel) invalidateSearch() {
	if m.searchCancel != nil {
		m.searchCancel()
		m.searchCancel = nil
	}
	m.searchGeneration++
	m.debounceSeqNo++
}

func (m *SearchTUIModel) invalidateSelection() {
	if m.selectionCancel != nil {
		m.selectionCancel()
		m.selectionCancel = nil
	}
	m.selectionGeneration++
	m.selectionID = ""
	m.directPullLoading = false
}

func (m *SearchTUIModel) beginSelection(identity string) (context.Context, requestStamp) {
	m.invalidateSearch()
	m.invalidateSelection()
	m.selectionID = identity
	ctx, cancel := context.WithCancel(m.ctx)
	m.selectionCancel = cancel
	return ctx, requestStamp{activation: m.activation, generation: m.selectionGeneration, identity: identity}
}

func (m *SearchTUIModel) accepts(stamp requestStamp, selection bool) bool {
	if !m.active || stamp.activation != m.activation {
		return false
	}
	if selection {
		return stamp.generation == m.selectionGeneration && stamp.identity == m.selectionID
	}
	return stamp.generation == m.searchGeneration
}

func (m *SearchTUIModel) selectionIdentity() string {
	if m.selectedItem == nil {
		return ""
	}
	id := m.selectedItem.Provider + "\x00" + m.selectedItem.APIName()
	if m.selectedVariant != nil {
		id += "\x00" + m.selectedVariant.Filename
	}
	return id
}

// OwnsMessage identifies asynchronous replies even while this singleton is off-stack.
func (m *SearchTUIModel) OwnsMessage(msg tea.Msg) bool {
	switch msg.(type) {
	case debounceSearchMsg, searchCompleteMsg, deployCompleteMsg, modelDetailsMsg, variantsLoadedMsg, providerLoadedMsg, hostsLoadedMsg, providerConfigsLoadedMsg, cloudModelAddedMsg, deployValidateMsg:
		return true
	}
	return false
}

func (m *SearchTUIModel) queueSearchPreferences() {
	registry, sort, tags := m.Provider, m.sortBy, slices.Clone(m.tagFilter)
	if m.registryPrefs == nil {
		m.registryPrefs = make(map[string]pkgConfig.SearchRegistryPrefs)
	}
	m.registryPrefs[registry] = pkgConfig.SearchRegistryPrefs{Sort: sort, Tags: tags}
	m.preferenceCommands = append(m.preferenceCommands, m.preferenceWriter.enqueue(func(store searchPreferenceStore) error {
		return store.SetSearchPrefs(registry, sort, tags)
	}))
}

func (m *SearchTUIModel) queueTagPreferences() {
	presets := slices.Clone(m.tagPresets)
	for i := range presets {
		presets[i].Tags = slices.Clone(presets[i].Tags)
	}
	tags := make([]pkgConfig.TagFilter, len(presets))
	for i, preset := range presets {
		tags[i] = pkgConfig.TagFilter{Tags: preset.Tags, Logic: preset.Logic}
	}
	m.preferenceCommands = append(m.preferenceCommands, m.preferenceWriter.enqueue(func(store searchPreferenceStore) error {
		return store.SetModelTags(tags)
	}))
}
