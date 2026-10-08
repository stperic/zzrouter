package search

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

const (
	// Characters of HTML kept after a tag link when reading its size and
	// context window.
	tagSectionSize = 1000
)

// Multipliers for the abbreviated counts a library card shows
// ("38.7M Pulls", "523K Pulls").
var statSuffixMultiplier = map[string]float64{
	"":  1,
	"K": 1_000,
	"M": 1_000_000,
	"B": 1_000_000_000,
}

// ErrLibraryUnparsed reports a library page that was fetched successfully
// and yielded no models. It is distinct from a transport error because
// the two need opposite responses: a network failure is someone else's
// outage and passes, while this one means the page markup moved and the
// parser below has to follow it.
var ErrLibraryUnparsed = errors.New("ollama library page yielded no models")

// Pre-compiled regex patterns for performance
var (
	// A card's own link, one per card, and the anchor each card's
	// section is cut at.
	libraryCardRegex = regexp.MustCompile(`href="/library/([a-zA-Z0-9._-]+)"`)

	// A card's stats render the number and its label as sibling spans:
	// <span >38.7M</span><span …>&nbsp;Pulls</span>. Number and label are
	// captured together — matching them separately is how a tag count
	// gets recorded as a pull count.
	libraryStatRegex = regexp.MustCompile(`>\s*([\d.,]+)\s*([KMB])?\s*</span>\s*<span[^>]*>(?:&nbsp;|\s)*(Pulls|Tags)\b`)

	// The relative date is the span that FOLLOWS the "Updated" label.
	updatedRegex = regexp.MustCompile(`Updated(?:&nbsp;|\s)*</span>\s*<span[^>]*>\s*([^<]+?)\s*</span>`)

	// A model with cloud-served variants carries a "cloud" badge.
	cloudBadgeRegex = regexp.MustCompile(`>\s*cloud\s*</span>`)

	// Tag link pattern
	tagLinkRegex = regexp.MustCompile(`<a[^>]*href="/library/([^/:"]+):([^"]+)"[^>]*>`)

	// Size extraction from tag section
	tagSizeRegex = regexp.MustCompile(`(?:•|\s)+([0-9.]+\s*[KMGT]B)(?:•|\s)+`)

	// Context extraction from tag section
	tagContextRegex = regexp.MustCompile(`([0-9]+K)\s+context\s+window`)
)

// ScrapeOllamaLibrary scrapes the Ollama library page to get all available models
// This is a workaround since Ollama doesn't provide a public API for the full library
// See: https://github.com/ollama/ollama/issues/7751
func ScrapeOllamaLibrary() ([]OllamaModelInfo, error) {
	url := "https://ollama.com/library"
	client := &http.Client{Timeout: httpTimeout}

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Ollama library: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama library returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return parseLibraryPage(body)
}

// parseLibraryPage turns a fetched library page into models and refuses
// one that parsed to nothing. A page that answered 200 and yielded no
// models is a parser that no longer understands the markup; returning it
// as an empty success makes a broken catalog read like "no results", and
// every caller downstream believes it.
func parseLibraryPage(body []byte) ([]OllamaModelInfo, error) {
	models := extractModelsFromLibraryHTML(string(body))
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: %d bytes of HTML held no model cards", ErrLibraryUnparsed, len(body))
	}
	return models, nil
}

// libraryCard is one model's slice of the library page.
type libraryCard struct {
	name    string
	section string
}

// extractLibraryCards splits the library page into one section per model,
// cut at each card's own /library/<name> link. Anchoring on the link
// rather than searching for the name is what keeps a card from being read
// through another card's text: "nomic-embed-text" occurs inside
// "nomic-embed-text-v2-moe", and a name search finds whichever comes
// first in the page, not the one whose card it is.
func extractLibraryCards(html string) []libraryCard {
	matches := libraryCardRegex.FindAllStringSubmatchIndex(html, -1)
	cards := make([]libraryCard, 0, len(matches))
	for i, m := range matches {
		name := html[m[2]:m[3]]
		if isExcludedPath(name) {
			continue
		}
		end := len(html)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		cards = append(cards, libraryCard{name: name, section: html[m[0]:end]})
	}
	return cards
}

// extractLibraryStat reads one labelled count ("Pulls", "Tags") from a
// card, expanding the K/M/B suffix the card abbreviates it with.
func extractLibraryStat(section, label string) int {
	for _, m := range libraryStatRegex.FindAllStringSubmatch(section, -1) {
		if m[3] != label {
			continue
		}
		var num float64
		if _, err := fmt.Sscanf(strings.ReplaceAll(m[1], ",", ""), "%f", &num); err != nil {
			continue
		}
		return int(num * statSuffixMultiplier[m[2]])
	}
	return 0
}

// extractModelsFromLibraryHTML builds one model per card on the library
// listing page. A card whose stats fail to parse still yields a model:
// dropping it would spend the whole catalog to hide a missing number.
func extractModelsFromLibraryHTML(html string) []OllamaModelInfo {
	cards := extractLibraryCards(html)
	models := make([]OllamaModelInfo, 0, len(cards))
	for _, card := range cards {
		model := OllamaModelInfo{
			Name:  card.name,
			Model: card.name + ":latest",
			// Estimated, not read: actual sizes are fetched lazily when
			// the user opens a model's details or tags.
			Size:       EstimateOllamaModelSize(card.name),
			URL:        fmt.Sprintf("https://ollama.com/library/%s", card.name),
			Pulls:      extractLibraryStat(card.section, "Pulls"),
			Tags:       extractLibraryStat(card.section, "Tags"),
			HasCloud:   cloudBadgeRegex.MatchString(card.section),
			ModifiedAt: extractUpdatedFromSection(card.section),
		}
		model.DaysAgo = parseRelativeDate(model.ModifiedAt)
		models = append(models, model)
	}
	return models
}

// extractUpdatedFromSection extracts the relative update date from a
// model section.
func extractUpdatedFromSection(section string) string {
	if matches := updatedRegex.FindStringSubmatch(section); len(matches) >= 2 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

// parseRelativeDate converts relative date strings to days ago
// Examples: "1 month ago" -> 30, "2 weeks ago" -> 14, "3 days ago" -> 3
func parseRelativeDate(relativeDate string) int {
	if relativeDate == "" {
		return 999999 // Unknown dates go to the end
	}

	lower := strings.ToLower(relativeDate)

	// Extract number and unit
	re := regexp.MustCompile(`(\d+)\s+(year|month|week|day|hour|minute)s?\s+ago`)
	matches := re.FindStringSubmatch(lower)

	if len(matches) < 3 {
		return 999999 // Can't parse, put at end
	}

	var num int
	_, _ = fmt.Sscanf(matches[1], "%d", &num)
	unit := matches[2]

	// Convert to days
	switch unit {
	case "year":
		return num * 365
	case "month":
		return num * 30
	case "week":
		return num * 7
	case "day":
		return num
	case "hour":
		return 0 // Less than a day
	case "minute":
		return 0 // Less than a day
	default:
		return 999999
	}
}

// isExcludedPath filters out common non-model paths
func isExcludedPath(path string) bool {
	excluded := []string{
		"search", "models", "blog", "download", "docs",
		"api", "signin", "signup", "library", "tags",
		"pricing", "github", "meetups", "discord",
		"privacy", "contact", "terms", "about",
		"", // empty string
	}

	path = strings.ToLower(path)
	if slices.Contains(excluded, path) {
		return true
	}

	// Exclude paths that are too short (likely not model names)
	if len(path) < 2 {
		return true
	}

	return false
}

// GetOllamaModelTags fetches the list of available tags/versions for a specific model
// This is called lazily when a user selects a model
func GetOllamaModelTags(modelName string) ([]OllamaTagInfo, error) {
	url := fmt.Sprintf("https://ollama.com/library/%s/tags", modelName)
	client := &http.Client{Timeout: httpTimeout}

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tags for %s: %w", modelName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tags page returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Extract tag details from the tags page
	tags := extractTagDetailsFromHTML(string(body))
	return tags, nil
}

// extractTagDetailsFromHTML extracts tag details (name, size, context, input) from the tags page
func extractTagDetailsFromHTML(html string) []OllamaTagInfo {
	var tags []OllamaTagInfo
	seenTags := make(map[string]bool) // Track duplicates but preserve order

	// Find all tag sections - each section contains a tag link and its metadata
	// Pattern: <a href="/library/model:tag">...</a> followed by size/context
	// Must have colon in href to distinguish from model name links
	// Use pre-compiled regex for better performance
	linkMatches := tagLinkRegex.FindAllStringSubmatchIndex(html, -1)

	for _, match := range linkMatches {
		if len(match) < 6 {
			continue
		}

		// match[4]:match[5] is the tag name (second capture group)
		tagName := html[match[4]:match[5]]
		if tagName == "" || isExcludedPath(tagName) {
			continue
		}

		// Skip if we've already seen this tag (avoid duplicates)
		if seenTags[tagName] {
			continue
		}
		seenTags[tagName] = true

		// Extract a section of HTML around this tag
		sectionStart := match[0]
		sectionEnd := min(sectionStart+tagSectionSize, len(html))
		section := html[sectionStart:sectionEnd]

		// Extract size from this section using pre-compiled regex
		size := "-"
		sizeMatches := tagSizeRegex.FindStringSubmatch(section)
		if len(sizeMatches) > 1 {
			size = strings.TrimSpace(sizeMatches[1])
		}

		// Extract context from this section using pre-compiled regex
		context := "-"
		contextMatches := tagContextRegex.FindStringSubmatch(section)
		if len(contextMatches) > 1 {
			context = contextMatches[1]
		}

		// Determine input type
		input := "Text"
		if strings.Contains(section, "Image") || strings.Contains(section, "Vision") || strings.Contains(section, "vision") {
			input = "Image+Text"
		}

		// Append in order (preserves HTML order)
		tags = append(tags, OllamaTagInfo{
			Name:    tagName,
			Size:    size,
			Context: context,
			Input:   input,
		})
	}

	return tags
}
