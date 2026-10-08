package search

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
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

// saveSearchPrefs persists the last used registry, sort, and tags to client config.
// Per-registry preferences (sort, tags) are stored under registry_prefs.
func saveSearchPrefs(registry, sort string, tags []string) {
	cm := pkgConfig.NewConfigManager("zzrouter")
	store, err := pkgConfig.NewClientConfigStore(cm)
	if err != nil {
		return
	}
	_ = store.SetSearchPrefs(registry, sort, tags)
}

// loadRegistryPrefs loads saved sort/tags for a registry from client config.
func loadRegistryPrefs(registry string) *pkgConfig.SearchRegistryPrefs {
	cm := pkgConfig.NewConfigManager("zzrouter")
	cfg, err := cm.LoadClientConfig()
	if err != nil || cfg == nil {
		return nil
	}
	if cfg.Preferences.Search.RegistryPrefs == nil {
		return nil
	}
	prefs, ok := cfg.Preferences.Search.RegistryPrefs[registry]
	if !ok {
		return nil
	}
	return &prefs
}

// saveTagPresets persists the current tag presets to client config.
func saveTagPresets(presets []TagPreset) {
	cm := pkgConfig.NewConfigManager("zzrouter")
	store, err := pkgConfig.NewClientConfigStore(cm)
	if err != nil {
		return
	}
	var tags []pkgConfig.TagFilter
	for _, p := range presets {
		tags = append(tags, pkgConfig.TagFilter{Tags: p.Tags, Logic: p.Logic})
	}
	_ = store.SetModelTags(tags)
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

// extractContextWindow extracts context window size from tags or name
func extractContextWindow(tags []string, name string) string {
	// Common patterns: "8k", "32k", "128k", "8192", etc.
	patterns := []string{"128k", "32k", "16k", "8k", "4k", "2k", "1k"}

	// Check tags first
	for _, tag := range tags {
		tagLower := strings.ToLower(tag)
		for _, pattern := range patterns {
			if strings.Contains(tagLower, pattern) {
				return strings.ToUpper(pattern)
			}
		}
	}

	// Check name
	nameLower := strings.ToLower(name)
	for _, pattern := range patterns {
		if strings.Contains(nameLower, pattern) {
			return strings.ToUpper(pattern)
		}
	}

	return ""
}

// extractInputTypes extracts input types from tags and pipeline
func extractInputTypes(tags []string, pipelineTag string) []string {
	var types []string
	seen := make(map[string]bool)

	// Check pipeline tag
	switch pipelineTag {
	case "text-generation", "text2text-generation":
		types = append(types, "text")
		seen["text"] = true
	case "image-to-text", "visual-question-answering":
		if !seen["image"] {
			types = append(types, "image")
			seen["image"] = true
		}
		if !seen["text"] {
			types = append(types, "text")
			seen["text"] = true
		}
	case "automatic-speech-recognition":
		if !seen["audio"] {
			types = append(types, "audio")
			seen["audio"] = true
		}
	}

	// Check tags for modalities
	for _, tag := range tags {
		tagLower := strings.ToLower(tag)
		if (strings.Contains(tagLower, "vision") || strings.Contains(tagLower, "image")) && !seen["image"] {
			types = append(types, "image")
			seen["image"] = true
		}
		if (strings.Contains(tagLower, "audio") || strings.Contains(tagLower, "speech")) && !seen["audio"] {
			types = append(types, "audio")
			seen["audio"] = true
		}
		if strings.Contains(tagLower, "video") && !seen["video"] {
			types = append(types, "video")
			seen["video"] = true
		}
	}

	// Default to text if nothing found
	if len(types) == 0 {
		types = append(types, "text")
	}

	return types
}

// extractCapabilities extracts model capabilities from tags
func extractCapabilities(tags []string) []string {
	var capabilities []string
	seen := make(map[string]bool)

	for _, tag := range tags {
		tagLower := strings.ToLower(tag)

		if (strings.Contains(tagLower, "tool") || strings.Contains(tagLower, "function")) && !seen["tools"] {
			capabilities = append(capabilities, "tools")
			seen["tools"] = true
		}
		if (strings.Contains(tagLower, "thinking") || strings.Contains(tagLower, "reasoning")) && !seen["thinking"] {
			capabilities = append(capabilities, "thinking")
			seen["thinking"] = true
		}
		if strings.Contains(tagLower, "vision") && !seen["vision"] {
			capabilities = append(capabilities, "vision")
			seen["vision"] = true
		}
		if strings.Contains(tagLower, "code") && !seen["code"] {
			capabilities = append(capabilities, "code")
			seen["code"] = true
		}
	}

	return capabilities
}

// extractQuantMethod extracts quantization method from filename
func extractQuantMethod(filename string) string {
	upper := strings.ToUpper(filename)

	// Full precision patterns
	if strings.Contains(upper, "FP16") || strings.Contains(upper, ".F16.") {
		return "FP16"
	}

	// GGUF patterns: Q4_K_M, Q5_K_S, Q8_0, etc.
	ggufPatterns := []string{"Q2_K", "Q3_K_S", "Q3_K_M", "Q3_K_L", "Q4_0", "Q4_K_S", "Q4_K_M", "Q5_0", "Q5_K_S", "Q5_K_M", "Q6_K", "Q8_0"}
	for _, pattern := range ggufPatterns {
		if strings.Contains(upper, pattern) {
			return pattern
		}
	}

	// GPTQ patterns: 4bit-128g, 4bit-32g, gptq-4bit-128g, etc.
	if strings.Contains(upper, "GPTQ") || strings.Contains(upper, "4BIT") || strings.Contains(upper, "8BIT") {
		// Check for group size variants
		if strings.Contains(upper, "128G") || strings.Contains(upper, "128-G") {
			if strings.Contains(upper, "4BIT") || strings.Contains(upper, "4-BIT") {
				return "GPTQ-4bit-128g"
			} else if strings.Contains(upper, "8BIT") || strings.Contains(upper, "8-BIT") {
				return "GPTQ-8bit-128g"
			}
		} else if strings.Contains(upper, "32G") || strings.Contains(upper, "32-G") {
			return "GPTQ-4bit-32g"
		} else if strings.Contains(upper, "4BIT") || strings.Contains(upper, "4-BIT") {
			return "GPTQ-4bit"
		} else if strings.Contains(upper, "8BIT") || strings.Contains(upper, "8-BIT") {
			return "GPTQ-8bit"
		}
		// If filename just has "gptq" but no specific variant, use filename
		if strings.Contains(filename, "/") {
			parts := strings.Split(filename, "/")
			return "GPTQ-" + parts[len(parts)-1]
		}
		return "GPTQ"
	}

	// AWQ patterns
	if strings.Contains(upper, "AWQ") {
		if strings.Contains(upper, "4BIT") {
			return "AWQ-4bit"
		}
		return "AWQ"
	}

	// Extract from filename if it contains model file info
	if strings.Contains(filename, "model") {
		return filepath.Base(filename)
	}

	return "Unknown"
}

// getQuantDescription returns description for quantization method
func getQuantDescription(quant string) string {
	descriptions := map[string]string{
		// Full precision
		"FP16": "full precision, no quality loss (largest size)",

		// GGUF quantizations
		"Q2_K":   "smallest, significant quality loss",
		"Q3_K_S": "very small, high quality loss",
		"Q3_K_M": "very small, high quality loss",
		"Q3_K_L": "small, substantial quality loss",
		"Q4_0":   "legacy; small, very high quality loss",
		"Q4_K_S": "small, greater quality loss",
		"Q4_K_M": "medium, balanced quality - recommended ⭐",
		"Q5_0":   "legacy; medium, balanced quality",
		"Q5_K_S": "large, low quality loss - recommended",
		"Q5_K_M": "large, very low quality loss - recommended ⭐",
		"Q6_K":   "very large, extremely low quality loss",
		"Q8_0":   "very large, extremely low quality loss",

		// GPTQ quantizations
		"GPTQ-4bit-128g": "4-bit, 128 group size - balanced",
		"GPTQ-4bit-32g":  "4-bit, 32 group size - higher accuracy",
		"GPTQ-8bit-128g": "8-bit, 128 group size - high quality",
		"GPTQ-4bit":      "4-bit quantization",
		"GPTQ-8bit":      "8-bit quantization",
		"GPTQ":           "GPTQ quantized model",

		// AWQ quantizations
		"AWQ-4bit": "4-bit AWQ - efficient and accurate",
		"AWQ":      "AWQ quantized model",
	}

	if desc, ok := descriptions[quant]; ok {
		return desc
	}
	return ""
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

// variantRecommendedPriority is the sort order for HuggingFace GGUF quantizations.
var variantRecommendedPriority = map[string]int{
	"Q4_K_M": 1, "Q5_K_M": 2, "Q5_K_S": 3, "Q6_K": 4, "Q4_K_S": 5,
	"Q8_0": 6, "Q3_K_M": 7, "Q3_K_L": 8, "Q3_K_S": 9, "Q5_0": 10,
	"Q4_0": 11, "Q2_K": 12,
}

// sortVariants sorts variants by recommended quantization order (Q4_K_M first).
func sortVariants(variants []ModelVariant) {
	sort.Slice(variants, func(i, j int) bool {
		p1 := variantRecommendedPriority[variants[i].QuantMethod]
		p2 := variantRecommendedPriority[variants[j].QuantMethod]
		if p1 == 0 {
			p1 = 100
		}
		if p2 == 0 {
			p2 = 100
		}
		return p1 < p2
	})
}

// sortVariantsByField sorts variants by the given field.
// "recommended" = HuggingFace quant priority or Ollama API order (default).
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
