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
	"maps"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/config/runsettings"
)

// defaultEndpoint mirrors prov_apps.EndpointChat; can't import the typed
// constant due to the prov_apps→config dependency direction.
const defaultEndpoint = "chat"

// IsEndpointAware keeps endpoint overlays and runtime features on separate runs.
func (s *ServiceConfig) IsEndpointAware() bool {
	for name := range s.Features {
		if endpoint := FeatureEndpoint(name); endpoint != "" && endpoint != defaultEndpoint {
			return true
		}
	}
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
		setEnv := func(from map[string]string, src ResolvedValue) {
			for k, v := range from {
				src.Key, src.Value = k, v
				out.Environment[k] = src
			}
		}
		LayParameters(out.Parameters, params, src)
		setEnv(env, src)
		if ov, ok := endpoints[endpoint]; ok {
			src.Endpoint = endpoint
			LayParameters(out.Parameters, ov.Parameters, src)
			setEnv(ov.Environment, src)
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

	// The legacy runtime value is the default tier, overridden by the tree.
	if s.Runtime != nil && s.Runtime.KeepAlive != "" {
		out.Parameters[KeepAliveParameter] = ResolvedValue{Value: s.Runtime.KeepAlive, Tier: TierDefault, Key: KeepAliveParameter}
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

// FlagName is how argv spells a parameter key, after "--": parameter_name
// and parameter-name are one flag, which every merge of tiers keys by.
func FlagName(key string) string {
	return strings.ReplaceAll(key, "_", "-")
}

// LayParameters lays one map of parameters over dst by FlagName, stamped with
// src, so the later map in the walk wins whatever either spelled. Spellings in
// params apply in sorted order, the flag name first; one whose value differs
// is recorded in Conflicts rather than chosen by map order.
func LayParameters(dst map[string]ResolvedValue, params map[string]string, src ResolvedValue) {
	laid := make(map[string]bool, len(params))
	for _, key := range slices.Sorted(maps.Keys(params)) {
		flag := FlagName(key)
		if !laid[flag] {
			laid[flag] = true
			src.Key, src.Value = key, params[key]
			dst[flag] = src
		} else if first := dst[flag]; first.Value != params[key] {
			first.Conflicts = append(first.Conflicts, key)
			dst[flag] = first
		}
	}
}

// SetParameter stores value under key's flag name in params and removes the
// flag's other spellings, so a write leaves one; it returns params,
// allocated when nil.
func SetParameter(params map[string]string, key, value string) map[string]string {
	DeleteParameter(params, key)
	if params == nil {
		params = make(map[string]string, 1)
	}
	params[FlagName(key)] = value
	return params
}

// DeleteParameter removes every spelling of key's flag from params.
func DeleteParameter(params map[string]string, key string) {
	maps.DeleteFunc(params, func(stored, _ string) bool { return FlagName(stored) == FlagName(key) })
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

// IsAuto identifies the user-facing request for provider-selected behavior.
func IsAuto(v string) bool { return v == runsettings.Auto }
