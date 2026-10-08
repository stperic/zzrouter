package cache

import (
	"maps"
	"strings"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// FeatureState returns source-backed feature evidence. Older node reports
// and malformed values remain unknown.
func (m *CachedModel) FeatureState(name string) string {
	if m == nil {
		return "unknown"
	}
	var state string
	switch features := m.Details["features"].(type) {
	case map[string]string:
		state = features[name]
	case map[string]any:
		state, _ = features[name].(string)
	}
	switch state {
	case "present", "available", "unsupported":
		return state
	}
	return "unknown"
}

// WireEndpoints returns the node's effective endpoints and whether it
// supplied a declaration, including an explicitly empty one.
func (m *CachedModel) WireEndpoints() ([]string, bool) {
	if m == nil {
		return nil, false
	}
	switch eps := m.Details["wire_endpoints"].(type) {
	case []string:
		return eps, true
	case []any:
		out := make([]string, 0, len(eps))
		for _, ep := range eps {
			s, ok := ep.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// EnrichLocalItems expands variants before applying node-local serving policy.
// Worker evidence is published here and preserved by coordinator aggregation.
func EnrichLocalItems(items []map[string]any, cfg *pkgConfig.AppsConfig, enrich func(string, string, map[string]any) map[string]any) []map[string]any {
	models := make([]*CachedModel, 0, len(items))
	byPlace := map[string]map[string]any{}
	for _, item := range items {
		m := &CachedModel{Name: extractStringField(item, "name"), Node: extractStringField(item, "node"), Provider: extractStringField(item, "assigned_app")}
		m.IsCloud, _ = item["is_cloud"].(bool)
		models = append(models, m)
		byPlace[strings.ToLower(m.Name)+"\x00"+m.Node+"\x00"+m.Provider] = item
	}
	for _, variant := range expandVariants(models, cfg) {
		key := strings.ToLower(variant.VariantOf) + "\x00" + variant.Node + "\x00"
		source := byPlace[key+variant.Provider]
		if source == nil {
			source = byPlace[key]
		}
		item := maps.Clone(source)
		item["name"], item["model"], item["assigned_app"] = variant.Name, variant.Name, variant.Provider
		item["variant_of"] = variant.VariantOf
		delete(item, "source_id")
		items = append(items, item)
	}
	if enrich != nil {
		for _, item := range items {
			details, _ := item["details"].(map[string]any)
			item["details"] = enrich(extractStringField(item, "assigned_app"), extractStringField(item, "name"), details)
		}
	}
	return items
}
