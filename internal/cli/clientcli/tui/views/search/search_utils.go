package search

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/stperic/zzrouter/pkg/utils"
)

// DateLayouts lists time formats tried when parsing dates from cloud providers.
var DateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05.000Z",
	"2006-01-02T15:04:05Z",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05", // Cloudflare uses space separator
	"2006-01-02",
}

// htmlLinkRe matches <a href="URL" ...>TEXT</a> patterns.
var htmlLinkRe = regexp.MustCompile(`(?i)<a\s[^>]*href=["']([^"']*)["'][^>]*>(.*?)</a>`)

// htmlTagRe matches any remaining HTML tags.
var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// renderHTMLForTerminal converts HTML to terminal-friendly text.
// Links become clickable OSC 8 hyperlinks (supported by iTerm2, macOS Terminal,
// Windows Terminal, GNOME Terminal, etc.) with underline styling.
// All other HTML tags are stripped.
func renderHTMLForTerminal(s string) string {
	// First pass: convert <a href="URL">TEXT</a> to OSC 8 hyperlinks
	// OSC 8 format: \x1b]8;;URL\x1b\\TEXT\x1b]8;;\x1b\\
	result := htmlLinkRe.ReplaceAllStringFunc(s, func(match string) string {
		parts := htmlLinkRe.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		url := parts[1]
		text := parts[2]
		// Strip any nested tags from the link text
		text = htmlTagRe.ReplaceAllString(text, "")
		if text == "" {
			text = url
		}
		// OSC 8 hyperlink with underline
		return fmt.Sprintf("\x1b]8;;%s\x1b\\\x1b[4m%s\x1b[24m\x1b]8;;\x1b\\", url, text)
	})

	// Second pass: strip any remaining HTML tags
	result = htmlTagRe.ReplaceAllString(result, "")

	return strings.TrimSpace(result)
}

// Helper functions

// formatErrorMessage formats an error message with user-friendly text
func formatErrorMessage(format string, args ...any) string {
	// If the first arg is an error, extract user-friendly message
	if len(args) > 0 {
		if err, ok := args[0].(error); ok {
			args[0] = shared.ExtractUserFriendlyError(err)
		}
	}
	return fmt.Sprintf("✗ "+format, args...)
}

// filterResultsByName returns results whose name matches the filter pattern.
// Empty filter returns all results.
func filterResultsByName(results []searchResult, filter string) []searchResult {
	if filter == "" {
		return results
	}
	var filtered []searchResult
	for _, r := range results {
		if matchesWildcard(r.Name, filter) {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// applyClientSort sorts results client-side by the given key.
// Returns the sorted slice, or the original if the key isn't a client sort.
func applyClientSort(results []searchResult, sortBy string) []searchResult {
	if len(results) == 0 {
		return results
	}
	sorted := make([]searchResult, len(results))
	copy(sorted, results)

	switch sortBy {
	case "popular", "downloads", "trendingScore":
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Downloads > sorted[j].Downloads
		})
	case "name":
		sort.Slice(sorted, func(i, j int) bool {
			// Sort by display name (strip org prefix for HuggingFace models)
			ni, nj := sorted[i].Name, sorted[j].Name
			if idx := strings.LastIndex(ni, "/"); idx >= 0 {
				ni = ni[idx+1:]
			}
			if idx := strings.LastIndex(nj, "/"); idx >= 0 {
				nj = nj[idx+1:]
			}
			return strings.ToLower(ni) < strings.ToLower(nj)
		})
	case "newest", "lastModified", "date", "created", "createdAt":
		sortByDate(sorted)
	case "context":
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].ContextLength > sorted[j].ContextLength
		})
	case "price":
		sort.Slice(sorted, func(i, j int) bool {
			// Free models first, then cheapest
			if sorted[i].PricePrompt == 0 && sorted[j].PricePrompt > 0 {
				return true
			}
			if sorted[j].PricePrompt == 0 && sorted[i].PricePrompt > 0 {
				return false
			}
			return sorted[i].PricePrompt < sorted[j].PricePrompt
		})
	case "price-desc":
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].PricePrompt > sorted[j].PricePrompt
		})
	default:
		return results
	}
	return sorted
}

// mapSortToAPI translates unified sort names to server API values.
func mapSortToAPI(sortBy string) string {
	switch sortBy {
	case "", "popular":
		return "trendingScore"
	case "newest":
		return "lastModified"
	default:
		return sortBy // name, downloads, likes, etc. pass through
	}
}

// sortByDate sorts results in-place by date, newest first.
// Handles both ISO 8601 timestamps and relative dates ("2 days ago").
func sortByDate(sorted []searchResult) {
	type resultWithTime struct {
		result searchResult
		time   time.Time
	}

	layouts := DateLayouts

	withTime := make([]resultWithTime, len(sorted))
	for i := range sorted {
		dateStr := sorted[i].ModifiedAt
		if dateStr == "" {
			dateStr = sorted[i].CreatedAt
		}

		var t time.Time
		if dateStr != "" {
			var err error
			for _, layout := range layouts {
				t, err = time.Parse(layout, dateStr)
				if err == nil {
					break
				}
			}
			if err != nil {
				t = parseRelativeDateToTime(dateStr)
			}
		}
		withTime[i] = resultWithTime{result: sorted[i], time: t}
	}

	sort.Slice(withTime, func(i, j int) bool {
		return withTime[i].time.After(withTime[j].time)
	})

	for i, wt := range withTime {
		sorted[i] = wt.result
	}
}

// relativeDateRe matches patterns like "2 days ago", "1 month ago".
var relativeDateRe = regexp.MustCompile(`(\d+)\s+(year|month|week|day|hour|minute)s?\s+ago`)

// parseRelativeDateToTime converts relative date strings ("2 days ago") to a time.Time.
// Returns zero time if the string can't be parsed.
func parseRelativeDateToTime(s string) time.Time {
	matches := relativeDateRe.FindStringSubmatch(strings.ToLower(s))
	if len(matches) < 3 {
		return time.Time{}
	}
	var num int
	_, _ = fmt.Sscanf(matches[1], "%d", &num)
	now := utils.Now()
	switch matches[2] {
	case "year":
		return now.AddDate(-num, 0, 0)
	case "month":
		return now.AddDate(0, -num, 0)
	case "week":
		return now.AddDate(0, 0, -num*7)
	case "day":
		return now.AddDate(0, 0, -num)
	case "hour":
		return now.Add(-time.Duration(num) * time.Hour)
	case "minute":
		return now.Add(-time.Duration(num) * time.Minute)
	}
	return time.Time{}
}

// FormatRelativeTimeFromISO formats an ISO timestamp as relative time (e.g., "11 months ago")
func FormatRelativeTimeFromISO(isoTimeStr string) string {
	if isoTimeStr == "" {
		return "-"
	}

	var t time.Time
	var err error
	for _, layout := range DateLayouts {
		t, err = time.Parse(layout, isoTimeStr)
		if err == nil {
			break
		}
	}

	if err != nil {
		// If can't parse, just return the date part
		if len(isoTimeStr) >= 10 {
			return isoTimeStr[:10]
		}
		return isoTimeStr
	}

	// Calculate duration since the timestamp
	now := utils.Now()
	duration := now.Sub(t)

	// Format as relative time
	if duration < time.Minute {
		return "just now"
	}

	if duration < time.Hour {
		minutes := int(duration.Minutes())
		if minutes == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", minutes)
	}

	if duration < 24*time.Hour {
		hours := int(duration.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	}

	// Use days for < 14 days
	if duration < 14*24*time.Hour {
		days := int(duration.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}

	// Use weeks for 14-30 days
	if duration < 30*24*time.Hour {
		weeks := int(duration.Hours() / (24 * 7))
		if weeks == 1 {
			return "1 week ago"
		}
		return fmt.Sprintf("%d weeks ago", weeks)
	}

	// Use months for 30 days - 365 days
	if duration < 365*24*time.Hour {
		months := int(duration.Hours() / (24 * 30))
		if months == 1 {
			return "1 month ago"
		}
		return fmt.Sprintf("%d months ago", months)
	}

	// Use years for > 365 days
	years := int(duration.Hours() / (24 * 365))
	if years == 1 {
		return "1 year ago"
	}
	return fmt.Sprintf("%d years ago", years)
}

// sortVariants follows the registry recommendation and stable catalog order.
func sortVariants(variants []ModelVariant) {
	sort.SliceStable(variants, func(i, j int) bool {
		if variants[i].Recommended != variants[j].Recommended {
			return variants[i].Recommended
		}
		return variants[i].CatalogOrder < variants[j].CatalogOrder
	})
}

// sortVariantsByField sorts variants by the given field.
// "recommended" follows the server recommendation and catalog order.
// "name" = alphabetical. "size" = smallest first.
func sortVariantsByField(variants []ModelVariant, field string) {
	switch field {
	case "name":
		sort.Slice(variants, func(i, j int) bool {
			return strings.ToLower(variants[i].Filename) < strings.ToLower(variants[j].Filename)
		})
	case "size":
		sort.Slice(variants, func(i, j int) bool {
			return variants[i].Size < variants[j].Size
		})
	case "recommended":
		sortVariants(variants)
	}
}
