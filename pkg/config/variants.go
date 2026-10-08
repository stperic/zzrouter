package config

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/constants"
)

// variantNamePattern is what a variant may be called. The router never
// parses a variant's name (From carries its base), so the name only has
// to stay clear of every grammar a model name already has: glob
// characters (model keys are patterns), ':' (an Ollama tag), '@' and
// "::" (a node), '#' (a GGUF file) and '/' (a repository). '+' is free,
// hence the suggested <base>+<variant>.
var variantNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// ErrInvalidVariant is a variant definition the tree cannot hold.
var ErrInvalidVariant = errors.New("invalid variant")

// ErrModelNameConflict means real weights occupy a configured variant name.
var ErrModelNameConflict = errors.New("model name conflict")

// ModelNameConflictError identifies the name that must be disambiguated.
type ModelNameConflictError struct {
	Model string
	Node  string
}

func (e *ModelNameConflictError) Error() string {
	location := "in the local model store"
	if e.Node != "" {
		location = "on node " + e.Node
	}
	return fmt.Sprintf("%s: %q names both a configured variant and real weights %s; rename the variant or remove its from definition", ErrModelNameConflict, e.Model, location)
}

func (e *ModelNameConflictError) Unwrap() error { return ErrModelNameConflict }

// ValidateVariantName reports whether name can be a variant's name.
func ValidateVariantName(name string) error {
	if !variantNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q must match %s: letters, digits and . _ + -, without the "+
			"characters model names use for globs, tags, nodes, files and repositories", ErrInvalidVariant, name, variantNamePattern)
	}
	return nil
}

// reservedRequestFields are the body fields a model's request defaults
// may not set: they carry the conversation or the routing, and the
// router passes those through as the client sent them.
var reservedRequestFields = []string{"input", "messages", "model", "prompt", "stream", "system", "tools"}

// isReservedRequestField reports whether request defaults may not set
// key. Case is ignored: engines that decode JSON case-insensitively would
// read "Tools" as tools.
func isReservedRequestField(key string) bool {
	return slices.ContainsFunc(reservedRequestFields, func(f string) bool { return strings.EqualFold(f, key) })
}

// ModelProblem is a models or model_defaults entry the tree cannot hold,
// at the merge-patch path of the field at fault.
type ModelProblem struct {
	Path    string
	Want    string
	Message string
}

// ModelProblems is every ModelProblem found in one check. As an error it
// wraps ErrInvalidVariant, so a load names them all and a write can
// report each at its own path.
type ModelProblems []ModelProblem

func (p ModelProblems) Error() string {
	msgs := make([]string, len(p))
	for i, m := range p {
		msgs[i] = m.Path + ": " + m.Message
	}
	return strings.Join(msgs, "; ")
}

func (p ModelProblems) Unwrap() error { return ErrInvalidVariant }

// orNil keeps an empty check from reading as a typed non-nil error.
func (p ModelProblems) orNil() error {
	if len(p) == 0 {
		return nil
	}
	return p
}

// Whether the router launches a provider's models, for checkModels.
const (
	modelsLaunched    = true
	modelsNotLaunched = false
)

// checkModels holds a provider's model entries to what the router can
// honour: variants and request defaults only where a launch reads them
// (launches), a variant named and based as docs/plan_model_templates_and_variants.md
// sets out, and no request default over a field the client owns. Every
// path that brings a tree in — load, sync, a write — runs it, so no
// caller has to know the rules. A Message never repeats its Path.
func checkModels(launches bool, modelDefaults, models map[string]ModelSpec) error {
	var problems ModelProblems
	add := func(path, want, format string, args ...any) {
		problems = append(problems, ModelProblem{Path: path, Want: want, Message: fmt.Sprintf(format, args...)})
	}
	checkRequest := func(prefix string, request map[string]any) {
		for _, field := range slices.Sorted(maps.Keys(request)) {
			if isReservedRequestField(field) {
				add(prefix+".request."+field, "a field other than "+strings.Join(reservedRequestFields, ", "),
					"request defaults cannot set %q: it carries the conversation or the routing, which pass through as the client sent them", field)
			}
		}
	}
	names := slices.Sorted(maps.Keys(models))
	variantOf := func(name string) (string, bool) {
		for _, key := range names {
			if models[key].From != "" && strings.EqualFold(key, name) {
				return key, true
			}
		}
		return "", false
	}

	for _, pattern := range slices.Sorted(maps.Keys(modelDefaults)) {
		spec := modelDefaults[pattern]
		if spec.From != "" {
			add("model_defaults."+pattern+".from", "no from",
				"a family pattern names no weights, so only a models entry can be a variant")
		}
		checkRequest("model_defaults."+pattern, spec.Request)
	}
	folded := map[string]string{}
	for _, name := range names {
		spec := models[name]
		path := "models." + name
		// A variant is found ignoring case, so two such keys would pick
		// one by map order.
		if other, ok := folded[strings.ToLower(name)]; ok && (spec.From != "" || models[other].From != "") {
			add(path, "a name no other entry spells", "differs from models.%s only in case, so a request cannot tell them apart", other)
		}
		folded[strings.ToLower(name)] = name
		if !launches {
			if spec.From != "" || len(spec.Request) > 0 {
				add(path, "parameters, environment or endpoints",
					"variants and request defaults need a provider the router launches")
			}
			continue
		}
		checkRequest(path, spec.Request)
		if spec.From == "" {
			continue
		}
		if err := ValidateVariantName(name); err != nil {
			add(path+".from", "variant name", "%v", err)
			continue
		}
		if strings.EqualFold(spec.From, name) {
			add(path+".from", "another model", "names the variant itself")
			continue
		}
		if base, ok := variantOf(spec.From); ok {
			add(path+".from", "weights, not a variant",
				"names %q, which is itself a variant; a variant runs a model's weights, one level deep", base)
			add("models."+base+".from", "a model no variant runs",
				"makes %q a variant, but variant %q runs it; a variant's base must be weights", base, name)
		}
	}
	return problems.orNil()
}

// Variant reports whether model names a variant in this provider: a
// literal models key carrying From. It returns the key as spelled and the
// base it runs. A "#file" hint (the runs API appends one) does not hide
// the variant; WeightsOf carries it to the base. An exact key decides on
// its own; only without one is the key matched ignoring case.
func (s *ServiceConfig) Variant(model string) (name, base string, ok bool) {
	model, _ = splitFileHint(model)
	if model == "" {
		return "", "", false
	}
	if spec, found := s.Models[model]; found {
		return model, spec.From, spec.From != ""
	}
	for key, spec := range s.Models {
		if spec.From != "" && strings.EqualFold(key, model) {
			return key, spec.From, true
		}
	}
	return "", "", false
}

// splitFileHint separates a "#file" hint from a model name.
func splitFileHint(model string) (name, hint string) {
	if i := strings.Index(model, "#"); i > 0 {
		return model[:i], model[i:]
	}
	return model, ""
}

// Variants maps each variant this provider defines to its base.
func (s *ServiceConfig) Variants() map[string]string {
	out := map[string]string{}
	for key, spec := range s.Models {
		if spec.From != "" {
			out[key] = spec.From
		}
	}
	return out
}

// WeightsOf names the model whose weights a launch of model loads: the
// base for a variant, the model itself otherwise. A #file hint on model
// replaces one the variant's base pins, so the result names one file.
func (s *ServiceConfig) WeightsOf(model string) string {
	if _, base, ok := s.Variant(model); ok {
		if _, hint := splitFileHint(model); hint != "" {
			repo, _ := splitFileHint(base)
			return repo + hint
		}
		return base
	}
	return model
}

// SameLaunch reports whether a and b resolve to the same process on node:
// the same parameters (after "auto" is dropped) and environment, on the
// default endpoint and on every endpoint any tier overlays. A variant
// that only adds request defaults, or repeats its base's launch values,
// is the same launch as its base and can be served by its process.
func (s *ServiceConfig) SameLaunch(node, a, b string) bool {
	for _, endpoint := range s.overlaidEndpoints() {
		ra, rb := s.ResolveEndpoint(node, a, endpoint), s.ResolveEndpoint(node, b, endpoint)
		if !maps.Equal(FilterAutoValues(FlattenParameters(ra.Parameters)), FilterAutoValues(FlattenParameters(rb.Parameters))) ||
			!maps.Equal(FlattenEnvironment(ra.Environment), FlattenEnvironment(rb.Environment)) {
			return false
		}
	}
	return true
}

// overlaidEndpoints is the default endpoint plus every endpoint an
// overlay anywhere in the tree names.
func (s *ServiceConfig) overlaidEndpoints() []string {
	set := map[string]bool{defaultEndpoint: true}
	add := func(eps map[string]EndpointOverlay) {
		for ep := range eps {
			set[ep] = true
		}
	}
	if s.Defaults != nil {
		add(s.Defaults.Endpoints)
	}
	for _, m := range s.ModelDefaults {
		add(m.Endpoints)
	}
	for _, m := range s.Models {
		add(m.Endpoints)
	}
	for _, n := range s.Nodes {
		add(n.Endpoints)
		for _, nm := range n.Models {
			add(nm.Endpoints)
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// checkVariantNames refuses a variant of provider that another provider
// also defines: a variant name selects one engine.
func (c *AppsConfig) checkVariantNames(provider string, sc ServiceConfig) error {
	var problems ModelProblems
	for _, v := range slices.Sorted(maps.Keys(sc.Variants())) {
		c.RangeApps(func(other string, osc ServiceConfig) bool {
			if other == provider || osc.Mode != constants.AppModeOnDemand {
				return true
			}
			if _, _, found := osc.Variant(v); found {
				problems = append(problems, ModelProblem{Path: "models." + v + ".from", Want: "a name no other provider uses",
					Message: fmt.Sprintf("provider %s already defines variant %q; a variant name selects one engine", other, v)})
				return false
			}
			return true
		})
	}
	return problems.orNil()
}

// LookupVariant finds the provider that defines a variant of this name.
// Only on-demand providers launch, so only they hold variants.
func (c *AppsConfig) LookupVariant(name string) (provider, base string, ok bool) {
	c.RangeApps(func(app string, sc ServiceConfig) bool {
		if sc.Mode != constants.AppModeOnDemand {
			return true
		}
		if _, b, found := sc.Variant(name); found {
			provider, base, ok = app, b, true
			return false
		}
		return true
	})
	return provider, base, ok
}
