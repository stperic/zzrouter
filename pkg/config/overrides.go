package config

import (
	"maps"
	"path/filepath"
	"strings"
)

// CloneOrEmpty returns maps.Clone(src), or an empty map if src is nil.
func CloneOrEmpty(src map[string]string) map[string]string {
	if src == nil {
		return make(map[string]string)
	}
	return maps.Clone(src)
}

// matchesModelPattern checks if a model name matches a pattern (supports wildcards)
// Pattern examples:
//   - "Qwen/Qwen2.5-VL-3B-Instruct" - exact match
//   - "Qwen/*" - all models from Qwen org
//   - "*/Qwen2.5-VL-*" - any org with Qwen2.5-VL models
//   - "*" - matches everything
func matchesModelPattern(modelName, pattern string) bool {
	// Use filepath.Match for glob-style pattern matching
	// This supports * and ? wildcards
	matched, err := filepath.Match(pattern, modelName)
	if err != nil {
		// If pattern is invalid, try exact match
		return modelName == pattern
	}
	return matched
}

// calculatePatternSpecificity returns a specificity score for a pattern
// Higher score = more specific pattern
// Scoring:
//   - Each literal character: +1
//   - Each wildcard (*): -10
//   - Exact match (no wildcards): +1000
func calculatePatternSpecificity(pattern string) int {
	if !strings.Contains(pattern, "*") && !strings.Contains(pattern, "?") {
		// Exact match gets highest score
		return 1000 + len(pattern)
	}

	score := 0
	for _, ch := range pattern {
		switch ch {
		case '*':
			score -= 10
		case '?':
			score -= 5
		default:
			score += 1
		}
	}
	return score
}

// MergeOverrides merges parameter layers from lowest to highest priority.
// Each successive layer overrides values from previous layers.
// Typical call: MergeOverrides(appDefaults, nodeOverrides, modelOverrides, userParams)
func MergeOverrides(layers ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, layer := range layers {
		maps.Copy(result, layer)
	}
	return result
}
