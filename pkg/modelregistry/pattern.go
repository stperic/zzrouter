package modelregistry

import "strings"

// MatchPattern performs wildcard pattern matching with support for * and ?.
//   - * matches any sequence of characters
//   - ? matches any single character
//   - Case-insensitive matching
//   - If no wildcards, uses 3-tier priority matching (exact, suffix, no match)
//
// SINGLE SOURCE OF TRUTH for model name matching across the codebase.
func MatchPattern(str, pattern string) bool {
	// Empty pattern or * matches everything.
	if pattern == "" || pattern == "*" {
		return true
	}

	// Case-insensitive matching.
	str = strings.ToLower(str)
	pattern = strings.ToLower(pattern)

	// If no wildcards, use priority matching.
	if !strings.ContainsAny(pattern, "*?[]") {
		// Priority 1: Exact match.
		if str == pattern {
			return true
		}

		// Priority 2: Suffix match for org/model names (handles #variant format)
		// e.g., "Qwen3-1.7B-MLX-4bit" matches "lmstudio-community/Qwen3-1.7B-MLX-4bit"
		// e.g., "Llama-3.2-3B-Instruct-GGUF#Q4_K_M" matches "bartowski/Llama-3.2-3B-Instruct-GGUF#Q4_K_M"
		// e.g., "model#Q4_K_M" matches "org/model#Q4_K_M"
		// Note: For GGUF models, names are stored as filename without extension
		// (e.g., "Llama-3.2-3B-Instruct-Q4_K_M") and exact match (Priority 1)
		// handles these cases.
		if strings.Contains(str, "/") {
			parts := strings.Split(str, "/")
			if len(parts) >= 2 {
				lastPart := parts[len(parts)-1]

				// Direct suffix match.
				if lastPart == pattern {
					return true
				}

				// Handle #variant format: split both and compare model name + variant separately.
				if strings.Contains(pattern, "#") && strings.Contains(lastPart, "#") {
					patternModel, patternVariant, _ := strings.Cut(pattern, "#")
					lastModel, lastVariant, _ := strings.Cut(lastPart, "#")

					// Variant must match exactly, model name must match.
					if patternVariant == lastVariant && patternModel == lastModel {
						return true
					}
				}
			}
		}

		// No match (removed substring matching to prevent false positives).
		return false
	}

	// For wildcard patterns, use manual matching.
	return matchWildcardManual(str, pattern)
}

// matchWildcardManual implements manual wildcard matching.
// Supports * (any chars) and ? (single char).
func matchWildcardManual(str, pattern string) bool {
	if strings.Contains(pattern, "?") {
		return matchWithQuestionMark(str, pattern)
	}

	// Split pattern by *.
	parts := strings.Split(pattern, "*")

	if len(parts) == 1 {
		// No * in pattern, must be exact match.
		return str == pattern
	}

	// Remove empty parts from start and end to simplify logic:
	//   "lla*"  → ["lla", ""] → parts=["lla"], endsWithStar=true
	//   "*lla"  → ["", "lla"] → parts=["lla"], startsWithStar=true
	//   "*lla*" → ["", "lla", ""] → parts=["lla"], startsWithStar=true, endsWithStar=true

	startsWithStar := parts[0] == ""
	endsWithStar := parts[len(parts)-1] == ""

	nonEmptyParts := make([]string, 0)
	for _, part := range parts {
		if part != "" {
			nonEmptyParts = append(nonEmptyParts, part)
		}
	}

	if len(nonEmptyParts) == 0 {
		// Pattern is just "*" or "**", matches everything.
		return true
	}

	pos := 0
	for i, part := range nonEmptyParts {
		idx := strings.Index(str[pos:], part)
		if idx == -1 {
			return false
		}

		// First part: if pattern doesn't start with *, must match at position 0.
		if i == 0 && !startsWithStar && idx != 0 {
			return false
		}

		// Last part: if pattern doesn't end with *, must match at end.
		if i == len(nonEmptyParts)-1 && !endsWithStar {
			if pos+idx+len(part) != len(str) {
				return false
			}
		}

		pos += idx + len(part)
	}

	return true
}

// matchWithQuestionMark handles patterns with the ? wildcard.
func matchWithQuestionMark(str, pattern string) bool {
	if len(str) != len(pattern) {
		return false
	}
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '?' && pattern[i] != str[i] {
			return false
		}
	}
	return true
}
