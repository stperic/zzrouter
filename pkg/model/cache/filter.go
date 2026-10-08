package cache

import (
	"time"

	"github.com/stperic/zzrouter/pkg/modelregistry"
)

// filterCachedModels filters cached models by host, repo, app, and/or
// model name. Empty filters on a dimension mean "don't filter."
func filterCachedModels(models []*CachedModel, host, repo, app, model string) []*CachedModel {
	if host == "" && repo == "" && app == "" && model == "" {
		return models
	}

	filtered := make([]*CachedModel, 0, len(models))
	for _, cm := range models {
		if matchesCachedModel(cm, host, repo, app, model) {
			filtered = append(filtered, cm)
		}
	}
	return filtered
}

// matchesCachedModel reports whether a cached model passes the filter
// criteria. Uses centralized MatchPattern from modelregistry (DRY).
func matchesCachedModel(cm *CachedModel, host, repo, app, model string) bool {
	if host != "" && host != "*" {
		if !modelregistry.MatchPattern(cm.Node, host) {
			return false
		}
	}
	if repo != "" && repo != "*" {
		if !modelregistry.MatchPattern(cm.SourceRepo, repo) {
			return false
		}
	}
	if app != "" && app != "*" {
		if !modelregistry.MatchPattern(cm.Provider, app) {
			return false
		}
	}
	if model != "" && model != "*" {
		nameMatch := modelregistry.MatchPattern(cm.Name, model)
		sourceMatch := cm.SourceID != "" && modelregistry.MatchPattern(cm.SourceID, model)
		if !nameMatch && !sourceMatch {
			return false
		}
	}
	return true
}

// extractStringField safely extracts a string field from a map.
// Also handles time.Time values (from direct ToOllamaFormat calls,
// not JSON roundtrip).
func extractStringField(item map[string]any, key string) string {
	if val, ok := item[key].(string); ok {
		return val
	}
	if val, ok := item[key].(time.Time); ok {
		return val.Format(time.RFC3339)
	}
	return ""
}

// extractInt64Field safely extracts an int64 field from a map.
// Handles both int64 and float64 (JSON roundtrip produces float64).
func extractInt64Field(item map[string]any, key string) int64 {
	if val, ok := item[key].(int64); ok {
		return val
	}
	if val, ok := item[key].(float64); ok {
		return int64(val)
	}
	return 0
}
