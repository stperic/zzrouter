package search

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/stperic/zzrouter/pkg/model/pricing"
)

const defaultMetadataPriority = 50

// Keys already represented by typed fields on searchResult — skip in metadata.
var metadataSkipKeys = map[string]bool{
	"id": true, "name": true, "model": true, "downloads": true, "pulls": true,
	"likes": true, "created_at": true, "createdAt": true, "created": true,
	"modified_at": true, "lastModified": true, "last_modified": true,
	"tags": true, "size": true, "siblings": true,
	"context_length": true, "context_window": true, "max_input_tokens": true,
	"pricing": true, "has_cloud": true, "trendingScore": true, "url": true,
	"safetensors": true, "gguf": true, "object": true, "type": true,
	"provider": true, "model_id": true, "readme": true, "pipeline_tag": true,
	"license": true, "author": true, "description": true, "owned_by": true,
}

// Keys that contain Unix timestamps (float64) — format as dates instead of numbers.
var metadataTimestampKeys = map[string]bool{
	"updated_at": true, "created_at": true, "created": true,
}

// metadataDisplayPriority maps display-ready keys to sort priority (lower = shown first).
// Built from raw key → priority mapping, pre-formatted at init time.
var metadataDisplayPriority map[string]int

func init() {
	raw := map[string]int{
		"display_name":                  1,
		"max_output_tokens":             3,
		"max_tokens":                    4,
		"mode":                          5,
		"capabilities":                  10,
		pricing.MetadataKeyCapabilities: 11,
		pricing.MetadataKeyInputTypes:   13,
		"deprecation_date":              20,
	}
	metadataDisplayPriority = make(map[string]int, len(raw))
	for k, v := range raw {
		metadataDisplayPriority[formatMetadataKey(k)] = v
	}
}

// buildMetadata converts a raw server response map into ordered KeyValue pairs
// for the detail view. Skips keys already represented by typed searchResult fields.
func buildMetadata(raw map[string]any) []KeyValue {
	if raw == nil {
		return nil
	}

	var pairs []KeyValue
	for key, val := range raw {
		if metadataSkipKeys[key] {
			continue
		}
		// Skip pricing-store-derived fields when provider has native equivalents
		if key == pricing.MetadataKeyCapabilities && raw["capabilities"] != nil {
			continue
		}
		if key == pricing.MetadataKeyInputTypes && raw["capabilities"] != nil {
			continue
		}
		if entries := formatMetadataEntry(key, val); len(entries) > 0 {
			pairs = append(pairs, entries...)
		}
	}

	sortMetadata(pairs)
	return pairs
}

// mergeMetadata merges additional metadata into existing, deduplicating by key.
// Additional entries only supplement — they don't overwrite existing keys.
func mergeMetadata(existing, additional []KeyValue) []KeyValue {
	seen := make(map[string]bool, len(existing))
	for _, kv := range existing {
		seen[kv.Key] = true
	}
	for _, kv := range additional {
		if !seen[kv.Key] {
			existing = append(existing, kv)
			seen[kv.Key] = true
		}
	}
	sortMetadata(existing)
	return existing
}

// formatMetadataEntry converts a single key-value pair into display-ready KeyValue(s).
func formatMetadataEntry(key string, val any) []KeyValue {
	displayKey := formatMetadataKey(key)

	switch v := val.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []KeyValue{{Key: displayKey, Value: v}}

	case float64:
		if v == 0 {
			return nil
		}
		// Unix timestamps: format as relative date
		if metadataTimestampKeys[key] && v > 1e9 && v < 1e11 {
			return []KeyValue{{Key: displayKey, Value: FormatRelativeTimeFromISO(time.Unix(int64(v), 0).Format(time.RFC3339))}}
		}
		// Large integers: format with commas
		if v >= 1000 && v == float64(int64(v)) {
			return []KeyValue{{Key: displayKey, Value: formatNumberWithCommas(int64(v))}}
		}
		return []KeyValue{{Key: displayKey, Value: fmt.Sprintf("%g", v)}}

	case bool:
		if !v {
			return nil
		}
		return []KeyValue{{Key: displayKey, Value: "yes"}}

	case []any:
		var strs []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				strs = append(strs, s)
			}
		}
		if len(strs) == 0 {
			return nil
		}
		return []KeyValue{{Key: displayKey, Value: strings.Join(strs, ", ")}}

	case map[string]any:
		// Flatten nested objects — extract supported capabilities
		return flattenNestedMap(displayKey, v)

	default:
		return nil
	}
}

// flattenNestedMap extracts a summary from nested objects like Anthropic's capabilities.
// For {thinking: {supported: true}, image_input: {supported: true}} → "thinking, image input"
func flattenNestedMap(parentKey string, m map[string]any) []KeyValue {
	var supported []string
	for k, v := range m {
		switch sub := v.(type) {
		case map[string]any:
			if s, ok := sub["supported"].(bool); ok && s {
				supported = append(supported, formatMetadataKey(k))
			}
		case bool:
			if sub {
				supported = append(supported, formatMetadataKey(k))
			}
		case string:
			if sub != "" {
				supported = append(supported, fmt.Sprintf("%s: %s", formatMetadataKey(k), sub))
			}
		}
	}
	if len(supported) == 0 {
		return nil
	}
	sort.Strings(supported)
	return []KeyValue{{Key: parentKey, Value: strings.Join(supported, ", ")}}
}

// formatMetadataKey converts snake_case or camelCase to Title Case.
func formatMetadataKey(key string) string {
	// Handle common display overrides
	switch key {
	case "max_output_tokens":
		return "Max Output"
	case "max_input_tokens":
		return "Max Input"
	case "max_tokens":
		return "Max Output"
	case "display_name":
		return "Display Name"
	case "deprecation_date":
		return "Deprecated"
	case pricing.MetadataKeyCapabilities:
		return "Capabilities"
	case pricing.MetadataKeyInputTypes:
		return "Input"
	case "owned_by":
		return "Owner"
	}

	// Split on underscores and camelCase boundaries
	key = strings.ReplaceAll(key, "_", " ")

	// Capitalize each word
	words := strings.Fields(key)
	for i, w := range words {
		if len(w) > 0 {
			runes := []rune(w)
			runes[0] = unicode.ToUpper(runes[0])
			words[i] = string(runes)
		}
	}
	return strings.Join(words, " ")
}

// sortMetadata orders pairs by priority, then alphabetically.
func sortMetadata(pairs []KeyValue) {
	sort.Slice(pairs, func(i, j int) bool {
		oi := metadataDisplayPriority[pairs[i].Key]
		oj := metadataDisplayPriority[pairs[j].Key]
		if oi == 0 {
			oi = defaultMetadataPriority
		}
		if oj == 0 {
			oj = defaultMetadataPriority
		}
		if oi != oj {
			return oi < oj
		}
		return pairs[i].Key < pairs[j].Key
	})
}

func formatNumberWithCommas(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}
