package search

import "fmt"

// metadataEquivalents maps external-metadata field names to provider-native
// field names that carry the same data. When a provider already has one of
// the equivalent fields, the external field is skipped during merge.
var metadataEquivalents = map[string][]string{
	"context_length":    {"max_input_tokens", "context_window"},
	"max_output_tokens": {"max_tokens"},
}

// MergeMetadataWithPriority merges source fields into target in place.
// Target (provider-native) data takes priority — source only fills gaps.
// Uses metadataEquivalents to detect semantically equivalent keys so a
// provider's "context_window" will suppress a source's "context_length".
func MergeMetadataWithPriority(target, source map[string]any) {
	for k, v := range source {
		if _, exists := target[k]; exists {
			continue
		}
		if equivs, ok := metadataEquivalents[k]; ok {
			hasEquiv := false
			for _, eq := range equivs {
				if _, exists := target[eq]; exists {
					hasEquiv = true
					break
				}
			}
			if hasEquiv {
				continue
			}
		}
		target[k] = v
	}
}

// EnrichCloudResultsWithURL injects the model page URL into each cloud
// search result using the provider's registered ModelURLFmt. Must be
// called after NormalizeCloudResults since it reads the normalized
// "api_id" key as the URL substitution token.
//
// Results that are not maps, or lack an "api_id" string, are left alone
// — the function is defensive by design so a single malformed entry
// does not poison the batch.
func EnrichCloudResultsWithURL(results []any, provider Provider) {
	entry, ok := GetCloudProvider(provider)
	if !ok || entry.Meta.ModelURLFmt == "" {
		return
	}
	for _, result := range results {
		m, isMap := result.(map[string]any)
		if !isMap {
			continue
		}
		apiID, _ := m["api_id"].(string)
		if apiID == "" {
			continue
		}
		m["url"] = fmt.Sprintf(entry.Meta.ModelURLFmt, apiID)
	}
}
