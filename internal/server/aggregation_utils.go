// package server provides HTTP handlers for the zzrouter host server.
// Aggregation utilities - DRY helpers for post-aggregation sorting, filtering, and pagination

package server

import (
	"strings"
)

// AggregationParams contains parameters for post-aggregation processing
type AggregationParams struct {
	// Filtering (applied per-host to reduce data transfer)
	FilterFields map[string]string // e.g., {"registry": "ollama", "model": "llama*"}

	// Sorting (applied after aggregation for global sort)
	SortBy    string // Field name to sort by
	SortOrder string // "asc" or "desc"

	// Pagination (applied after aggregation for global pagination)
	Limit  int
	Offset int
}

// matchesFilterPattern performs wildcard matching
func matchesFilterPattern(value, pattern string) bool {
	if value == pattern {
		return true
	}

	// Wildcard matching
	if strings.Contains(pattern, "*") {
		if strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") {
			return strings.Contains(value, pattern[1:len(pattern)-1])
		} else if strings.HasPrefix(pattern, "*") {
			return strings.HasSuffix(value, pattern[1:])
		} else if strings.HasSuffix(pattern, "*") {
			return strings.HasPrefix(value, pattern[:len(pattern)-1])
		}
	}

	return false
}
