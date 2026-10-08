package utils

import (
	"fmt"
	"strings"
)

// FormatSize formats size in bytes to human-readable format (decimal, 1000-based like Ollama)
// This is the single source of truth for size formatting across the entire project (DRY principle)
func FormatSize(size int64) string {
	if size == 0 {
		return "N/A"
	}

	const unit = 1000
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

// MatchPattern performs case-insensitive pattern matching with wildcard support
// Supports * (zero or more chars) and ? (exactly one char)
// This is the single source of truth for wildcard matching (DRY principle)
func MatchPattern(pattern, text string) bool {
	// Convert to lowercase for case-insensitive matching
	pattern = strings.ToLower(pattern)
	text = strings.ToLower(text)

	// Handle empty cases
	if pattern == "" {
		return text == ""
	}
	if pattern == "*" {
		return true
	}

	// Check if pattern contains wildcards
	hasWildcard := strings.Contains(pattern, "*") || strings.Contains(pattern, "?")

	if !hasWildcard {
		// No wildcards - use substring matching
		return strings.Contains(text, pattern)
	}

	// Wildcard matching algorithm
	pi, ti := 0, 0
	starIdx, matchIdx := -1, 0

	for ti < len(text) {
		if pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == text[ti]) {
			// Character match or ? wildcard
			pi++
			ti++
		} else if pi < len(pattern) && pattern[pi] == '*' {
			// * wildcard - remember position
			starIdx = pi
			matchIdx = ti
			pi++
		} else if starIdx != -1 {
			// Backtrack to last * and try matching one more character
			pi = starIdx + 1
			matchIdx++
			ti = matchIdx
		} else {
			// No match
			return false
		}
	}

	// Handle remaining * in pattern
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}

	return pi == len(pattern)
}

// FormatModelName formats a model name for display by showing only the last segment after the final slash
// Examples:
//   - "lmstudio-community/Phi-4-mini-reasoning-MLX-4bit" -> "Phi-4-mini-reasoning-MLX-4bit"
//   - "Qwen/Qwen3-0.6B" -> "Qwen3-0.6B"
//   - "simple-model" -> "simple-model"
//
// This is the single source of truth for model name display formatting (DRY principle)
func FormatModelName(modelName string) string {
	if modelName == "" {
		return ""
	}

	// Find the last slash
	lastSlash := strings.LastIndex(modelName, "/")
	if lastSlash == -1 {
		// No slash found, return the whole name
		return modelName
	}

	// Return everything after the last slash
	return modelName[lastSlash+1:]
}
