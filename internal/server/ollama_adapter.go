// OllamaAdapter implements ProtocolAdapter for the /api/* Ollama-compat
// surface. It owns the mapping from canonical cache.CachedModel /
// ShowModelResponse shapes to Ollama's wire format (OllamaTagEntry,
// OllamaTagsResponse, etc.), preserving the field-level translations
// (e.g. quant_level → quantization_level) that real Ollama clients depend
// on.
package server

import "github.com/stperic/zzrouter/pkg/model/cache"

// OllamaAdapter has no per-instance state; the canonical inputs carry
// everything needed to produce a wire-format entry.
type OllamaAdapter struct{}

// NewOllamaAdapter constructs the adapter.
func NewOllamaAdapter() *OllamaAdapter {
	return &OllamaAdapter{}
}

// ListEntry translates a cache.CachedModel into an OllamaTagEntry. Field-level
// notes:
//   - `name` / `model`: both populated for compatibility with clients that
//     read either.
//   - `details.quantization_level`: sourced from cache.CachedModel.Details or
//     .Extra "quant_level" (the internal naming differs from the Ollama
//     wire field name; the translation must not be lost).
//   - `details.format` / `family` / `parameter_size`: same pattern.
func (a *OllamaAdapter) ListEntry(m *cache.CachedModel) any {
	return OllamaTagEntry{
		Name:       m.Name,
		Model:      m.Model,
		Size:       m.Size,
		ModifiedAt: m.Modified,
		Digest:     m.Digest,
		Details:    cachedModelToOllamaDetails(m),
	}
}

// ListEnvelope wraps the translated entries in Ollama's list envelope.
// The slice is rebuilt as []OllamaTagEntry so the JSON encoder produces
// a typed array, not []any.
func (a *OllamaAdapter) ListEnvelope(entries []any) any {
	models := make([]OllamaTagEntry, 0, len(entries))
	for _, e := range entries {
		if entry, ok := e.(OllamaTagEntry); ok {
			models = append(models, entry)
		}
	}
	return OllamaTagsResponse{Models: models}
}

// ShowResponse translates a ShowModelResponse into the body Ollama clients
// expect from POST /api/show. The response body is the raw provider detail
// map (which, for Ollama-backed models, already matches Ollama's native
// /api/show shape: modelfile / parameters / template / details / ...).
//
// When r.Route is populated (model has ≥2 replicas in an auto-route
// group), the adapter additionally emits a `zzrouter_route` extension
// field. Vanilla Ollama clients tolerate unknown fields; dashboards that
// know about the extension can surface cluster topology.
func (a *OllamaAdapter) ShowResponse(r *ShowModelResponse) any {
	if r == nil {
		return nil
	}

	// Start from the raw daemon details (this is already Ollama-shaped).
	out := map[string]any{}
	for k, v := range r.Details {
		out[k] = v
	}

	if r.Route != nil {
		out["zzrouter_route"] = r.Route
	}
	return out
}

// cachedModelToOllamaDetails extracts the four Ollama-detail fields from
// the canonical cache.CachedModel. Looks in both Details and Extra to tolerate
// either source-of-truth (some providers populate one, some the other).
func cachedModelToOllamaDetails(m *cache.CachedModel) OllamaModelDetails {
	d := OllamaModelDetails{Format: m.Format}
	if d.Format == "" {
		d.Format = stringFromMaps("format", m.Details, m.Extra)
	}
	d.Family = stringFromMaps("family", m.Details, m.Extra)
	d.ParameterSize = stringFromMaps("parameter_size", m.Details, m.Extra)
	// Ollama wire field is "quantization_level"; some internal sources use
	// the shorter "quant_level". Try both — first hit wins.
	d.QuantLevel = stringFromMaps("quant_level", m.Details, m.Extra)
	if d.QuantLevel == "" {
		d.QuantLevel = stringFromMaps("quantization_level", m.Details, m.Extra)
	}
	return d
}

// stringFromMaps looks up `key` in the given maps in order and returns the
// first non-empty string value. Returns "" if not present in any map.
func stringFromMaps(key string, maps ...map[string]any) string {
	for _, m := range maps {
		if m == nil {
			continue
		}
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
