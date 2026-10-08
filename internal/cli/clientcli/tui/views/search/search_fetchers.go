package search

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/search"
)

// fetchProviderConfigs fetches provider metadata from the server's
// GET /zzrouter/search?type=providers endpoint. The result populates providerKeys
// and providerConfigs on the model, replacing the hardcoded defaults.
func (m SearchTUIModel) fetchProviderConfigs() tea.Cmd {
	return func() tea.Msg {
		cm := pkgConfig.NewConfigManager("zzrouter")
		clientConfig, err := cm.LoadClientConfig()
		if err != nil {
			return providerConfigsLoadedMsg{} // silent fallback to defaults
		}
		host, err := shared.EnsureNodeOnline(clientConfig)
		if err != nil {
			return providerConfigsLoadedMsg{}
		}
		client := pkgClient.NewClient(*host)
		data, err := client.GetSearchProviders()
		if err != nil {
			return providerConfigsLoadedMsg{}
		}
		keys, configs := parseSearchProviders(data)
		return providerConfigsLoadedMsg{keys: keys, configs: configs}
	}
}

// fetchResults performs the search using the unified server API
func (m SearchTUIModel) fetchResults() tea.Cmd { //nolint:gocyclo,cyclop // Search response decoding handles optional fields from multiple registries together.
	return func() tea.Msg {
		// Get client config
		cm := pkgConfig.NewConfigManager("zzrouter")
		clientConfig, err := cm.LoadClientConfig()
		if err != nil {
			return searchCompleteMsg{results: nil, err: fmt.Errorf("failed to load config: %w", err)}
		}

		// Ensure host is online
		host, err := shared.EnsureNodeOnline(clientConfig)
		if err != nil {
			return searchCompleteMsg{results: nil, err: fmt.Errorf("server not available: %w", err)}
		}

		// Create client
		client := pkgClient.NewClient(*host)

		// Determine provider parameter
		providerParam := "all"
		if m.Provider != "" {
			providerParam = m.Provider
		}

		// Map unified sort names to server API values
		sortBy := mapSortToAPI(m.sortBy)
		order := "desc"

		// Convert tag filter to comma-separated string for API
		tagsParam := ""
		if len(m.tagFilter) > 0 {
			tagsParam = strings.Join(m.tagFilter, ",")
		}

		// HuggingFace API supports up to 1000; use 250 for all providers
		limit := 250

		// Call unified search API with sort and tags
		searchResponse, err := client.SearchModelsWithTags(m.query, providerParam, limit, sortBy, order, tagsParam, m.tagLogic)
		if err != nil {
			return searchCompleteMsg{results: nil, err: fmt.Errorf("search failed: %w", err)}
		}

		// Parse provider from top-level response
		responseProvider, _ := searchResponse["provider"].(string)
		if responseProvider == "" {
			responseProvider = constants.RepoHuggingFace // Default fallback
		}

		// Parse results from response
		resultsInterface, ok := searchResponse["results"]
		if !ok {
			return searchCompleteMsg{results: nil, err: fmt.Errorf("no results in response")}
		}

		results, ok := resultsInterface.([]any)
		if !ok {
			return searchCompleteMsg{results: nil, err: fmt.Errorf("invalid results format")}
		}

		// Convert to searchResult format
		var allResults []searchResult
		for _, item := range results {
			result, ok := item.(map[string]any)
			if !ok {
				continue
			}

			// Extract fields with type assertions
			id, _ := result["id"].(string)
			name, _ := result["name"].(string)
			displayName, _ := result["display_name"].(string)
			apiID, _ := result["api_id"].(string)
			downloads, _ := result["downloads"].(float64)
			pulls, _ := result["pulls"].(float64)
			likes, _ := result["likes"].(float64)
			trendingScore, _ := result["trendingScore"].(float64)

			// Try different date field names
			createdAt, _ := result["created_at"].(string)
			if createdAt == "" {
				createdAt, _ = result["createdAt"].(string)
			}
			// OpenRouter uses "created" as a Unix timestamp
			if createdAt == "" {
				if createdUnix, ok := result["created"].(float64); ok && createdUnix > 0 {
					createdAt = time.Unix(int64(createdUnix), 0).Format(time.RFC3339)
				}
			}

			modifiedAt, _ := result["modified_at"].(string)
			if modifiedAt == "" {
				modifiedAt, _ = result["lastModified"].(string)
			}
			if modifiedAt == "" {
				modifiedAt, _ = result["last_modified"].(string)
			}

			// Extract variant/sibling count
			variantCount := 0

			// For Ollama: use tags count (number of available versions)
			if tagsCount, ok := result["tags"].(float64); ok && tagsCount > 0 {
				variantCount = int(tagsCount)
			} else if siblingsInterface, ok := result["siblings"].([]any); ok {
				// For HuggingFace: count only displayable GGUF variants
				for _, sib := range siblingsInterface {
					if sibMap, ok := sib.(map[string]any); ok {
						if rfilename, ok := sibMap["rfilename"].(string); ok {
							filename := strings.ToLower(rfilename)

							// Must be a .gguf file
							if !strings.HasSuffix(filename, ".gguf") {
								continue
							}

							// Skip non-model files
							if strings.Contains(filename, "readme") ||
								strings.Contains(filename, "license") ||
								strings.Contains(filename, ".txt") ||
								strings.Contains(filename, ".md") ||
								strings.Contains(filename, "imatrix") ||
								strings.Contains(filename, "config.json") ||
								strings.Contains(filename, "tokenizer") {
								continue
							}

							// Only count if we can identify the quant method
							quant := extractQuantMethod(rfilename)
							if quant != "Unknown" {
								variantCount++
							}
						}
					}
				}
			}

			// Trust the server's response provider; fall back to heuristic for "all" queries
			var resultProvider string
			if responseProvider != "" && responseProvider != "all" {
				resultProvider = responseProvider
			} else if pulls > 0 {
				resultProvider = constants.RepoOllama
			} else if id != "" {
				resultProvider = constants.RepoHuggingFace
			} else {
				resultProvider = constants.RepoHuggingFace
			}

			// api_id from server normalization — the canonical API identifier.
			// Falls back to id/name for non-cloud providers.
			rawID := apiID
			if rawID == "" {
				rawID = id
				if rawID == "" {
					rawID = name
				}
			}
			// display_name from server normalization — clean for UI.
			// Falls back to rawID for non-cloud providers.
			modelName := displayName
			if modelName == "" {
				modelName = rawID
			}

			// Use downloads if available, otherwise pulls
			downloadCount := int(downloads)
			if downloadCount == 0 {
				downloadCount = int(pulls)
			}

			// Extract tags
			var tags []string
			if tagsInterface, ok := result["tags"].([]any); ok {
				for _, tag := range tagsInterface {
					if tagStr, ok := tag.(string); ok {
						tags = append(tags, tagStr)
					}
				}
			}

			// Extract size: top-level "size" (Ollama/cloud), safetensors.total, or gguf.total
			sizeFloat, _ := result["size"].(float64)
			sizeBytes := int64(sizeFloat)
			if sizeBytes == 0 {
				if st, ok := result["safetensors"].(map[string]any); ok {
					if total, ok := st["total"].(float64); ok {
						sizeBytes = int64(total)
					}
				}
			}
			if sizeBytes == 0 {
				if gguf, ok := result["gguf"].(map[string]any); ok {
					if total, ok := gguf["total"].(float64); ok {
						sizeBytes = int64(total)
					}
				}
			}
			sizeDisplay := ""
			if sizeBytes > 0 {
				sizeDisplay = shared.FormatSize(sizeBytes)
			}

			// Extract description (OpenRouter provides this)
			description, _ := result["description"].(string)

			// Extract context window from provider-specific field names
			contextWindow := ""
			contextLength := 0
			if ctxLen, ok := result["context_length"].(float64); ok && ctxLen > 0 {
				contextLength = int(ctxLen)
			} else if ctxWin, ok := result["context_window"].(float64); ok && ctxWin > 0 {
				contextLength = int(ctxWin)
			} else if maxIn, ok := result["max_input_tokens"].(float64); ok && maxIn > 0 {
				contextLength = int(maxIn)
			}
			if contextLength > 0 {
				contextWindow = shared.FormatContextWindow(contextLength)
			}

			// Extract pricing (OpenRouter provides pricing.prompt and pricing.completion)
			var pricePrompt, priceComplete float64
			priceDisplay := ""
			if pricing, ok := result["pricing"].(map[string]any); ok {
				if p, ok := pricing["prompt"].(string); ok {
					_, _ = fmt.Sscanf(p, "%f", &pricePrompt)
				}
				if p, ok := pricing["completion"].(string); ok {
					_, _ = fmt.Sscanf(p, "%f", &priceComplete)
				}
				if pricePrompt > 0 || priceComplete > 0 {
					priceDisplay = shared.FormatCloudPrice(pricePrompt, priceComplete)
				}
			}

			// URL comes from the server (enriched with provider's model_url)
			url, _ := result["url"].(string)

			// Ollama cloud badge
			hasCloud, _ := result["has_cloud"].(bool)

			allResults = append(allResults, searchResult{
				Provider:      resultProvider,
				Name:          modelName,
				RawID:         rawID,
				Description:   description,
				Downloads:     downloadCount,
				Likes:         int(likes),
				CreatedAt:     createdAt,
				ModifiedAt:    modifiedAt,
				Tags:          tags,
				SizeBytes:     sizeBytes,
				SizeDisplay:   sizeDisplay,
				URL:           url,
				VariantCount:  variantCount,
				ContextWindow: contextWindow,
				ContextLength: contextLength,
				TrendingScore: trendingScore,
				PricePrompt:   pricePrompt,
				PriceComplete: priceComplete,
				PriceDisplay:  priceDisplay,
				HasCloud:      hasCloud,
				rawData:       result,
			})
		}

		// Filter out Ollama entries with zero downloads (non-model pages that slipped through scraping).
		// Cloud providers (OpenRouter) and HuggingFace don't use this heuristic.
		if responseProvider == constants.RepoOllama {
			var cleaned []searchResult
			for _, r := range allResults {
				if r.Downloads > 0 {
					cleaned = append(cleaned, r)
				}
			}
			allResults = cleaned

			// Ollama cloud/local tag filter
			if len(m.tagFilter) > 0 {
				var filtered []searchResult
				for _, r := range allResults {
					for _, tag := range m.tagFilter {
						switch strings.ToLower(tag) {
						case "cloud":
							if r.HasCloud {
								filtered = append(filtered, r)
							}
						case "local":
							if !r.HasCloud {
								filtered = append(filtered, r)
							}
						}
					}
				}
				allResults = filtered
			}
		}

		// Client-side name .Filter: when NOT broad search (~), filter results to only
		// models whose name matches the filter pattern. Supports wildcards:
		// "qwen3*mlx" matches names containing "qwen3" AND "mlx" in order.
		if !m.broadSearch && m.inlineFilter != "" {
			var filtered []searchResult
			for _, r := range allResults {
				if matchesWildcard(r.Name, m.inlineFilter) {
					filtered = append(filtered, r)
				}
			}
			allResults = filtered
		}

		// Empty results is not an error — keep TUI alive so user can refine
		if len(allResults) == 0 {
			return searchCompleteMsg{
				results: nil,
				err:     nil,
			}
		}

		return searchCompleteMsg{
			results: allResults,
			err:     nil,
		}
	}
}

// loadVariants loads available variants for the selected model
func (m SearchTUIModel) loadVariants() tea.Cmd {
	return func() tea.Msg {
		if m.selectedItem == nil {
			return variantsLoadedMsg{err: fmt.Errorf("no model selected")}
		}

		// Handle Ollama models - fetch tags from library page
		if m.selectedItem.Provider == constants.RepoOllama {
			return m.loadOllamaTags()
		}

		// Handle HuggingFace models - fetch GGUF variants
		if m.selectedItem.Provider != constants.RepoHuggingFace {
			return variantsLoadedMsg{variants: nil, err: nil}
		}

		// If we already know there are no variants from the search results, skip
		if m.selectedItem.VariantCount == 0 {
			return variantsLoadedMsg{variants: nil, err: nil}
		}

		// Get client config
		cm := pkgConfig.NewConfigManager("zzrouter")
		clientConfig, err := cm.LoadClientConfig()
		if err != nil {
			return variantsLoadedMsg{err: fmt.Errorf("failed to load config: %w", err)}
		}

		// Ensure host is online
		host, err := shared.EnsureNodeOnline(clientConfig)
		if err != nil {
			return variantsLoadedMsg{err: fmt.Errorf("server not available: %w", err)}
		}

		// Create client
		client := pkgClient.NewClient(*host)

		// Fetch model card from server
		cardResponse, err := client.GetModelCard(m.selectedItem.Provider, m.selectedItem.APIName())
		if err != nil {
			return variantsLoadedMsg{err: fmt.Errorf("failed to fetch model card: %w", err)}
		}

		// Parse siblings from response
		siblingsInterface, ok := cardResponse["siblings"]
		if !ok {
			return variantsLoadedMsg{variants: nil, err: nil}
		}

		siblings, ok := siblingsInterface.([]any)
		if !ok {
			return variantsLoadedMsg{variants: nil, err: nil}
		}

		// Extract quantized variants from siblings
		var variants []ModelVariant
		for _, siblingItem := range siblings {
			sibling, ok := siblingItem.(map[string]any)
			if !ok {
				continue
			}

			rfilename, _ := sibling["rfilename"].(string)
			if rfilename == "" {
				continue
			}

			filename := strings.ToLower(rfilename)

			// Skip non-model files
			if strings.Contains(filename, "readme") ||
				strings.Contains(filename, "license") ||
				strings.Contains(filename, ".txt") ||
				strings.Contains(filename, ".md") ||
				strings.Contains(filename, "config.json") ||
				strings.Contains(filename, "tokenizer") {
				continue
			}

			// Check for GGUF, GPTQ (.safetensors), or AWQ files
			// GPTQ models use .safetensors, AWQ can use .safetensors or .pt
			if strings.HasSuffix(filename, ".gguf") ||
				strings.HasSuffix(filename, ".safetensors") ||
				strings.HasSuffix(filename, ".bin") ||
				strings.HasSuffix(filename, ".pt") {

				// Get size from sibling
				sizeFloat, _ := sibling["size"].(float64)
				size := int64(sizeFloat)

				quant := extractQuantMethod(rfilename)
				desc := getQuantDescription(quant)

				// Only add if we could identify the quant method or it's a model file
				if quant != "Unknown" || strings.Contains(filename, "model") {
					variants = append(variants, ModelVariant{
						Filename:    rfilename,
						Size:        size,
						SizeDisplay: shared.FormatSize(size),
						QuantMethod: quant,
						Description: desc,
					})
				}
			}
		}

		// Sort variants by recommended order
		sortVariants(variants)

		return variantsLoadedMsg{variants: variants, err: nil}
	}
}

// loadOllamaTags loads available tags/versions for an Ollama model
func (m SearchTUIModel) loadOllamaTags() tea.Msg {
	if m.selectedItem == nil {
		return variantsLoadedMsg{err: fmt.Errorf("no model selected")}
	}

	// Fetch tags from Ollama library using the scraper
	tags, err := search.GetOllamaModelTags(m.selectedItem.Name)
	if err != nil {
		return variantsLoadedMsg{err: fmt.Errorf("failed to fetch Ollama tags: %w", err)}
	}

	// Convert tags to ModelVariant format for display
	var variants []ModelVariant
	for _, tag := range tags {
		// Parse size string to bytes if possible (for sorting)
		sizeBytes := int64(0)
		// TODO: Parse size string like "5.2GB" to bytes

		description := tag.Input
		// Mark cloud tags so users know they require an API key
		if strings.HasSuffix(tag.Name, "-cloud") || strings.Contains(tag.Name, "-cloud-") || strings.EqualFold(tag.Name, "cloud") {
			description = "cloud (requires API key)"
		}

		variants = append(variants, ModelVariant{
			Filename:    tag.Name,
			Size:        sizeBytes,
			SizeDisplay: tag.Size,
			QuantMethod: tag.Context, // Show context in quant column
			Description: description,
		})
	}

	return variantsLoadedMsg{variants: variants, err: nil}
}

// fetchModelDetails fetches additional model details like size
func (m SearchTUIModel) fetchModelDetails() tea.Cmd {
	return func() tea.Msg {
		if m.selectedItem == nil {
			return modelDetailsMsg{err: fmt.Errorf("no model selected")}
		}

		// Get client config
		cm := pkgConfig.NewConfigManager("zzrouter")
		clientConfig, err := cm.LoadClientConfig()
		if err != nil {
			return modelDetailsMsg{err: fmt.Errorf("failed to load config: %w", err)}
		}

		// Ensure host is online
		host, err := shared.EnsureNodeOnline(clientConfig)
		if err != nil {
			return modelDetailsMsg{err: fmt.Errorf("server not available: %w", err)}
		}

		// Create client
		client := pkgClient.NewClient(*host)

		// Fetch model card from server
		cardResponse, err := client.GetModelCard(m.selectedItem.Provider, m.selectedItem.APIName())
		if err != nil {
			return modelDetailsMsg{err: fmt.Errorf("failed to fetch model card: %w", err)}
		}

		// Extract fields from response
		sizeFloat, _ := cardResponse["size"].(float64)
		sizeBytes := int64(sizeFloat)

		// If a variant is selected, use its size instead of total model size
		if m.selectedVariant != nil && m.selectedVariant.Size > 0 {
			sizeBytes = m.selectedVariant.Size
		}

		sizeDisplay := shared.FormatSize(sizeBytes)

		description, _ := cardResponse["description"].(string)
		readme, _ := cardResponse["readme"].(string)
		pipelineTag, _ := cardResponse["pipeline_tag"].(string)

		// Extract tags
		var tags []string
		if tagsInterface, ok := cardResponse["tags"].([]any); ok {
			for _, tag := range tagsInterface {
				if tagStr, ok := tag.(string); ok {
					tags = append(tags, tagStr)
				}
			}
		}

		// Use raw README markdown for detail view; fall back to extracted description
		descForDetail := readme
		if descForDetail == "" {
			descForDetail = description
		}

		// Build metadata from the full card response
		metadata := buildMetadata(cardResponse)

		// For local providers, supplement with tag-based heuristics (single merge)
		cfg := m.getProviderConfig(m.selectedItem.Provider)
		if !cfg.IsCloud {
			var extra []KeyValue
			if cw := extractContextWindow(tags, m.selectedItem.Name); cw != "" {
				extra = append(extra, KeyValue{Key: "Context", Value: cw})
			}
			if it := extractInputTypes(tags, pipelineTag); len(it) > 0 {
				extra = append(extra, KeyValue{Key: "Input", Value: strings.Join(it, ", ")})
			}
			if caps := extractCapabilities(tags); len(caps) > 0 {
				extra = append(extra, KeyValue{Key: "Capabilities", Value: strings.Join(caps, ", ")})
			}
			if sizeDisplay != "" && sizeDisplay != shared.EmptyValue {
				extra = append(extra, KeyValue{Key: "Size", Value: sizeDisplay})
			}
			if len(extra) > 0 {
				metadata = mergeMetadata(metadata, extra)
			}
		}

		return modelDetailsMsg{
			description: descForDetail,
			metadata:    metadata,
			err:         nil,
		}
	}
}

// loadAvailableNodes fetches the list of available nodes with compatible inference providers
// parseResourceInfo extracts available/total GB from a string like "1519 / 1858 GB" or "70 / 128 GB".
func parseResourceInfo(s string) (available, total float64) {
	_, _ = fmt.Sscanf(strings.ReplaceAll(s, ",", ""), "%f / %f", &available, &total)
	return
}

// loadAvailableNodes fetches nodes that have a compatible provider for the selected model.
// Uses /zzrouter/providers + /zzrouter/nodes (two fast requests) instead of broadcasting to every node.
func (m SearchTUIModel) loadAvailableNodes() tea.Cmd {
	return func() tea.Msg {
		cm := pkgConfig.NewConfigManager("zzrouter")
		clientConfig, err := cm.LoadClientConfig()
		if err != nil {
			return hostsLoadedMsg{err: fmt.Errorf("failed to load config: %w", err)}
		}
		host, err := shared.EnsureNodeOnline(clientConfig)
		if err != nil {
			return hostsLoadedMsg{err: fmt.Errorf("server not available: %w", err)}
		}
		client := pkgClient.NewClient(*host)

		// 1. Fetch all providers across cluster
		providers, err := shared.FetchProviders(client, "", "")
		if err != nil {
			return hostsLoadedMsg{err: fmt.Errorf("failed to fetch providers: %w", err)}
		}

		// 2. Determine which provider type and format the model needs
		repo := m.selectedItem.Provider
		if repo == "" {
			repo = constants.RepoHuggingFace
		}

		modelName := m.selectedItem.Name

		requiredFormat := modelregistry.DetectFormatFromName(modelName)
		if requiredFormat == "" && m.selectedItem != nil {
			requiredFormat = modelregistry.DetectFormatFromTags(m.selectedItem.Tags)
		}

		// 3. Filter providers to find nodes with compatible providers AND formats
		nodeProviders := make(map[string][]string) // node → list of compatible provider names
		for _, app := range providers {
			// Repo/app compatibility lives in modelregistry so this loop
			// doesn't have to know Ollama is a walled garden.
			if !metadata.RepoAppCompatible(repo, app.Name) {
				continue
			}

			// Format .Filter: app must support the required format
			if !modelregistry.FormatSupported(app.Formats, requiredFormat) {
				continue
			}

			nodeProviders[app.Node] = append(nodeProviders[app.Node], app.Name)
		}

		if len(nodeProviders) == 0 {
			return hostsLoadedMsg{err: noCompatibleNodesError(repo, modelName, requiredFormat, providers)}
		}

		// 4. Fetch node details for disk/RAM info
		nodeDetails, _ := shared.FetchNodes(client, "", false)
		nodeInfoMap := make(map[string]shared.NodeInfo)
		for _, n := range nodeDetails {
			nodeInfoMap[n.Name] = n
		}

		// 5. Build host list
		var hosts []hostWithProviders
		for nodeName, appTypes := range nodeProviders {
			h := hostWithProviders{
				name:      nodeName,
				providers: appTypes,
			}
			if info, ok := nodeInfoMap[nodeName]; ok {
				h.role = info.ClusterRole
				h.diskAvailableGB, h.diskTotalGB = parseResourceInfo(info.Disk)
				h.ramAvailableGB, h.ramTotalGB = parseResourceInfo(info.Memory)
				h.vramAvailableGB, h.vramTotalGB = parseResourceInfo(info.GPU) // "95 / 96 GB [1]" format
				h.gpuCount = len(info.GPUs)
				if h.gpuCount == 0 && info.GPU != "" && info.GPU != "-" {
					h.gpuCount = 1
				}
			}
			hosts = append(hosts, h)
		}

		return hostsLoadedMsg{hosts: hosts}
	}
}

// validateModelForDeploy checks if a model exists before pulling.
// For Ollama cloud variants, checks API key availability via a deploy dry-run.
// For others, uses GetModelCard as an existence check.
func (m SearchTUIModel) validateModelForDeploy(provider, modelName string) tea.Cmd {
	return func() tea.Msg {
		cm := pkgConfig.NewConfigManager("zzrouter")
		clientConfig, err := cm.LoadClientConfig()
		if err != nil {
			return deployValidateMsg{modelName: modelName, provider: provider, err: fmt.Errorf("config error: %w", err)}
		}

		host, err := shared.EnsureNodeOnline(clientConfig)
		if err != nil {
			return deployValidateMsg{modelName: modelName, provider: provider, err: fmt.Errorf("server not available: %w", err)}
		}

		client := pkgClient.NewClient(*host)

		// For Ollama: check model card from ollama.com library
		// For HuggingFace: check model card from HF API
		_, err = client.GetModelCard(provider, modelName)
		if err != nil {
			return deployValidateMsg{modelName: modelName, provider: provider, err: fmt.Errorf("model not found: %s", modelName)}
		}

		return deployValidateMsg{modelName: modelName, provider: provider, err: nil}
	}
}

// deployModelToNodes initiates a model deploy to one or more nodes.
func (m SearchTUIModel) deployModelToNodes(nodes []string) tea.Cmd {
	return func() tea.Msg {
		if m.selectedItem == nil {
			return deployCompleteMsg{success: false, err: fmt.Errorf("no model selected")}
		}

		cm := pkgConfig.NewConfigManager("zzrouter")
		clientConfig, err := cm.LoadClientConfig()
		if err != nil {
			return deployCompleteMsg{success: false, err: err}
		}

		host, err := shared.EnsureNodeOnline(clientConfig)
		if err != nil {
			return deployCompleteMsg{success: false, err: err}
		}

		client := pkgClient.NewClient(*host)

		repository := m.selectedItem.Provider
		if repository == "" {
			repository = constants.RepoHuggingFace
		}

		var errors []string
		file := ""
		if m.selectedVariant != nil {
			file = m.selectedVariant.Filename
		}
		for _, node := range nodes {
			if _, err := client.Deploy(m.selectedItem.APIName(), repository, file, []string{node}, false); err != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", node, err))
			}
		}

		if len(errors) > 0 {
			return deployCompleteMsg{success: false, err: fmt.Errorf("deploy failed on: %s", strings.Join(errors, "; "))}
		}
		return deployCompleteMsg{success: true, err: nil}
	}
}

// noCompatibleNodesError builds a user-friendly error from provider config data.
func noCompatibleNodesError(repo, modelName, requiredFormat string, allProviders []shared.ProviderInfo) error {
	var providers []modelregistry.ProviderFormatEntry
	for _, a := range allProviders {
		providers = append(providers, modelregistry.ProviderFormatEntry{Name: a.Name, Formats: a.Formats})
	}
	return errors.New(modelregistry.FormatIncompatibleError(modelName, requiredFormat, repo, providers))
}
