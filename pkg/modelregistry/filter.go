package modelregistry

import "github.com/stperic/zzrouter/pkg/modelregistry/metadata"

// ModelFilter represents filter criteria for models.
type ModelFilter struct {
	Format      string // Filter by format (GGUF, safetensors, etc.)
	SourceRepo  string // Filter by source repository (huggingface, ollama, etc.)
	NamePattern string // Filter by name pattern (supports wildcards)
	MinSize     int64  // Minimum size in bytes
	MaxSize     int64  // Maximum size in bytes
}

// Matches reports whether a model matches the filter criteria.
func (f *ModelFilter) Matches(model *metadata.ModelMetadata) bool {
	// Format filter (supports wildcards).
	if f.Format != "" && f.Format != "*" {
		if !MatchPattern(model.Format, f.Format) {
			return false
		}
	}

	// Source repository filter (supports wildcards).
	if f.SourceRepo != "" && f.SourceRepo != "*" {
		if !MatchPattern(model.SourceRepo, f.SourceRepo) {
			return false
		}
	}

	// Name pattern filter (supports wildcards, also checks SourceID).
	if f.NamePattern != "" && f.NamePattern != "*" {
		if !MatchPattern(model.Name, f.NamePattern) && !MatchPattern(model.SourceID, f.NamePattern) {
			return false
		}
	}

	// Size filters.
	if f.MinSize > 0 && model.Size < f.MinSize {
		return false
	}
	if f.MaxSize > 0 && model.Size > f.MaxSize {
		return false
	}

	return true
}

// FilterModels returns models matching the given criteria.
func (r *Registry) FilterModels(filter *ModelFilter) ([]*metadata.ModelMetadata, error) {
	filtered, err := r.filterModels(filter.Matches)
	if err != nil {
		return nil, err
	}

	sortModels(filtered, SortByName, true)
	return filtered, nil
}

// GetModelsByFormat returns all models of a specific format.
func (r *Registry) GetModelsByFormat(format string) ([]*metadata.ModelMetadata, error) {
	return r.filterModels(func(m *metadata.ModelMetadata) bool {
		return m.Format == format
	})
}

// GetModelsBySource returns all models from a specific source.
func (r *Registry) GetModelsBySource(source string) ([]*metadata.ModelMetadata, error) {
	return r.filterModels(func(m *metadata.ModelMetadata) bool {
		return m.SourceRepo == source
	})
}

// RegistryStats contains statistics about the model registry.
type RegistryStats struct {
	TotalModels int            `json:"total_models"`
	TotalSize   int64          `json:"total_size"`
	ByFormat    map[string]int `json:"by_format"`
	BySource    map[string]int `json:"by_source"`
}

// GetStats returns statistics about the registry.
func (r *Registry) GetStats() *RegistryStats {
	stats := &RegistryStats{
		TotalModels: 0,
		TotalSize:   0,
		ByFormat:    make(map[string]int),
		BySource:    make(map[string]int),
	}

	_ = r.withModels(func(models []*metadata.ModelMetadata) error {
		stats.TotalModels = len(models)
		for _, model := range models {
			stats.TotalSize += model.Size
			stats.ByFormat[model.Format]++
			stats.BySource[model.SourceRepo]++
		}
		return nil
	})

	return stats
}
