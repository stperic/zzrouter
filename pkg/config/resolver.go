// Package config provides the tier resolver over the provider parameter tree.
//
// Resolve walks defaults → model defaults → model → node → node-model for a
// single (provider, node, model) triple and returns a map of key →
// ResolvedValue carrying provenance. Model defaults are the release's
// (reconcile-managed); every other tier is the operator's. The merge order
// matches plan §4.2; null keys are never persisted per plan §4.3, so
// presence-means-set holds at every tier.
package config

import (
	"fmt"
	"strings"
)

// defaultEndpoint mirrors prov_apps.EndpointChat; can't import the typed
// constant due to the prov_apps→config dependency direction.
const defaultEndpoint = "chat"

// IsEndpointAware reports whether any tier in this provider's config
// declares an `endpoints:` overlay. The launcher uses this to decide
// whether per-endpoint instances coexist for one model: providers that
// declare overlays opt in by data, no separate flag.
func (s *ServiceConfig) IsEndpointAware() bool {
	if s.Defaults != nil && len(s.Defaults.Endpoints) > 0 {
		return true
	}
	for _, ms := range s.Models {
		if len(ms.Endpoints) > 0 {
			return true
		}
	}
	for _, ns := range s.Nodes {
		if len(ns.Endpoints) > 0 {
			return true
		}
		for _, nm := range ns.Models {
			if len(nm.Endpoints) > 0 {
				return true
			}
		}
	}
	return false
}

// Resolve is the 2-arg shim for list/inspection callers; dispatch callers
// should use ResolveEndpoint to thread the request endpoint.
func (s *ServiceConfig) Resolve(node, model string) ResolvedParams {
	return s.ResolveEndpoint(node, model, defaultEndpoint)
}

// ResolveEndpoint walks the tier tree and applies the endpoints[E]
// overlay after each tier's base. Unknown endpoint silently no-ops;
// validation lives at dispatch.
//
// A variant (a models entry with From) walks its base's cells first and
// its own after them at the model and node × model tiers, and takes its
// model defaults from its base: the weights decide the family.
func (s *ServiceConfig) ResolveEndpoint(node, model, endpoint string) ResolvedParams {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	out := ResolvedParams{
		Parameters:  make(map[string]ResolvedValue),
		Environment: make(map[string]ResolvedValue),
		Request:     make(map[string]ResolvedValue),
	}
	// apply lays one tier's base and then its overlay for endpoint over
	// what the lower tiers left.
	apply := func(params, env map[string]string, endpoints map[string]EndpointOverlay, src ResolvedValue) {
		set := func(dst map[string]ResolvedValue, from map[string]string, src ResolvedValue) {
			for k, v := range from {
				src.Value = v
				dst[k] = src
			}
		}
		set(out.Parameters, params, src)
		set(out.Environment, env, src)
		if ov, ok := endpoints[endpoint]; ok {
			src.Endpoint = endpoint
			set(out.Parameters, ov.Parameters, src)
			set(out.Environment, ov.Environment, src)
		}
	}
	applyRequest := func(request map[string]any, src ResolvedValue) {
		for k, v := range request {
			src.Value = v
			out.Request[k] = src
		}
	}

	// The model cells to walk, lowest first: the base's, then the
	// variant's own.
	cells := []string{model}
	if name, base, ok := s.Variant(model); ok {
		out.From = base
		cells = []string{base, name}
	}

	// Tier 0 — defaults.
	if s.Defaults != nil {
		apply(s.Defaults.Parameters, s.Defaults.Environment, s.Defaults.Endpoints, ResolvedValue{Tier: TierDefault})
	}

	// Tier 1 — model.
	for _, m := range cells {
		if key, ms, ok := matchSpec(s.Models, m); ok {
			src := ResolvedValue{Tier: TierModel, Model: m, Pattern: patternIfNot(key, m)}
			apply(ms.Parameters, ms.Environment, ms.Endpoints, src)
			applyRequest(ms.Request, src)
		}
	}

	// Tier 2 — node, then Tier 3 — node × model.
	if ns, ok := s.Nodes[node]; ok && node != "" {
		apply(ns.Parameters, ns.Environment, ns.Endpoints, ResolvedValue{Tier: TierNode, Node: node})
		for _, m := range cells {
			if key, nm, ok := matchSpec(ns.Models, m); ok {
				apply(nm.Parameters, nm.Environment, nm.Endpoints, ResolvedValue{Tier: TierNodeModel, Node: node, Model: m, Pattern: patternIfNot(key, m)})
			}
		}
	}

	return out
}

// patternIfNot is key unless key is the model's own name, which needs no
// separate mention in provenance.
func patternIfNot(key, model string) string {
	if strings.EqualFold(key, model) {
		return ""
	}
	return key
}

// FlattenParameters turns a ResolvedParams.Parameters map into the flat
// map[string]string the launcher consumes. Non-string values are coerced
// via fmt.Sprintf to preserve today's launcher behavior.
func FlattenParameters(rp map[string]ResolvedValue) map[string]string {
	out := make(map[string]string, len(rp))
	for k, rv := range rp {
		out[k] = coerceToString(rv.Value)
	}
	return out
}

// FlattenEnvironment mirrors FlattenParameters for env vars.
func FlattenEnvironment(rp map[string]ResolvedValue) map[string]string {
	return FlattenParameters(rp)
}

func coerceToString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprint(s)
	}
}

// matchSpec picks the best spec for a model name from a map keyed by
// glob pattern: exact (case-insensitive) first, then most-specific
// wildcard. Works for ModelSpec (Tier 1) and NodeModelSpec (Tier 3)
// — same selection rule, one body. It returns the key that matched.
func matchSpec[T any](specs map[string]T, model string) (string, T, bool) {
	var zero T
	if len(specs) == 0 || model == "" {
		return "", zero, false
	}
	for pattern, spec := range specs {
		if strings.EqualFold(pattern, model) {
			return pattern, spec, true
		}
	}
	normalized := strings.ToLower(model)
	bestKey, best := "", zero
	bestSpec := 0
	found := false
	for pattern, spec := range specs {
		if matchesModelPattern(normalized, strings.ToLower(pattern)) {
			// Specificity goes negative for a short glob ("qwen*" scores
			// -6), so the first match is taken whatever its score. Ties go
			// to the lexically smaller key so the choice does not depend on
			// map order.
			sp := calculatePatternSpecificity(pattern)
			if !found || sp > bestSpec || (sp == bestSpec && pattern < bestKey) {
				bestSpec = sp
				bestKey, best = pattern, spec
				found = true
			}
		}
	}
	return bestKey, best, found
}

// autoValue is the sentinel a parameter carries to mean "the provider
// decides"; the flag is dropped rather than passed through.
const autoValue = "auto"

// IsAuto reports whether a resolved value is the "provider decides"
// sentinel, which names nothing and is never passed to the engine.
func IsAuto(v string) bool { return v == autoValue }

// FilterAutoValues strips keys whose value is exactly "auto", which is
// the tree's way of saying "let the provider compute this" rather than
// "pass the literal string auto on the command line".
//
// It lives beside the resolver because it is the last step of parameter
// resolution, not a presentation concern: every launch path has to apply
// it or the provider receives a flag it cannot parse. Applying it after
// the request tier is deliberate, so a caller can neutralise a
// configured value by sending "auto" for it.
//
// The input map is returned unchanged when no "auto" is present, so the
// common case allocates nothing.
func FilterAutoValues(parameters map[string]string) map[string]string {
	hasAuto := false
	for _, v := range parameters {
		if v == autoValue {
			hasAuto = true
			break
		}
	}
	if !hasAuto {
		return parameters
	}
	filtered := make(map[string]string, len(parameters))
	for k, v := range parameters {
		if v != autoValue {
			filtered[k] = v
		}
	}
	return filtered
}
