package search

import "strings"

// FilterModelsByTags filters models by tag list using AND/OR logic.
// `logic` is case-insensitive; anything other than "AND" or "OR" defaults
// to AND. An empty filter set returns the input unchanged.
func FilterModelsByTags(models []ModelInfo, filterTags []string, logic string) []ModelInfo {
	if len(filterTags) == 0 {
		return models
	}

	logic = strings.ToUpper(logic)
	if logic != "AND" && logic != "OR" {
		logic = "AND"
	}

	var filtered []ModelInfo

	for _, model := range models {
		modelTagsLower := make(map[string]bool)
		for _, tag := range model.Tags {
			modelTagsLower[strings.ToLower(tag)] = true
		}

		matches := false

		if logic == "AND" {
			matches = true
			for _, filterTag := range filterTags {
				if !modelTagsLower[strings.ToLower(filterTag)] {
					matches = false
					break
				}
			}
		} else {
			for _, filterTag := range filterTags {
				if modelTagsLower[strings.ToLower(filterTag)] {
					matches = true
					break
				}
			}
		}

		if matches {
			filtered = append(filtered, model)
		}
	}

	return filtered
}

// ParseSortDirection converts a string sort direction to an int suitable for
// SearchParams.Direction. "desc"/"descending"/"down" → -1. "asc"/
// "ascending"/"up" → 1. Unknown values default to -1 (descending) because
// most UI flows default to newest-first.
func ParseSortDirection(order string) int {
	switch strings.ToLower(order) {
	case "desc", "descending", "down":
		return -1
	case "asc", "ascending", "up":
		return 1
	default:
		return -1
	}
}
