package modelregistry

import (
	"sort"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/utils"
)

// SortBy defines sorting criteria
type SortBy string

const (
	SortByName     SortBy = "name"
	SortBySize     SortBy = "size"
	SortByModified SortBy = "modified"
	SortByAdded    SortBy = "added"
	SortByFormat   SortBy = "format"
	SortBySource   SortBy = "source"
)

// SearchModels searches for models by name or pattern
// Uses MatchPattern for wildcard patterns, substring matching otherwise
func (r *Registry) SearchModels(query string) []*metadata.ModelMetadata {
	if query == "" {
		models, _ := r.ListAllModels()
		return models
	}

	hasWildcards := strings.ContainsAny(query, "*?[]")
	queryLower := strings.ToLower(query)

	filter := func(m *metadata.ModelMetadata) bool {
		if hasWildcards {
			return MatchPattern(m.Name, query) ||
				(m.SourceID != "" && MatchPattern(m.SourceID, query))
		}
		// Simple substring search (case-insensitive)
		return strings.Contains(strings.ToLower(m.Name), queryLower) ||
			strings.Contains(strings.ToLower(m.SourceID), queryLower) ||
			strings.Contains(strings.ToLower(m.FullPath), queryLower)
	}

	results, _ := r.filterModels(filter)
	return results
}

// FindModelsByPattern finds models matching a pattern
// Supports wildcards: * (any characters), ? (single character)
func (r *Registry) FindModelsByPattern(pattern string) []*metadata.ModelMetadata {
	if pattern == "" || pattern == "*" {
		models, _ := r.ListAllModels()
		return models
	}

	results, _ := r.filterModels(func(m *metadata.ModelMetadata) bool {
		return MatchPattern(m.Name, pattern)
	})
	return results
}

// ListModelsSorted returns all models sorted by the specified criteria
func (r *Registry) ListModelsSorted(sortBy SortBy, ascending bool) []*metadata.ModelMetadata {
	models, err := r.ListAllModels()
	if err != nil {
		return []*metadata.ModelMetadata{}
	}
	// Create a copy to avoid modifying the cached slice
	sorted := make([]*metadata.ModelMetadata, len(models))
	copy(sorted, models)
	sortModels(sorted, sortBy, ascending)
	return sorted
}

// GroupModelsBy groups models by the specified field
func (r *Registry) GroupModelsBy(field string) map[string][]*metadata.ModelMetadata {
	groups := make(map[string][]*metadata.ModelMetadata)

	_ = r.withModels(func(models []*metadata.ModelMetadata) error {
		for _, model := range models {
			var key string

			switch field {
			case "format":
				key = model.Format
			case "source":
				key = model.SourceRepo
			case "quantization":
				key = model.Quantization
				if key == "" {
					key = "none"
				}
			default:
				key = "unknown"
			}

			groups[key] = append(groups[key], model)
		}
		return nil
	})

	return groups
}

// ListModelsPaginated returns a paginated list of models
// Returns the models for the requested page and the total count
func (r *Registry) ListModelsPaginated(offset, limit int) ([]*metadata.ModelMetadata, int) {
	allModels, err := r.ListAllModels()
	if err != nil {
		return []*metadata.ModelMetadata{}, 0
	}
	total := len(allModels)

	// Validate offset and limit
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 20 // Default page size
	}

	// Calculate end index
	end := min(offset+limit, total)

	// Return empty slice if offset is beyond total
	if offset >= total {
		return []*metadata.ModelMetadata{}, total
	}

	return allModels[offset:end], total
}

// FindRecentModels returns models modified or added within the specified duration
func (r *Registry) FindRecentModels(since time.Duration) []*metadata.ModelMetadata {
	cutoff := utils.Now().Add(-since)

	results, _ := r.filterModels(func(m *metadata.ModelMetadata) bool {
		return m.Modified.After(cutoff) || m.AddedAt.After(cutoff)
	})

	// Sort by most recent first
	sort.Slice(results, func(i, j int) bool {
		iTime := results[i].Modified
		if results[i].AddedAt.After(iTime) {
			iTime = results[i].AddedAt
		}

		jTime := results[j].Modified
		if results[j].AddedAt.After(jTime) {
			jTime = results[j].AddedAt
		}

		return iTime.After(jTime)
	})

	return results
}

// FindModelsBySize returns models within a size range
func (r *Registry) FindModelsBySize(minSize, maxSize int64) []*metadata.ModelMetadata {
	results, _ := r.filterModels(func(m *metadata.ModelMetadata) bool {
		return (minSize == 0 || m.Size >= minSize) &&
			(maxSize == 0 || m.Size <= maxSize)
	})
	return results
}

// FindModelsByFormats returns models matching any of the specified formats
func (r *Registry) FindModelsByFormats(formats []string) []*metadata.ModelMetadata {
	if len(formats) == 0 {
		models, _ := r.ListAllModels()
		return models
	}

	// Create format lookup map
	formatMap := make(map[string]bool)
	for _, format := range formats {
		formatMap[strings.ToLower(format)] = true
	}

	results, _ := r.filterModels(func(m *metadata.ModelMetadata) bool {
		return formatMap[strings.ToLower(m.Format)]
	})
	return results
}

// FindModelsBySources returns models from any of the specified sources
func (r *Registry) FindModelsBySources(sources []string) []*metadata.ModelMetadata {
	if len(sources) == 0 {
		models, _ := r.ListAllModels()
		return models
	}

	// Create source lookup map
	sourceMap := make(map[string]bool)
	for _, source := range sources {
		sourceMap[strings.ToLower(source)] = true
	}

	results, _ := r.filterModels(func(m *metadata.ModelMetadata) bool {
		return sourceMap[strings.ToLower(m.SourceRepo)]
	})
	return results
}

// AdvancedSearch performs a multi-criteria search
type SearchCriteria struct {
	Query         string        // Text search in name/path/sourceID
	Formats       []string      // Filter by formats
	Sources       []string      // Filter by sources
	MinSize       int64         // Minimum size
	MaxSize       int64         // Maximum size
	ModifiedSince time.Duration // Modified within duration
	SortBy        SortBy        // Sort criteria
	Ascending     bool          // Sort direction
	Limit         int           // Max results (0 = no limit)
}

// AdvancedSearch performs a comprehensive search with multiple criteria
func (r *Registry) AdvancedSearch(criteria *SearchCriteria) []*metadata.ModelMetadata {
	// Start with all models. Copy up front because ListAllModels returns
	// the shared cached slice; downstream filter helpers make fresh
	// slices of their own, but the sort-only path (no filters, only
	// SortBy set) would otherwise sort the cache in place.
	allModels, err := r.ListAllModels()
	if err != nil {
		return []*metadata.ModelMetadata{}
	}
	results := append([]*metadata.ModelMetadata(nil), allModels...)

	// Apply text search
	if criteria.Query != "" {
		results = filterByQuery(results, criteria.Query)
	}

	// Apply format filter
	if len(criteria.Formats) > 0 {
		results = filterByFormats(results, criteria.Formats)
	}

	// Apply source filter
	if len(criteria.Sources) > 0 {
		results = filterBySources(results, criteria.Sources)
	}

	// Apply size filter
	if criteria.MinSize > 0 || criteria.MaxSize > 0 {
		results = filterBySize(results, criteria.MinSize, criteria.MaxSize)
	}

	// Apply time filter
	if criteria.ModifiedSince > 0 {
		cutoff := utils.Now().Add(-criteria.ModifiedSince)
		results = filterByTime(results, cutoff)
	}

	// Sort results
	if criteria.SortBy != "" {
		sortModels(results, criteria.SortBy, criteria.Ascending)
	}

	// Apply limit
	if criteria.Limit > 0 && len(results) > criteria.Limit {
		results = results[:criteria.Limit]
	}

	return results
}

// Helper functions for AdvancedSearch

func filterByQuery(models []*metadata.ModelMetadata, query string) []*metadata.ModelMetadata {
	filtered := make([]*metadata.ModelMetadata, 0)
	hasWildcards := strings.ContainsAny(query, "*?[]")

	queryLower := strings.ToLower(query)

	for _, model := range models {
		var matches bool
		if hasWildcards {
			// Use MatchPattern for wildcard patterns
			matches = MatchPattern(model.Name, query) ||
				(model.SourceID != "" && MatchPattern(model.SourceID, query))
		} else {
			// Simple substring search (case-insensitive)
			matches = strings.Contains(strings.ToLower(model.Name), queryLower) ||
				strings.Contains(strings.ToLower(model.SourceID), queryLower) ||
				strings.Contains(strings.ToLower(model.FullPath), queryLower)
		}
		if matches {
			filtered = append(filtered, model)
		}
	}

	return filtered
}

func filterByFormats(models []*metadata.ModelMetadata, formats []string) []*metadata.ModelMetadata {
	formatMap := make(map[string]bool)
	for _, f := range formats {
		formatMap[strings.ToLower(f)] = true
	}

	filtered := make([]*metadata.ModelMetadata, 0)
	for _, model := range models {
		if formatMap[strings.ToLower(model.Format)] {
			filtered = append(filtered, model)
		}
	}

	return filtered
}

func filterBySources(models []*metadata.ModelMetadata, sources []string) []*metadata.ModelMetadata {
	sourceMap := make(map[string]bool)
	for _, s := range sources {
		sourceMap[strings.ToLower(s)] = true
	}

	filtered := make([]*metadata.ModelMetadata, 0)
	for _, model := range models {
		if sourceMap[strings.ToLower(model.SourceRepo)] {
			filtered = append(filtered, model)
		}
	}

	return filtered
}

func filterBySize(models []*metadata.ModelMetadata, minSize, maxSize int64) []*metadata.ModelMetadata {
	filtered := make([]*metadata.ModelMetadata, 0)

	for _, model := range models {
		if (minSize == 0 || model.Size >= minSize) &&
			(maxSize == 0 || model.Size <= maxSize) {
			filtered = append(filtered, model)
		}
	}

	return filtered
}

func filterByTime(models []*metadata.ModelMetadata, cutoff time.Time) []*metadata.ModelMetadata {
	filtered := make([]*metadata.ModelMetadata, 0)

	for _, model := range models {
		if model.Modified.After(cutoff) || model.AddedAt.After(cutoff) {
			filtered = append(filtered, model)
		}
	}

	return filtered
}

func sortModels(models []*metadata.ModelMetadata, sortBy SortBy, ascending bool) {
	sort.Slice(models, func(i, j int) bool {
		var less bool

		switch sortBy {
		case SortByName:
			less = models[i].Name < models[j].Name
		case SortBySize:
			less = models[i].Size < models[j].Size
		case SortByModified:
			less = models[i].Modified.Before(models[j].Modified)
		case SortByAdded:
			less = models[i].AddedAt.Before(models[j].AddedAt)
		case SortByFormat:
			less = models[i].Format < models[j].Format
		case SortBySource:
			less = models[i].SourceRepo < models[j].SourceRepo
		default:
			less = models[i].Name < models[j].Name
		}

		if !ascending {
			less = !less
		}

		return less
	})
}
