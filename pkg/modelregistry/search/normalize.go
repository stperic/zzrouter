package search

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

// SortCloudResults orders the complete normalized catalog before pagination.
func SortCloudResults(results []any, key string, direction int) {
	if key == "" {
		return
	}
	slices.SortStableFunc(results, func(a, b any) int {
		left, _ := a.(map[string]any)
		right, _ := b.(map[string]any)
		leftID, _ := left["api_id"].(string)
		rightID, _ := right["api_id"].(string)
		comparison := 0
		switch key {
		case "date", "createdAt":
			ld, rd := cloudCreatedDate(left), cloudCreatedDate(right)
			if ld.IsZero() != rd.IsZero() {
				if ld.IsZero() {
					return 1
				}
				return -1
			}
			comparison = ld.Compare(rd)
		case "name", "title":
			ln, _ := left["display_name"].(string)
			rn, _ := right["display_name"].(string)
			comparison = strings.Compare(strings.ToLower(ln), strings.ToLower(rn))
		default:
			comparison = strings.Compare(leftID, rightID)
		}
		if direction < 0 {
			comparison = -comparison
		}
		if comparison == 0 {
			return cmp.Compare(leftID, rightID)
		}
		return comparison
	})
}

func cloudCreatedDate(model map[string]any) time.Time {
	for _, key := range []string{"created", "created_at", "createdAt"} {
		switch value := model[key].(type) {
		case float64:
			if value > 0 {
				return time.Unix(int64(value), 0).UTC()
			}
		case string:
			for _, format := range []string{time.RFC3339, time.DateOnly} {
				if date, err := time.Parse(format, value); err == nil {
					return date
				}
			}
		}
	}
	return time.Time{}
}

// NormalizeCloudResults sets canonical api_id and display_name fields on each
// cloud search result. This is the SINGLE place where provider-specific model
// identifier quirks are handled. All downstream code (enrichment, URL building,
// client display, deploy, chat, card) reads these canonical fields only.
//
// Canonical fields set on each result map:
//   - api_id:       The model identifier for ALL API operations (chat, deploy, register, card).
//   - display_name: Clean human-readable name for UI display.
//
// Provider quirks handled here:
//   - OpenRouter:  id="qwen/qwen3.6-plus:free" (API), name="Qwen: Qwen3.6 Plus (free)" (display)
//   - Cloudflare:  id=UUID (not useful for API), name="@cf/meta/llama-3" (API + display)
//   - Google:      id="models/gemini-2.5-flash" (API, has prefix), no name field
//   - OpenAI/Groq: id="gpt-4o" (API + display), no name field
//   - Anthropic:   id="claude-3-sonnet" (API), display_name="Claude 3 Sonnet" (provider-set)
func NormalizeCloudResults(results []any) {
	for _, result := range results {
		m, isMap := result.(map[string]any)
		if !isMap {
			continue
		}

		id, _ := m["id"].(string)
		name, _ := m["name"].(string)

		// --- api_id: the identifier for API operations ---
		// Default: use id (works for OpenRouter, OpenAI, Groq, Anthropic).
		// Exceptions:
		//   - Cloudflare: id is a UUID, use name instead ("@cf/meta/llama-3")
		//   - Google Gemini: id has "models/" prefix, strip it ("models/gemini-2.5-flash" → "gemini-2.5-flash")
		apiID := id
		if isUUID(id) && name != "" {
			apiID = name
		}
		apiID = strings.TrimPrefix(apiID, "models/")
		if apiID != "" {
			m["api_id"] = apiID
		}

		// --- display_name: human-readable name for UI ---
		// Use provider's display_name if already set (Anthropic).
		// Otherwise prefer name (human-readable) over id.
		// Strip Google's "models/" prefix for clean display.
		if dn, ok := m["display_name"].(string); !ok || dn == "" {
			dn = name
			if dn == "" {
				dn = id
			}
			if dn != "" {
				dn = strings.TrimPrefix(dn, "models/") // Google Gemini prefix
				m["display_name"] = dn
			}
		}
	}
}

// isUUID returns true if s looks like a UUID (8-4-4-4-12 hex pattern).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
