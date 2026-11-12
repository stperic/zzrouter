package search

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

func (m *SearchTUIModel) fetchProviderConfigs() tea.Cmd {
	if !m.active {
		return nil
	}
	client, ctx := m.client, m.ctx
	stamp := requestStamp{activation: m.activation}
	return func() tea.Msg {
		providers, err := client.GetSearchProviders(ctx)
		keys, configs := parseSearchProviders(providers)
		return providerConfigsLoadedMsg{stamp: stamp, keys: keys, configs: configs, err: err}
	}
}

func (m *SearchTUIModel) fetchResults() tea.Cmd {
	if !m.active {
		return nil
	}
	m.invalidateSearch()
	m.invalidateSelection()
	ctx, cancel := context.WithCancel(m.ctx)
	m.searchCancel = cancel
	stamp := requestStamp{activation: m.activation, generation: m.searchGeneration}
	cfg := m.getProviderConfig(m.Provider)
	sortBy := m.sortBy
	if sortBy == "" {
		sortBy = cfg.defaultSort()
	}
	query := metadata.SearchQuery{Query: m.query, Provider: m.Provider, Sort: mapSortToAPI(sortBy), Order: "desc", Tags: strings.Join(m.tagFilter, ","), Logic: m.tagLogic, Limit: 250, Full: true, Enrich: true}
	if cfg.ClientFilter {
		query.Query = ""
	}
	client, filter := m.client, m.inlineFilter
	return func() tea.Msg {
		page, err := client.SearchModels(ctx, query)
		if err != nil {
			return searchCompleteMsg{stamp: stamp, err: err}
		}
		rows := make([]searchResult, 0, len(page.Results))
		for _, result := range page.Results {
			rows = append(rows, presentationResult(result))
		}
		return searchCompleteMsg{stamp: stamp, results: rows, filter: filter}
	}
}

func presentationResult(result metadata.DiscoveryResult) searchResult {
	d := result.Discovery
	row := searchResult{Provider: d.Provider, Name: d.DisplayName, RawID: d.APIID, Description: d.Description, Downloads: d.Downloads, Likes: d.Likes, CreatedAt: d.CreatedAt, ModifiedAt: d.ModifiedAt, Tags: slices.Clone(d.Tags), SizeBytes: d.SizeBytes, URL: d.URL, VariantCount: d.VariantCount, ContextLength: d.ContextLength, TrendingScore: d.TrendingScore, HasCloud: d.HasCloud, rawData: result.Native}
	row.Metadata = buildMetadata(result.Native)
	if d.ParameterCount > 0 {
		row.Metadata = mergeMetadata(row.Metadata, []KeyValue{{Key: "Parameters", Value: strconv.FormatInt(d.ParameterCount, 10)}})
	}
	if d.SizeBytes > 0 {
		row.SizeDisplay = shared.FormatSize(d.SizeBytes)
	}
	if d.ContextLength > 0 {
		row.ContextWindow = shared.FormatSearchDownloads(d.ContextLength)
	}
	row.PricePrompt, _ = strconv.ParseFloat(d.PricePrompt, 64)
	row.PriceComplete, _ = strconv.ParseFloat(d.PriceComplete, 64)
	if row.PricePrompt > 0 || row.PriceComplete > 0 {
		row.PriceDisplay = fmt.Sprintf("$%.2f/M", row.PricePrompt*1e6)
	}
	return row
}

func (m *SearchTUIModel) loadVariants() tea.Cmd {
	if !m.active || m.selectedItem == nil {
		return nil
	}
	ctx, stamp := m.beginSelection(m.selectionIdentity())
	client, model, registry, provider := m.client, m.selectedItem.APIName(), m.selectedItem.Provider, m.selectedProvider
	return func() tea.Msg {
		reply := variantsLoadedMsg{stamp: stamp}
		switch registry {
		case constants.RepoOllama:
			tags, err := client.GetOllamaTags(ctx, model)
			if err != nil {
				reply.err = err
				return reply
			}
			for i, tag := range tags.Tags {
				reply.variants = append(reply.variants, ModelVariant{Filename: tag.Name, SizeDisplay: tag.Size, QuantMethod: tag.Context, Description: tag.Input, IsCloud: tag.IsCloud, CatalogOrder: i})
			}
		case constants.RepoHuggingFace:
			variants, err := client.GetHuggingFaceVariants(ctx, model, provider)
			if err != nil {
				reply.err = err
				return reply
			}
			reply.features = variants.Features
			for i, variant := range variants.Variants {
				description := variant.RecommendationReason
				if variant.Shards > 1 {
					description = fmt.Sprintf("%d shards. %s", variant.Shards, description)
				}
				reply.variants = append(reply.variants, ModelVariant{Filename: variant.File, Size: variant.SizeBytes, SizeDisplay: variant.SizeHuman, QuantMethod: variant.Quantization, Description: description, Recommended: variant.Recommended, Shards: variant.Shards, CatalogOrder: i})
			}
			sortVariants(reply.variants)
		}
		return reply
	}
}

func (m *SearchTUIModel) fetchModelDetails() tea.Cmd {
	if !m.active || m.selectedItem == nil {
		return nil
	}
	ctx, stamp := m.beginSelection(m.selectionIdentity())
	client, registry, model := m.client, m.selectedItem.Provider, m.selectedItem.APIName()
	return func() tea.Msg {
		card, err := client.GetModelCard(ctx, registry, model)
		if err != nil {
			return modelDetailsMsg{stamp: stamp, err: err}
		}
		description := card.Discovery.README
		if description == "" {
			description = card.Discovery.Description
		}
		return modelDetailsMsg{stamp: stamp, description: description, metadata: presentationResult(*card).Metadata}
	}
}

func (m *SearchTUIModel) loadAvailableNodes() tea.Cmd {
	if !m.active || m.selectedItem == nil {
		return nil
	}
	ctx, stamp := m.beginSelection(m.selectionIdentity())
	client, model, registry, provider := m.client, m.selectedItem.APIName(), m.selectedItem.Provider, m.selectedProvider
	return func() tea.Msg {
		inventory, err := client.GetNodeInventory(ctx)
		if err != nil {
			return hostsLoadedMsg{stamp: stamp, err: err}
		}
		capacity, assessmentErr := client.GetCompatibleNodes(ctx, model, registry, provider)
		if err := ctx.Err(); err != nil {
			return hostsLoadedMsg{stamp: stamp, err: err}
		}
		hosts := make([]hostWithProviders, 0, len(inventory))
		for _, node := range inventory {
			host := hostWithProviders{name: node.Name, role: node.ClusterRole, capacity: "unknown"}
			for _, provider := range node.Providers {
				host.providers = append(host.providers, provider.Name)
			}
			if node.DiskInfo != nil {
				host.diskAvailableGB = node.DiskInfo.AvailableGB
				host.diskTotalGB = node.DiskInfo.TotalGB
			}
			if node.MemoryInfo != nil {
				host.ramAvailableGB = node.MemoryInfo.AvailableGB
				host.ramTotalGB = node.MemoryInfo.TotalGB
			}
			if node.GPU != nil {
				host.gpuCount = node.GPU.Count
				host.vramAvailableKnown = len(node.GPU.GPUs) > 0
				for _, gpu := range node.GPU.GPUs {
					host.vramTotalGB += gpu.TotalGB
					if gpu.AvailableGB != nil {
						host.vramAvailableGB += *gpu.AvailableGB
					} else {
						host.vramAvailableKnown = false
					}
				}
			}
			if capacity != nil {
				for _, assessment := range capacity.Metadata.Assessments {
					if assessment.Node == node.Name || assessment.Node == "" {
						host.assessments = append(host.assessments, assessment)
					}
				}
			}
			if assessmentErr != nil {
				host.assessments = append(host.assessments, pkgClient.NodeAssessment{Node: node.Name, Status: "unknown", UnknownCause: "assessment_failed", Reason: "Capacity assessment unavailable"})
			}
			for _, assessment := range host.assessments {
				if assessment.Status == "fit" {
					host.capacity = "fit"
					break
				}
				if assessment.Status == "no_fit" {
					host.capacity = "no fit"
				}
			}
			hosts = append(hosts, host)
		}
		sort.SliceStable(hosts, func(i, j int) bool { return hosts[i].name < hosts[j].name })
		return hostsLoadedMsg{stamp: stamp, hosts: hosts}
	}
}

func (m *SearchTUIModel) validateModelForDeploy(provider, modelName string) tea.Cmd {
	if !m.active {
		return nil
	}
	ctx, stamp := m.beginSelection(provider + "\x00" + modelName)
	m.directPullLoading = m.State == stateDirectPull
	client := m.client
	return func() tea.Msg {
		card, err := client.GetModelCard(ctx, provider, modelName)
		reply := deployValidateMsg{stamp: stamp, modelName: modelName, provider: provider, err: err}
		if card != nil {
			reply.cloud = card.Discovery.IsCloud
		}
		return reply
	}
}

func (m *SearchTUIModel) deployModelToNodes(nodes []string) tea.Cmd {
	if !m.active || m.selectedItem == nil {
		return nil
	}
	ctx, stamp := m.beginSelection(m.selectionIdentity())
	request := pkgClient.DeployRequest{Model: m.selectedItem.APIName(), Registry: m.selectedItem.Provider, Provider: m.selectedProvider, Nodes: slices.Clone(nodes)}
	if m.selectedVariant != nil {
		request.File = m.selectedVariant.Filename
	}
	if len(request.Nodes) == 1 && request.Nodes[0] == "" {
		request.Nodes = nil
	}
	client := m.client
	return func() tea.Msg {
		deployment, err := client.Deploy(ctx, request)
		return deployCompleteMsg{stamp: stamp, deployment: deployment, err: err}
	}
}
