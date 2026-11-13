package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/search"
	"github.com/stperic/zzrouter/pkg/ui"
)

// ModelSearchService owns the discovery surface — public model search
// across HuggingFace, Ollama, and cloud providers — plus model-card
// fetch and the in-memory search-result cache that fronts those
// upstream APIs. It is a separate concern from ModelService (which
// manages the local model catalog, pull/show/delete lifecycle, and
// routing) and is constructed and wired alongside ModelService in
// routes_public.go.
//
// Coordinator-only: search hits external HTTP APIs and is gated by the
// coordinator role; workers never run a ModelSearchService.
type ModelSearchService struct {
	searchCache  *search.SearchCache
	backend      *backend.Resolver
	cards        *search.CardCache
	appsConfig   func() *pkgConfig.AppsConfig
	pricingStore *pricing.Store // Token-cost pricing data (may be nil if disabled)
}

// NewModelSearchService constructs a search service with an empty cache
// and a getter that follows ReloadConfig swaps so newly-enabled
// providers and registries take effect without a restart.
func NewModelSearchService(appsConfig func() *pkgConfig.AppsConfig, pricingStore *pricing.Store, resolvers ...*backend.Resolver) *ModelSearchService {
	resolver := backend.NewResolver(appsConfig)
	if len(resolvers) > 0 && resolvers[0] != nil {
		resolver = resolvers[0]
	}
	return &ModelSearchService{
		searchCache: search.NewSearchCache(search.DefaultSearchCacheTTL),
		backend:     resolver, cards: search.NewCardCache(),
		appsConfig:   appsConfig,
		pricingStore: pricingStore,
	}
}

// Start launches the search-cache TTL reaper. Idempotent and nil-safe.
func (s *ModelSearchService) Start(_ context.Context) {
	if s == nil {
		return
	}
	if s.searchCache != nil {
		s.searchCache.Start()
	}
}

// Stop halts the search-cache TTL reaper. Idempotent and nil-safe.
func (s *ModelSearchService) Stop(_ context.Context) {
	if s == nil {
		return
	}
	if s.searchCache != nil {
		s.searchCache.Stop()
	}
}

// HandleSearchPublic is the entry point for GET /zzrouter/search. The
// `type` query parameter dispatches to one of three sub-flows: model
// search across providers, provider catalog enumeration, or popular-
// models browse. Parameter validation is centralized here so the
// dispatchers can assume well-formed input.
func (s *ModelSearchService) HandleSearchPublic(c *gin.Context) {
	query, ok := searchTextQuery(c)
	if !ok || !validateSingleQuery(c, "sort", "order", "cursor", "since", "open_source") {
		return
	}
	if c.Query("since") != "" || c.Query("open_source") != "" {
		BadRequest(c, "since and open_source filters are supported only by the template catalog")
		return
	}
	// Validate required parameters. `type` uses an enum validator so the
	// 400 response lists every valid value — agents can parse the allowed
	// set without a separate schema call.
	params := ValidateParams(c, []ParamRule{
		RequiredEnumQueryParam("type", []string{"models", "providers", "popular"}),
		OptionalQueryParam("cursor"),
		OptionalQueryParam("provider"),
		OptionalQueryParam("author"),
		OptionalQueryParam("tags"),
		OptionalQueryParam("logic"),
		OptionalQueryParam("sort"),
		{Name: "order", Source: ParamSourceQuery, Default: "desc", AllowedValues: []string{"asc", "desc"}},
		BoolQueryParam("full"),
		IntQueryParam("limit", 20, 1, 1000),
		IntQueryParam("offset", 0, 0, 1000000),
	})
	if params == nil {
		return
	}

	searchType := params.GetString("type")
	provider := strings.ToLower(params.GetString("provider"))
	if provider == "" {
		provider = "all"
	}
	author := params.GetString("author")
	tagsParam := params.GetString("tags")
	tagLogic := params.GetString("logic")
	if tagLogic == "" {
		tagLogic = "AND"
	}
	sort := params.GetString("sort")
	if sort == "" {
		switch provider {
		case "all", "huggingface", "hf":
			sort = "trendingScore"
		case "ollama":
			if query == "" {
				sort = "downloads"
			}
		}
	}
	order := params.GetString("order")
	full := params.GetBool("full")
	limit := params.GetInt("limit")
	offset := params.GetInt("offset")

	// Parse tags if provided
	var tags []string
	if tagsParam != "" {
		tags = strings.Split(tagsParam, ",")
		// Trim whitespace from each tag and remove empty strings
		var cleanTags []string
		for _, tag := range tags {
			trimmed := strings.TrimSpace(tag)
			if trimmed != "" {
				cleanTags = append(cleanTags, trimmed)
			}
		}
		tags = cleanTags
	}

	// Validate tag logic parameter
	tagLogic = strings.ToUpper(tagLogic)
	if tagLogic != "AND" && tagLogic != "OR" {
		BadRequest(c, "Invalid logic parameter: logic must be either AND or OR")
		return
	}

	// Create search client with timeout
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	enrich := c.Query("enrich") == "true"

	// Dispatch by type. The enum is already validated by RequiredEnumQueryParam
	// in the ValidateParams call above; this switch is exhaustive and the
	// default branch is a defensive fallback that should be unreachable.
	switch searchType {
	case "models":
		s.handleModelsSearch(c, ctx, query, provider, author, tags, tagLogic, sort, order, full, enrich, limit, offset, params.GetString("cursor"))
	case "providers":
		s.handleAppsSearch(c, ctx, query, limit, offset)
	case "popular":
		s.handlePopularModelsSearch(c, ctx, provider, enrich, limit, offset)
	default:
		InternalNodeError(c, fmt.Sprintf("unreachable: unknown search type %q passed enum validation", searchType))
	}
}

func (s *ModelSearchService) handleModelsSearch(c *gin.Context, ctx context.Context, query, provider, author string, tags []string, tagLogic, sort, order string, full, enrich bool, limit, offset int, cursor string) {
	// Parse provider
	searchProvider, err := search.ParseProvider(provider)
	if err != nil {
		BadRequest(c, "Invalid provider parameter: provider must be one of: huggingface, ollama, all")
		return
	}

	// Parse sort direction
	direction := search.ParseSortDirection(order)

	// Build search parameters
	params := search.SearchParams{
		Provider:  searchProvider,
		Search:    query,
		Author:    author,
		Filter:    tags,
		Sort:      sort,
		Direction: direction,
		Full:      full,
		Pagination: search.PaginationParams{
			Limit:  limit,
			Offset: offset,
			Cursor: cursor,
		},
	}

	// Validate parameters
	if err := params.Validate(); err != nil {
		BadRequest(c, "Invalid search parameters: "+err.Error())
		return
	}

	targetName := string(searchProvider)
	if searchProvider == search.ProviderAll {
		targetName = "huggingface"
	}
	target, ok := s.backend.Target(targetName)
	if (!ok && searchProvider != search.ProviderOllama) || (target != nil && target.Cloud && !target.Configured) {
		s.connectionUnavailable(c, targetName)
		return
	}
	revision := ""
	if target != nil {
		revision = target.Revision
	}
	cacheKey := search.SearchCacheKey(params, tagLogic, enrich, revision)
	if cached := s.searchCache.Get(cacheKey); cached != nil {
		respondSuccess(c, "Search completed", cached)
		return
	}

	// Execute search based on provider
	if searchProvider == search.ProviderHuggingFace || searchProvider == search.ProviderAll {
		client := search.NewClient(search.ClientOptions{Target: target})
		results, err := client.SearchModels(ctx, params)
		if err != nil {
			InternalNodeError(c, "Search failed: "+err.Error())
			return
		}

		// Node-side post-processing for AND logic
		filteredResults := results.Results
		if len(tags) > 0 && tagLogic == "AND" {
			filteredResults = search.FilterModelsByTags(results.Results, tags, tagLogic)
		}

		resp := gin.H{
			"type":           "models",
			"query":          query,
			"provider":       provider,
			"results":        filteredResults,
			"total":          len(filteredResults),
			"is_total_exact": false,
			"pagination": gin.H{
				"limit":    limit,
				"offset":   offset,
				"has_next": results.HasMore,
				"cursor":   results.Cursor,
				"has_prev": offset > 0,
			},
		}
		s.completeSearch(c, "Search completed", resp, cacheKey)
		return
	}

	// Cloud search providers (OpenRouter, Cloudflare, etc.) — registry-driven
	if search.IsCloudSearchProvider(searchProvider) {
		results, err := search.SearchCloudTarget(ctx, target, query, 0)
		if err != nil {
			InternalNodeError(c, fmt.Sprintf("%s search failed: %s", searchProvider, err.Error()))
			return
		}

		search.NormalizeCloudResults(results)
		search.SortCloudResults(results, sort, direction)
		total := len(results)
		results = results[min(offset, total):min(offset+limit, total)]
		if enrich {
			s.enrichCloudResultsWithMetadata(results, string(searchProvider))
		}
		search.EnrichCloudResultsWithURL(results, searchProvider)

		resp := gin.H{
			"type":           "models",
			"query":          query,
			"provider":       string(searchProvider),
			"results":        results,
			"total":          total,
			"is_total_exact": true,
			"pagination": gin.H{
				"limit":    limit,
				"offset":   offset,
				"has_next": offset+limit < total,
				"has_prev": offset > 0,
			},
		}
		s.completeSearch(c, "Search completed", resp, cacheKey)
		return
	}

	if searchProvider == search.ProviderOllama {
		results, err := search.SearchOllamaModelsPaginated(ctx, params)
		if err != nil {
			InternalNodeError(c, "Ollama search failed: "+err.Error())
			return
		}
		s.completeSearch(c, "Search completed", gin.H{
			"type": "models", "query": query, "provider": "ollama", "results": results.Results, "total": results.Total, "is_total_exact": results.IsTotalExact,
			"pagination": gin.H{"limit": limit, "offset": offset, "has_next": results.HasMore, "has_prev": offset > 0},
		}, cacheKey)
		return
	}

	InternalNodeError(c, "Unsupported search configuration: Unable to process search request")
}

func (s *ModelSearchService) handleAppsSearch(c *gin.Context, _ context.Context, query string, limit, offset int) {
	// Re-register cloud search providers to pick up newly enabled providers since startup.
	search.RegisterCloudProvidersFromConfig(s.appsConfig, s.backend)
	cfg := s.appsConfig()

	// Config-driven: collect active search registries from enabled providers
	activeRegistries := s.collectActiveRegistries()

	var apps []gin.H

	// Emit non-cloud registries (huggingface, ollama) driven by provider config search config
	for registryKey := range activeRegistries {
		svc, exists := cfg.LookupApp(registryKey)
		if !exists || svc.IsCloudProvider() {
			continue // cloud registries are handled below
		}
		if svc.Search == nil {
			continue // no search config — not a registry
		}
		name := svc.Name
		if name == "" {
			name = registryKey
		}
		sortOptions := svc.Search.SortOptions
		if len(sortOptions) == 0 {
			sortOptions = []string{"newest", "name"}
		}
		clientSorts := svc.Search.ClientSorts
		if len(clientSorts) == 0 {
			clientSorts = []string{"name", "newest"}
		}
		endpoint := ""
		modelURL := ""
		if svc.Runtime != nil {
			endpoint = svc.Runtime.Endpoint
			modelURL = svc.Runtime.ModelURLFmt
		}
		apps = append(apps, gin.H{
			"name":               registryKey,
			"display_name":       name,
			"description":        svc.Description,
			"supported_features": []string{"search"},
			"base_url":           endpoint,
			"model_url_fmt":      modelURL,
			"icon":               ui.GetProviderEmojiRaw(registryKey),
			"sort_options":       sortOptions,
			"client_sorts":       clientSorts,
			"client_filter":      svc.Search.ClientFilter,
			"has_tags":           svc.Search.HasTags,
			"configured":         true,
			"enabled":            true,
		})
	}

	// Emit cloud search providers — driven by search.CloudProviders() registry
	for _, name := range search.CloudProviders() {
		entry, _ := search.GetCloudProvider(name)
		meta := entry.Meta

		configured, enabled := s.cloudProviderStatus(string(name))

		apps = append(apps, gin.H{
			"name":               string(name),
			"display_name":       meta.DisplayName,
			"description":        meta.Description,
			"supported_features": []string{"search"},
			"base_url":           meta.BaseURL,
			"model_url_fmt":      meta.ModelURLFmt,
			"icon":               ui.GetProviderEmojiRaw(string(name)),
			"sort_options":       meta.SortOptions,
			"client_sorts":       meta.ClientSorts,
			"client_filter":      meta.ClientFilter,
			"has_tags":           meta.HasTags,
			"is_cloud":           true,
			"configured":         configured,
			"enabled":            enabled,
		})
	}

	// Filter apps if query provided
	if query != "" {
		filtered := []gin.H{}
		queryLower := strings.ToLower(query)
		for _, provider := range apps {
			rawName, _ := provider["name"].(string)
			rawDisplay, _ := provider["display_name"].(string)
			name := strings.ToLower(rawName)
			displayName := strings.ToLower(rawDisplay)
			if strings.Contains(name, queryLower) || strings.Contains(displayName, queryLower) {
				filtered = append(filtered, provider)
			}
		}
		apps = filtered
	}

	respondSuccess(c, "Providers retrieved", gin.H{
		"type":           "providers",
		"query":          query,
		"results":        apps,
		"total":          len(apps),
		"is_total_exact": true,
		"pagination": gin.H{
			"limit":    limit,
			"offset":   offset,
			"has_next": false,
			"has_prev": false,
		},
	})
}

// cloudProviderStatus checks provider config for a cloud provider's configuration status.
// Returns (configured, enabled) — configured means the provider exists in provider config,
// enabled means it's configured AND has enabled: true AND API credentials are present.
func (s *ModelSearchService) cloudProviderStatus(name string) (bool, bool) {
	cfg := s.appsConfig()
	if cfg == nil {
		return false, false
	}
	appCfg, exists := cfg.LookupApp(name)
	if !exists {
		return false, false
	}
	return true, appCfg.IsEnabled() && s.backend.Available(name)
}

// collectActiveRegistries determines which search registries should be active
// based on enabled providers in provider config. Config-driven via search.registry fields.
// Returns a set of registry keys (e.g., "huggingface", "ollama").
func (s *ModelSearchService) collectActiveRegistries() map[string]bool {
	registries := make(map[string]bool)
	cfg := s.appsConfig()
	if cfg == nil {
		return registries
	}
	cfg.RangeApps(func(key string, svc pkgConfig.ServiceConfig) bool {
		if !svc.IsEnabled() {
			return true
		}
		// Search registries (mode: registry) are always active when enabled
		if svc.IsModelHubRegistry() && svc.Search != nil {
			registries[key] = true
			return true
		}
		if svc.Search != nil && svc.Search.Registry != "" {
			// This provider activates another registry (e.g., vllm → huggingface)
			registries[svc.Search.Registry] = true
		} else if svc.Search != nil && len(svc.Search.SortOptions) > 0 {
			// This provider IS a registry with search config (e.g., ollama)
			registries[key] = true
		}
		// Cloud providers without search.registry are handled by search.CloudProviders()
		return true
	})
	return registries
}

func (s *ModelSearchService) handlePopularModelsSearch(c *gin.Context, ctx context.Context, provider string, enrich bool, limit, offset int) {
	searchProvider, err := search.ParseProvider(provider)
	if err != nil {
		BadRequest(c, "Invalid provider parameter: provider must be one of: huggingface, ollama, all")
		return
	}

	targetName := string(searchProvider)
	if searchProvider == search.ProviderAll {
		targetName = "huggingface"
	}
	target, ok := s.backend.Target(targetName)
	if (!ok && searchProvider != search.ProviderOllama) || (target != nil && target.Cloud && !target.Configured) {
		s.connectionUnavailable(c, targetName)
		return
	}

	if searchProvider == search.ProviderHuggingFace || searchProvider == search.ProviderAll {
		params := search.SearchParams{
			Provider:  search.ProviderHuggingFace,
			Sort:      "downloads",
			Direction: -1,
			Pagination: search.PaginationParams{
				Limit:  limit,
				Offset: offset,
			},
		}

		client := search.NewClient(search.ClientOptions{Target: target})
		results, err := client.SearchModels(ctx, params)
		if err != nil {
			InternalNodeError(c, "Failed to get popular models: "+err.Error())
			return
		}

		s.completeSearch(c, "Popular models retrieved", gin.H{
			"type":           "popular",
			"provider":       "huggingface",
			"results":        results.Results,
			"total":          results.Total,
			"is_total_exact": results.IsTotalExact,
			"pagination": gin.H{
				"limit":    limit,
				"offset":   offset,
				"has_next": offset+limit < results.Total,
				"has_prev": offset > 0,
			},
		}, "")
		return
	}

	if searchProvider == search.ProviderOllama {
		results, err := search.GetOllamaPopularModels(ctx, limit)
		if err != nil {
			InternalNodeError(c, "Failed to get popular Ollama models: "+err.Error())
			return
		}

		s.completeSearch(c, "Popular models retrieved", gin.H{
			"type":           "popular",
			"provider":       "ollama",
			"results":        results.Results,
			"total":          results.Total,
			"is_total_exact": results.IsTotalExact,
			"pagination": gin.H{
				"limit":    limit,
				"offset":   offset,
				"has_next": false,
				"has_prev": false,
			},
		}, "")
		return
	}

	// Cloud search providers — popular = search with empty query
	if search.IsCloudSearchProvider(searchProvider) {
		results, err := search.SearchCloudTarget(ctx, target, "", limit)
		if err != nil {
			InternalNodeError(c, fmt.Sprintf("Failed to get %s models: %s", searchProvider, err.Error()))
			return
		}

		search.NormalizeCloudResults(results)
		if enrich {
			s.enrichCloudResultsWithMetadata(results, string(searchProvider))
		}
		search.EnrichCloudResultsWithURL(results, searchProvider)

		s.completeSearch(c, "Popular models retrieved", gin.H{
			"type":           "popular",
			"provider":       string(searchProvider),
			"results":        results,
			"total":          len(results),
			"is_total_exact": true,
			"pagination": gin.H{
				"limit":    limit,
				"offset":   offset,
				"has_next": false,
				"has_prev": false,
			},
		}, "")
		return
	}

	InternalNodeError(c, "Unsupported provider for popular models: Unable to get popular models for specified provider")
}

// handleGetModelCard returns model card details from the search registries
// (HuggingFace card API, Ollama, cloud provider catalogs). For cloud
// providers it also merges pricing-store metadata where the upstream
// data is missing fields (provider-native data takes priority).
func (s *ModelSearchService) handleGetModelCard(c *gin.Context) {
	provider := c.Param("provider")
	modelID := c.Param("id")
	modelID = strings.TrimPrefix(modelID, "/")
	modelID = strings.TrimSuffix(modelID, "/card")

	decodedID, err := url.QueryUnescape(modelID)
	if err != nil {
		BadRequest(c, "Invalid model ID")
		return
	}

	searchProvider, err := search.ParseProvider(provider)
	if err != nil {
		BadRequest(c, "Invalid provider")
		return
	}
	provider = string(searchProvider)

	if searchProvider == search.ProviderHuggingFace && !metadata.ValidHuggingFaceID(decodedID) {
		BadRequest(c, "Hugging Face model id must be in org/repo form")
		return
	}
	if searchProvider == search.ProviderOllama {
		base, _, _ := strings.Cut(decodedID, ":")
		if !search.ValidOllamaModelName(base) {
			BadRequest(c, "Invalid Ollama catalog identifier")
			return
		}
	}
	target, ok := s.backend.Target(provider)
	if (!ok && searchProvider != search.ProviderOllama) || (target != nil && target.Cloud && !target.Configured) {
		s.connectionUnavailable(c, provider)
		return
	}
	ctx := c.Request.Context()

	// Cloud providers: return raw map with all native fields + pricing-store enrichment
	if search.IsCloudSearchProvider(searchProvider) {
		raw, err := search.GetCloudModelRaw(ctx, searchProvider, decodedID, target)
		if err != nil {
			if isModelCardNotFound(err) {
				NotFound(c, fmt.Sprintf("model %q not found in %s registry", decodedID, provider))
				return
			}
			InternalNodeError(c, fmt.Sprintf("Failed to fetch model card: %s", err.Error()))
			return
		}
		// Enrich with pricing-store metadata (provider data takes priority)
		if meta := s.lookupModelMetadata(provider, decodedID); meta != nil {
			search.MergeMetadataWithPriority(raw, meta)
		}
		respondSuccess(c, "Model card retrieved", s.discoveryResult(provider, raw))
		return
	}

	// Local providers: use ModelCard struct
	modelCard, err := s.cards.GetModelCard(ctx, searchProvider, decodedID, target)
	if err != nil {
		if isModelCardNotFound(err) {
			NotFound(c, fmt.Sprintf("model %q not found in %s registry", decodedID, provider))
			return
		}
		InternalNodeError(c, fmt.Sprintf("Failed to fetch model card: %s", err.Error()))
		return
	}

	siblings := modelCard.Siblings
	totalSize := modelCard.Size
	if searchProvider == search.ProviderHuggingFace && len(siblings) > 0 {
		enrichedSiblings, err := search.NewClient(search.ClientOptions{Target: target}).EnrichSiblingsWithSizes(ctx, decodedID, siblings)
		if err == nil {
			siblings = enrichedSiblings
			totalSize = 0
			for _, sibling := range siblings {
				totalSize += int64(sibling.Size)
			}
		}
	}

	raw := gin.H{
		"provider":     provider,
		"model_id":     decodedID,
		"name":         modelCard.Name,
		"description":  modelCard.Description,
		"size":         totalSize,
		"tags":         modelCard.Tags,
		"license":      modelCard.License,
		"author":       modelCard.Author,
		"pipeline_tag": modelCard.PipelineTag,
		"downloads":    modelCard.Downloads,
		"likes":        modelCard.Likes,
		"created_at":   modelCard.CreatedAt,
		"url":          modelCard.URL,
		"siblings":     siblings,
		"readme":       modelCard.README,
	}
	encoded, encodeErr := json.Marshal(raw)
	if encodeErr != nil {
		InternalNodeError(c, "Unable to encode model card")
		return
	}
	var native map[string]any
	if err := json.Unmarshal(encoded, &native); err != nil {
		InternalNodeError(c, "Unable to normalize model card")
		return
	}
	respondSuccess(c, "Model card retrieved", s.discoveryResult(provider, native))
}

// lookupModelMetadata returns pricing-store metadata for a single model as a map.
// Returns nil if no data is available.
func (s *ModelSearchService) lookupModelMetadata(provider, modelID string) map[string]any {
	if s.pricingStore == nil || s.pricingStore.ModelCount() == 0 {
		return nil
	}

	p, found := s.pricingStore.LookupByProvider(provider, modelID)
	if !found {
		p, found = s.pricingStore.Lookup(modelID)
	}
	if !found {
		return nil
	}

	entry := map[string]any{
		"pricing": map[string]any{
			"prompt":     fmt.Sprintf("%g", p.InputCostPerToken),
			"completion": fmt.Sprintf("%g", p.OutputCostPerToken),
		},
	}
	if ctx := p.ContextWindow(); ctx > 0 {
		entry["context_length"] = ctx
	}
	if p.MaxOutputTokens > 0 {
		entry["max_output_tokens"] = p.MaxOutputTokens
	}
	if p.Mode != "" {
		entry["mode"] = p.Mode
	}
	if p.DeprecationDate != "" {
		entry["deprecation_date"] = p.DeprecationDate
	}

	// Capabilities from boolean flags
	var caps []string
	if p.SupportsFunctionCalling {
		caps = append(caps, "tool_calling")
	}
	if p.SupportsParallelToolCalls {
		caps = append(caps, "parallel_tools")
	}
	if p.SupportsVision {
		caps = append(caps, "vision")
	}
	if len(caps) > 0 {
		entry[pricing.MetadataKeyCapabilities] = caps
	}

	// Derive input types from capabilities
	inputTypes := []string{"text"}
	if p.SupportsVision {
		inputTypes = append(inputTypes, "image")
	}
	if p.InputCostPerAudioToken > 0 {
		inputTypes = append(inputTypes, "audio")
	}
	if len(inputTypes) > 1 {
		entry[pricing.MetadataKeyInputTypes] = inputTypes
	}

	return entry
}

// enrichCloudResultsWithMetadata merges pricing-store metadata into cloud
// search results. Provider-native data takes priority — pricing-store data
// only fills gaps. Must be called after NormalizeCloudResults (uses api_id
// as the lookup key).
func (s *ModelSearchService) enrichCloudResultsWithMetadata(results []any, provider string) {
	if s.pricingStore == nil || s.pricingStore.ModelCount() == 0 {
		return
	}

	for _, result := range results {
		m, isMap := result.(map[string]any)
		if !isMap {
			continue
		}
		apiID, _ := m["api_id"].(string)
		if apiID == "" {
			continue
		}

		meta := s.lookupModelMetadata(provider, apiID)
		if meta == nil {
			continue
		}

		search.MergeMetadataWithPriority(m, meta)
	}
}

// isModelCardNotFound preserves typed upstream absence without interpreting prose.
func isModelCardNotFound(err error) bool { return errors.Is(err, search.ErrModelNotFound) }
