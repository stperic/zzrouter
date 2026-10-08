package search

// Ollama catalog-side types + helpers. This file (together with
// ollama_scraper.go) scrapes the PUBLIC ollama.com/library web catalog
// for discovery — NOT the user's local Ollama daemon. The daemon-side
// client lives at pkg/modelregistry/source/ollama (ollama.Connector);
// see the top of that package for the inverse pointer.

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// OllamaModelInfo represents information about an Ollama model
type OllamaModelInfo struct {
	Name       string        `json:"name"`
	Model      string        `json:"model"`
	ModifiedAt string        `json:"modified_at"` // Relative date string (e.g., "1 month ago")
	DaysAgo    int           `json:"days_ago"`    // Days since last update (for sorting)
	Size       int64         `json:"size"`
	Digest     string        `json:"digest"`
	Details    OllamaDetails `json:"details"`
	Tags       int           `json:"tags"`      // Number of available versions/tags
	Pulls      int           `json:"pulls"`     // Number of pulls/downloads
	URL        string        `json:"url"`       // URL to model page for lazy loading details
	HasCloud   bool          `json:"has_cloud"` // True if model has cloud-served variants
}

// OllamaDetails contains additional model details
type OllamaDetails struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
}

// OllamaTagInfo represents a single tag/version of an Ollama model
type OllamaTagInfo struct {
	Name    string `json:"name"`    // Tag name (e.g., "latest", "7b", "8b-instruct-q5_K_S")
	Size    string `json:"size"`    // Human-readable size (e.g., "5.2GB")
	Context string `json:"context"` // Context window (e.g., "128K")
	Input   string `json:"input"`   // Input type (e.g., "Text", "Image+Text")
	Digest  string `json:"digest"`  // SHA256 digest
}

// OllamaTagsResponse represents the response from /api/tags
type OllamaTagsResponse struct {
	Models []OllamaModelInfo `json:"models"`
}

// Regex to find parameter counts like "7b", "8.5b", "70b" in a model name.
var paramRegex = regexp.MustCompile(`(\d+(\.\d+)?)b`)

// EstimateOllamaModelSize estimates model size based on parameter count and quantization.
func EstimateOllamaModelSize(modelName string) int64 {
	name := strings.ToLower(modelName)
	var paramCount = 7e9 // Default to 7 billion parameters if not found

	// Find the first match for a parameter pattern (e.g., "7b", "14b")
	matches := paramRegex.FindStringSubmatch(name)
	if len(matches) > 1 {
		// The first submatch is the number (e.g., "7", "8.5")
		val, err := strconv.ParseFloat(matches[1], 64)
		if err == nil {
			paramCount = val * 1e9 // Convert to billions
		}
	}

	// A more realistic multiplier for common Ollama quantizations (like Q4_K_M).
	// This accounts for the overhead and mixed precision, yielding a better estimate
	// than the theoretical 0.5 bytes/param for pure 4-bit.
	const realisticBytesPerParam = 0.70
	estimatedSize := paramCount * realisticBytesPerParam

	return int64(estimatedSize)
}

// filterOllamaModels filters Ollama models based on search criteria
func filterOllamaModels(models []OllamaModelInfo, query string) []OllamaModelInfo {
	if query == "" {
		return models
	}

	var filtered []OllamaModelInfo
	for _, model := range models {
		// Use wildcard matching for patterns with * or ?
		if strings.Contains(query, "*") || strings.Contains(query, "?") {
			match, err := filepath.Match(query, model.Name)
			if err == nil && match {
				filtered = append(filtered, model)
			}
		} else {
			// Use substring matching for regular queries
			if strings.Contains(strings.ToLower(model.Name), strings.ToLower(query)) {
				filtered = append(filtered, model)
			}
		}
	}
	return filtered
}

// sortOllamaModels sorts Ollama models based on criteria
func sortOllamaModels(models []OllamaModelInfo, sortBy string, direction int) []OllamaModelInfo {
	if sortBy == "" {
		sortBy = "downloads" // default for Ollama
	}

	switch sortBy {
	case "name":
		sort.Slice(models, func(i, j int) bool {
			if direction < 0 { // descending (Z-A)
				return models[i].Name > models[j].Name
			}
			return models[i].Name < models[j].Name // ascending (A-Z)
		})
	case "downloads", "pulls", "popular":
		sort.Slice(models, func(i, j int) bool {
			if direction < 0 { // descending (most downloads first)
				return models[i].Pulls > models[j].Pulls
			}
			return models[i].Pulls < models[j].Pulls // ascending
		})
	case "modified", "date", "updated", "newest":
		sort.Slice(models, func(i, j int) bool {
			// Use DaysAgo for proper sorting (lower = more recent)
			if direction < 0 { // descending (newest first)
				return models[i].DaysAgo < models[j].DaysAgo
			}
			return models[i].DaysAgo > models[j].DaysAgo // ascending (oldest first)
		})
	case "size":
		sort.Slice(models, func(i, j int) bool {
			if direction < 0 { // descending
				return models[i].Size > models[j].Size
			}
			return models[i].Size < models[j].Size // ascending
		})
	}
	return models
}

// paginateOllamaModels applies pagination to Ollama models
func paginateOllamaModels(models []OllamaModelInfo, pagination PaginationParams) *SearchResponse[OllamaModelInfo] {
	total := len(models)
	start := pagination.Offset
	end := start + pagination.Limit

	if start >= total {
		// Offset is beyond available results
		return &SearchResponse[OllamaModelInfo]{
			Results:      []OllamaModelInfo{},
			Total:        total,
			Offset:       pagination.Offset,
			Limit:        pagination.Limit,
			HasMore:      false,
			IsTotalExact: true, // Ollama provides exact totals
		}
	}

	if end > total || pagination.Limit <= 0 {
		end = total
	}

	results := models[start:end]
	hasMore := end < total

	return &SearchResponse[OllamaModelInfo]{
		Results:      results,
		Total:        total,
		Offset:       pagination.Offset,
		Limit:        pagination.Limit,
		HasMore:      hasMore,
		IsTotalExact: true, // Ollama provides exact totals
	}
}

// GetOllamaModels fetches locally-downloadable Ollama models from the library.
// Cloud-only models are excluded (use Ollama Cloud search for those).
// Uses web scraping since Ollama doesn't provide a public API for the full library.
// See: https://github.com/ollama/ollama/issues/7751
func GetOllamaModels() ([]OllamaModelInfo, error) {
	all, err := ScrapeOllamaLibrary()
	if err != nil {
		return nil, err
	}
	return locallyRunnable(all), nil
}

// locallyRunnable drops the models that can only be served from Ollama's
// cloud; those belong to ollama-cloud search. A "cloud" badge alone does
// not make a model cloud-only — gpt-oss and qwen3.5 carry one and still
// ship downloadable tags. The card's tag count is what separates them:
// no tags means there is nothing to pull and run here.
func locallyRunnable(all []OllamaModelInfo) []OllamaModelInfo {
	local := make([]OllamaModelInfo, 0, len(all))
	for _, m := range all {
		if m.HasCloud && m.Tags == 0 {
			continue
		}
		local = append(local, m)
	}
	return local
}

// GetOllamaModelsWithSort returns Ollama models sorted by the specified criteria
func GetOllamaModelsWithSort(sortBy string, limit int) (*SearchResponse[OllamaModelInfo], error) {
	models, err := GetOllamaModels()
	if err != nil {
		return nil, err
	}

	// Sort by specified criteria (descending by default)
	sorted := sortOllamaModels(models, sortBy, -1)

	if limit > 0 && limit < len(sorted) {
		sorted = sorted[:limit]
	}

	return &SearchResponse[OllamaModelInfo]{
		Results:      sorted,
		Total:        len(sorted),
		Offset:       0,
		Limit:        len(sorted),
		HasMore:      false,
		IsTotalExact: true,
	}, nil
}

// GetOllamaPopularModels returns the most popular Ollama models
func GetOllamaPopularModels(limit int) (*SearchResponse[OllamaModelInfo], error) {
	return GetOllamaModelsWithSort("downloads", limit)
}

// SearchOllamaModelsPaginated searches Ollama models by name with pagination
func SearchOllamaModelsPaginated(query string, pagination PaginationParams) (*SearchResponse[OllamaModelInfo], error) {
	models, err := GetOllamaModels()
	if err != nil {
		return nil, err
	}

	filtered := filterOllamaModels(models, query)

	return paginateOllamaModels(filtered, pagination), nil
}
