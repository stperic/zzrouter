package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
)

// paramPatchBody is the RFC 7396 Merge-Patch wire shape for
// PATCH /providers/:name/parameters. json.RawMessage lets us distinguish
// absent keys (no-op), explicit null (delete), and set values.
type paramPatchBody struct {
	Defaults *defaultsPatch         `json:"defaults,omitempty"`
	Models   map[string]*modelPatch `json:"models,omitempty"`
	Nodes    map[string]*nodePatch  `json:"nodes,omitempty"`
}

// defaultsPatch owns shared runtime recipes; model patches cannot acquire them.
type defaultsPatch struct {
	Parameters  map[string]json.RawMessage `json:"parameters,omitempty"`
	Environment map[string]json.RawMessage `json:"environment,omitempty"`
	Endpoints   map[string]*endpointPatch  `json:"endpoints,omitempty"`
	Install     json.RawMessage            `json:"install,omitempty"`
}

// modelPatch is a models.<model> cell: the leaf blocks every tier holds,
// plus what only a model cell holds. From makes the entry a variant over
// that model's weights; null makes it a plain entry again. Request holds
// request-time body defaults, merged per RFC 7396 (null deletes a field).
// Neither is a parameter, so neither is schema-validated as one.
type modelPatch struct {
	specPatch
	From    json.RawMessage `json:"from,omitempty"`
	Request json.RawMessage `json:"request,omitempty"`
}

// specPatch covers a (parameters, environment) leaf block — used at the
// defaults, model, node, and node-model tiers since all four carry the
// same two maps. Endpoints is the per-tier endpoints overlay map; a nil
// value at endpoints.X deletes the whole overlay (RFC 7396).
type specPatch struct {
	Parameters  map[string]json.RawMessage `json:"parameters,omitempty"`
	Environment map[string]json.RawMessage `json:"environment,omitempty"`
	Endpoints   map[string]*endpointPatch  `json:"endpoints,omitempty"`
}

// endpointPatch is the leaf shape under endpoints.<name>. Same two maps
// as specPatch; kept distinct because endpoint overlays don't recurse
// (no endpoints-inside-endpoints) and the type discipline catches a
// future mistake of nesting deeper than one level.
type endpointPatch struct {
	Parameters  map[string]json.RawMessage `json:"parameters,omitempty"`
	Environment map[string]json.RawMessage `json:"environment,omitempty"`
}

// nodePatch is specPatch plus the Tier-3 models submap.
type nodePatch struct {
	Install     json.RawMessage            `json:"install,omitempty"`
	Parameters  map[string]json.RawMessage `json:"parameters,omitempty"`
	Environment map[string]json.RawMessage `json:"environment,omitempty"`
	Endpoints   map[string]*endpointPatch  `json:"endpoints,omitempty"`
	Models      map[string]*specPatch      `json:"models,omitempty"`
}

// knownModelsForProvider returns the union of model names declared in
// cfg.Models and every cfg.Nodes[*].Models. Used to reject orphan-cell
// creation (plan §7.1). Wildcard patterns pass through because the
// resolver already supports glob matching (resolver.go).
func knownModelsForProvider(cfg *pkgConfig.ServiceConfig) map[string]bool {
	out := make(map[string]bool)
	for m := range cfg.Models {
		out[m] = true
	}
	for _, n := range cfg.Nodes {
		for m := range n.Models {
			out[m] = true
		}
	}
	return out
}

// hasWildcard reports whether the pattern has a glob meta-char; glob
// model names are accepted without a known-model lookup.
func hasWildcard(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '*' || s[i] == '?' {
			return true
		}
	}
	return false
}

// validatePatch runs all plan §7 checks without mutating config.
// Returns one entry per failing key, nil when the patch is clean. A
// slice rather than the first failure: a patch is a set of independent
// keys and a caller fixing them one round-trip at a time is the
// experience this endpoint exists to avoid.
func validatePatch(provider string, cfg *pkgConfig.ServiceConfig, patch *paramPatchBody,
	mergedSchema *schema.ProviderSchema, scope patchScope,
) []utils.ParamError {
	var errs []utils.ParamError
	knownNodes := scope.nodes
	knownModels := knownModelsForProvider(cfg)
	flat := mergedSchema.Parameters

	validate := func(key string, raw json.RawMessage, isEnv bool, shapes map[string]schema.ParamShape) *utils.ParamError {
		if !isEnv && mergedSchema.Diagnostics != nil && mergedSchema.Diagnostics.Memory != nil {
			budget := mergedSchema.Diagnostics.Memory
			var value string
			if budget.Kind == "fraction_total" && key == budget.Parameter && json.Unmarshal(raw, &value) == nil && value == "auto" {
				return nil
			}
		}
		return validateLeaf(key, raw, isEnv, shapes)
	}
	check := func(pp map[string]json.RawMessage, isEnv bool) {
		for k, raw := range pp {
			e := validate(k, raw, isEnv, flat)
			if e != nil {
				errs = append(errs, *e)
			}
		}
	}
	checkEndpoints := func(eps map[string]*endpointPatch) {
		// Endpoint overlay parameters look up the per-endpoint schema first
		// (so endpoint-scoped enums/ranges win) and fall back to the flat
		// schema when an endpoint hasn't declared the key explicitly.
		for epName, ep := range eps {
			if ep == nil {
				continue // null = delete; nothing to validate
			}
			scoped := mergedSchema.ForEndpoint(epName)
			for k, raw := range ep.Parameters {
				if e := validate(k, raw, false, scoped); e != nil {
					errs = append(errs, *e)
				}
			}
			for k, raw := range ep.Environment {
				if e := validate(k, raw, true, flat); e != nil {
					errs = append(errs, *e)
				}
			}
		}
	}
	checkSpec := func(p *specPatch) {
		if p == nil {
			return
		}
		check(p.Parameters, false)
		check(p.Environment, true)
		checkEndpoints(p.Endpoints)
	}

	if patch.Defaults != nil {
		checkSpec(&specPatch{Parameters: patch.Defaults.Parameters, Environment: patch.Defaults.Environment, Endpoints: patch.Defaults.Endpoints})
	}
	// Tier 1 models.X is a declaration — first write to an empty tree has
	// to pass. The orphan-cell guard fires on Tier 3 nodes.N.models.X,
	// which must reference a model already declared at Tier 1 or in
	// another node's Tier 3.
	for m, mp := range patch.Models {
		if mp == nil {
			continue // null = delete; nothing to validate
		}
		checkSpec(&mp.specPatch)
		errs = append(errs, validateModelCell(m, mp, scope)...)
	}
	// Tier 1 declarations in this patch become knownModels for the Tier-3
	// check below, so a single PATCH can atomically declare a model and
	// pin its (node, model) cell.
	for m := range patch.Models {
		if m != "" {
			knownModels[m] = true
		}
	}
	for n, np := range patch.Nodes {
		if !knownNodes[n] {
			errs = append(errs, utils.ParamError{
				Key:     "nodes." + n,
				Code:    string(httperr.CodeUnknownNode),
				Message: fmt.Sprintf("node %q is not a known cluster node", n),
			})
			continue
		}
		if np == nil {
			continue
		}
		check(np.Parameters, false)
		check(np.Environment, true)
		checkEndpoints(np.Endpoints)
		for m, mp := range np.Models {
			if !hasWildcard(m) && !knownModels[m] {
				errs = append(errs, utils.ParamError{
					Key:     fmt.Sprintf("nodes.%s.models.%s", n, m),
					Code:    string(httperr.CodeUnknownModel),
					Message: fmt.Sprintf("model %q is not known to provider %q", m, provider),
				})
				continue
			}
			checkSpec(mp)
		}
	}

	return errs
}

// patchScope is what a patch is checked against beyond its own tree.
type patchScope struct {
	nodes map[string]bool
	// weights reports the node holding real weights of a name; nil when
	// no catalog is wired.
	weights func(name string) (node string, ok bool)
}

// validateModelCell checks the shape of what only a model cell holds:
// from, which makes it a variant, and its request defaults. What a tree
// may hold (names, one level deep, reserved fields, one engine per name)
// is pkg/config's rule, checked on the patched tree under the store lock;
// the catalog is the one thing it cannot see. See
// docs/plan_model_templates_and_variants.md.
func validateModelCell(model string, mp *modelPatch, scope patchScope) []utils.ParamError {
	var errs []utils.ParamError
	fail := func(key string, code httperr.ParamErrorCode, want, format string, args ...any) {
		errs = append(errs, utils.ParamError{Key: key, Code: string(code), Want: want, Message: fmt.Sprintf(format, args...)})
	}
	fromKey := "models." + model + ".from"

	if len(mp.From) > 0 && !isJSONNull(mp.From) {
		var base string
		if err := json.Unmarshal(mp.From, &base); err != nil || base == "" {
			fail(fromKey, httperr.CodeWrongType, "string", "%s must name the model whose weights the variant runs", fromKey)
		} else if scope.weights != nil {
			// The launch would take the name for a variant and load its
			// base, serving other weights under that model's name.
			if node, ok := scope.weights(model); ok {
				fail(fromKey, httperr.CodeInvalidValue, "a name no model's weights carry",
					"%q names real weights on %s; a variant needs a name of its own", model, node)
			}
		}
	}

	if len(mp.Request) > 0 && !isJSONNull(mp.Request) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(mp.Request, &fields); err != nil || fields == nil {
			fail("models."+model+".request", httperr.CodeWrongType, "object",
				"models.%s.request must be an object of request-body fields", model)
		}
	}
	return errs
}

// validateLeaf validates a single leaf value against the merged schema.
// Environment values are free-form strings; only parameter leaves are
// schema-gated today.
func validateLeaf(key string, raw json.RawMessage, isEnv bool, sch map[string]schema.ParamShape) *utils.ParamError {
	// null is always valid (delete-if-present).
	if isJSONNull(raw) {
		return nil
	}
	if isEnv {
		// Env: accept strings only (plan leaves env schema unmodeled).
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: "string",
				Message: fmt.Sprintf("environment value for %q must be a JSON string", key)}
		}
		// Run env-name + env-value constraints here so partial patches
		// never land on disk (H4 — validate pre-apply).
		if err := validateEnvKey(key); err != nil {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeCoercionFailed), Message: err.Error()}
		}
		if err := validateEnvValue(key, s); err != nil {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeCoercionFailed), Message: err.Error()}
		}
		return nil
	}
	shape, known := sch[key]
	if !known {
		return &utils.ParamError{Key: key, Code: string(httperr.CodeUnknownFlag),
			Message: fmt.Sprintf("unknown parameter %q", key)}
	}
	return validateAgainstShape(key, raw, shape)
}

func validateAgainstShape(key string, raw json.RawMessage, shape schema.ParamShape) *utils.ParamError {
	switch shape.Kind {
	case schema.ParamInt:
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n != float64(int64(n)) {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: "int",
				Message: fmt.Sprintf("%s must be an integer", key)}
		}
		if shape.Min != nil && n < *shape.Min {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeOutOfRange), Got: n, Min: *shape.Min,
				Message: fmt.Sprintf("%s=%v below minimum %v", key, n, *shape.Min)}
		}
		if shape.Max != nil && n > *shape.Max {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeOutOfRange), Got: n, Max: *shape.Max,
				Message: fmt.Sprintf("%s=%v exceeds maximum %v", key, n, *shape.Max)}
		}
	case schema.ParamFloat:
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: "float",
				Message: fmt.Sprintf("%s must be a number", key)}
		}
		if shape.Min != nil && n < *shape.Min {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeOutOfRange), Got: n, Min: *shape.Min,
				Message: fmt.Sprintf("%s=%v below minimum %v", key, n, *shape.Min)}
		}
		if shape.Max != nil && n > *shape.Max {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeOutOfRange), Got: n, Max: *shape.Max,
				Message: fmt.Sprintf("%s=%v exceeds maximum %v", key, n, *shape.Max)}
		}
	case schema.ParamBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: "bool",
				Message: fmt.Sprintf("%s must be a boolean", key)}
		}
	case schema.ParamString, "":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: "string",
				Message: fmt.Sprintf("%s must be a string", key)}
		}
		if len(shape.Enum) > 0 {
			ok := false
			for _, allowed := range shape.Enum {
				if s == allowed {
					ok = true
					break
				}
			}
			if !ok {
				return &utils.ParamError{Key: key, Code: string(httperr.CodeCoercionFailed),
					Message: fmt.Sprintf("%s=%q not in allowed set %v", key, s, shape.Enum)}
			}
		}
	case schema.ParamAsset:
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: "string",
				Message: fmt.Sprintf("%s must be an asset name", key)}
		}
		// Existence is checked when the patch is applied, under the lock
		// asset writes take, so a concurrent delete cannot slip between.
		if err := assets.ValidateName(name); err != nil {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeInvalidValue), Want: "asset name",
				Message: fmt.Sprintf("%s: %v", key, err)}
		}
	default:
		// The kinds are a closed set; one this switch does not know would
		// otherwise pass any value unchecked.
		return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType),
			Message: fmt.Sprintf("%s has schema type %q, which this server cannot validate", key, shape.Kind)}
	}
	return nil
}

// isJSONNull reports whether a RawMessage is literally `null`.
func isJSONNull(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	// Trim ASCII whitespace that json.RawMessage may carry.
	i, j := 0, len(raw)
	for i < j && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
		i++
	}
	for j > i && (raw[j-1] == ' ' || raw[j-1] == '\t' || raw[j-1] == '\n' || raw[j-1] == '\r') {
		j--
	}
	return j-i == 4 && string(raw[i:j]) == "null"
}

// rawToString coerces a validated non-null RawMessage to its string form
// for persistence. Numbers and bools are stringified so the existing
// map[string]string on-disk shape holds.
func rawToString(raw json.RawMessage) (string, error) {
	if isJSONNull(raw) {
		return "", fmt.Errorf("null cannot be coerced to string")
	}
	// String: decode to strip quotes.
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	// Numbers/bools/other: raw JSON text is the canonical string.
	return string(raw), nil
}

// decodePatchBody decodes a merge-patch body, rejecting any field the
// shape does not define. Writes the 400 and returns false on failure.
//
// Strict because this is the single mutator for provider parameters and a
// silently-dropped field here reports success for a change that never
// happened. The natural wrong guess is the flat one -- `{"environment":
// {...}}` -- because that was the shape of the retired PUT and it is also
// how GET /resolved answers. Under json.Unmarshal that body returned 200
// with the unchanged resolved view and wrote nothing.
//
// Strictness stops at the struct level. The maps inside a leaf block are
// parameter and environment names, which are open by construction; an
// unrecognized one is CodeUnknownFlag from validatePatch against the
// merged schema, not a structural error.
func decodePatchBody(c *gin.Context, raw []byte) (*paramPatchBody, bool) {
	var patch paramPatchBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(&patch)
	if err == nil {
		if dec.More() {
			BadRequest(c, "merge-patch "+errTrailingJSON.Error())
			return nil, false
		}
		return &patch, true
	}

	if name, ok := unknownFieldName(err); ok {
		RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", []utils.ParamError{{
			Key:     strings.Trim(name, `"`),
			Code:    string(httperr.CodeUnknownField),
			Want:    "defaults | models.<model> | nodes.<node>[.models.<model>], each holding parameters | environment | endpoints, and models.<model> also from | request",
			Message: unknownFieldAdvice(strings.Trim(name, `"`)),
		}})
		return nil, false
	}
	BadRequest(c, "invalid merge-patch body: "+err.Error())
	return nil, false
}

// leafBlocks are the keys a tier holds. A rejected name that is one of
// these was sent at the wrong LEVEL; anything else is simply not a field
// this body has.
var leafBlocks = map[string]bool{"parameters": true, "environment": true, "endpoints": true, "install": true}

// unknownFieldAdvice tailors the rejection to which mistake was made.
// encoding/json reports no path with the name, so the name itself is the
// only signal -- which is enough to separate the one wrong guess worth
// naming from a plain typo. Telling someone who sent
// {"defaults":{"env":{...}}} to "send {"defaults":{...}}" is advice they
// already followed.
func unknownFieldAdvice(name string) string {
	if name == "model_defaults" {
		return "model_defaults ships with zzRouter and is rewritten on every start, so this body cannot set it. " +
			"Every tier this endpoint writes resolves above it: set the key under models.<model>, " +
			"or set an asset-typed key to \"auto\" there to drop the shipped file"
	}
	if leafBlocks[name] {
		return fmt.Sprintf("unknown field %q at the top level: every %s block lives under a tier, "+
			"so this body sets nothing. Wrap it: {\"defaults\":{%q:{...}}}", name, name, name)
	}
	return fmt.Sprintf("unknown field %q: this endpoint rejects fields it does not define rather than "+
		"ignoring them. See `want` for the shape", name)
}
