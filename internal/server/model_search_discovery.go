package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/search"
	"github.com/stperic/zzrouter/pkg/utils"
)

// completeSearch adds one authoritative presentation contract while retaining
// each provider's native response fields. Cached entries already contain it.
func (s *ModelSearchService) completeSearch(c *gin.Context, message string, payload gin.H, cacheKey string) {
	name, _ := payload["provider"].(string)
	provider, err := search.ParseProvider(name)
	if err != nil {
		InternalNodeError(c, "Unable to normalize discovery provider")
		return
	}
	if provider == search.ProviderAll {
		provider = search.ProviderHuggingFace
	}
	payload["provider"] = provider.String()
	encoded, err := json.Marshal(payload["results"])
	if err != nil {
		InternalNodeError(c, "Unable to encode discovery results")
		return
	}
	var native []map[string]any
	if err := json.Unmarshal(encoded, &native); err != nil {
		InternalNodeError(c, "Unable to normalize discovery results")
		return
	}
	results := make([]metadata.DiscoveryResult, 0, len(native))
	for _, row := range native {
		results = append(results, s.discoveryResult(provider.String(), row))
	}
	payload["results"] = results
	if cacheKey != "" {
		s.searchCache.Set(cacheKey, payload)
	}
	respondSuccess(c, message, payload)
}

func (s *ModelSearchService) discoveryResult(provider string, raw map[string]any) metadata.DiscoveryResult {
	d := metadata.Discovery{
		Provider: provider, APIID: discoveryString(raw, "api_id", "model_id", "id", "name", "model"),
		DisplayName: discoveryString(raw, "display_name", "name", "id", "model_id", "model"),
		Description: discoveryString(raw, "description"), README: discoveryString(raw, "readme"),
		Downloads: int(discoveryNumber(raw, "downloads", "pulls")), Likes: int(discoveryNumber(raw, "likes")),
		TrendingScore: discoveryNumber(raw, "trendingScore"), CreatedAt: discoveryString(raw, "createdAt", "created_at"),
		ModifiedAt: discoveryString(raw, "lastModified", "modified_at", "last_modified", "updated_at"),
		URL:        discoveryString(raw, "url"), SizeBytes: int64(discoveryNumber(raw, "size")),
		ContextLength: int(discoveryNumber(raw, "context_length", "context_window", "max_input_tokens")),
	}
	if d.DisplayName == "" {
		d.DisplayName = d.APIID
	}
	if d.CreatedAt == "" {
		if n := discoveryNumber(raw, "created"); n > 0 {
			d.CreatedAt = time.Unix(int64(n), 0).UTC().Format(time.RFC3339)
		}
	}
	d.HasCloud, _ = raw["has_cloud"].(bool)
	d.IsCloud = search.IsCloudSearchProvider(search.Provider(provider)) || (provider == constants.RepoOllama && (strings.HasSuffix(d.APIID, ":cloud") || strings.HasSuffix(d.APIID, "-cloud")))
	if tags, ok := raw["tags"].([]string); ok {
		d.Tags = append([]string(nil), tags...)
	}
	if tags, ok := raw["tags"].([]any); ok {
		for _, tag := range tags {
			if value, ok := tag.(string); ok {
				d.Tags = append(d.Tags, value)
			}
		}
	}
	if prices, ok := raw["pricing"].(map[string]any); ok {
		d.PricePrompt = discoveryString(prices, "prompt")
		d.PriceComplete = discoveryString(prices, "completion")
	}
	if provider == constants.RepoOllama {
		d.VariantCount = int(discoveryNumber(raw, "tags"))
	}
	if provider == constants.RepoHuggingFace {
		if st, ok := raw["safetensors"].(map[string]any); ok {
			d.ParameterCount = int64(discoveryNumber(st, "total"))
		}
		if d.ParameterCount == 0 {
			if gguf, ok := raw["gguf"].(map[string]any); ok {
				d.ParameterCount = int64(discoveryNumber(gguf, "total"))
			}
		}
		var files []metadata.TreeFileEntry
		complete := true
		if siblings, ok := raw["siblings"].([]any); ok {
			for _, sibling := range siblings {
				if file, ok := sibling.(map[string]any); ok {
					size := int64(discoveryNumber(file, "size"))
					if size <= 0 {
						complete = false
					}
					files = append(files, metadata.TreeFileEntry{Name: discoveryString(file, "rfilename"), Size: size})
				}
			}
		}
		features, _ := featureFileGlobs(s.appsConfig(), "")
		variants := buildVariantsResponse(d.APIID, files, features)
		d.VariantCount = len(variants.Variants)
		if d.SizeBytes == 0 && complete {
			d.SizeBytes = variants.TotalSizeBytes
		}
		if d.URL == "" {
			d.URL = "https://huggingface.co/" + d.APIID
		}
	}
	return metadata.DiscoveryResult{Discovery: d, Native: raw}
}

func discoveryString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := raw[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func discoveryNumber(raw map[string]any, keys ...string) float64 {
	for _, key := range keys {
		switch value := raw[key].(type) {
		case float64:
			if value != 0 {
				return value
			}
		case int:
			if value != 0 {
				return float64(value)
			}
		case int64:
			if value != 0 {
				return float64(value)
			}
		case json.Number:
			n, _ := value.Float64()
			if n != 0 {
				return n
			}
		case string:
			n, _ := strconv.ParseFloat(value, 64)
			if n != 0 {
				return n
			}
		}
	}
	return 0
}

func (s *ModelSearchService) connectionUnavailable(c *gin.Context, provider string) {
	RespondWithProblemOpts(c, http.StatusServiceUnavailable, "Provider Connection Unavailable",
		fmt.Sprintf("Discovery connection for %s is unavailable", provider), ProblemOpts{
			Code: string(httperr.CodeProviderConnectionUnavailable),
			Errors: []utils.ParamError{{
				Key: "provider", Code: string(httperr.CodeProviderConnectionUnavailable), Got: provider,
				Message: "The configured connection cannot be used for discovery.",
				Hint:    `Use zz_api_search(query="ProviderConnection") to inspect and repair this provider's coordinator connection, then retry discovery.`,
			}},
		})
}
