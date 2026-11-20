package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

const (
	maxOperations = 64
)

// OperationDefinition selects implemented execution semantics for a native diagnostic.
type OperationDefinition struct {
	Version      int              `yaml:"version" json:"version"`
	Description  string           `yaml:"description" json:"description"`
	Adapter      string           `yaml:"adapter" json:"adapter"`
	Audience     string           `yaml:"audience" json:"audience"`
	Effect       string           `yaml:"effect" json:"effect"`
	TargetScope  string           `yaml:"target_scope" json:"target_scope"`
	Transport    string           `yaml:"transport" json:"transport"`
	Binding      OperationBinding `yaml:"binding" json:"binding"`
	InputSchema  OperationSchema  `yaml:"input_schema" json:"input_schema"`
	OutputSchema OperationSchema  `yaml:"output_schema" json:"output_schema"`
	Runtimes     []string         `yaml:"runtimes" json:"runtimes"`
}

// OperationBinding is a fixed engine-relative JSON request, never caller routing input.
type OperationBinding struct {
	Method string `yaml:"method" json:"method"`
	Path   string `yaml:"path" json:"path"`
}

// CompiledOperation belongs to one validated provider snapshot and exposes no mutable schemas.
type CompiledOperation struct {
	id, provider, key, revision string
	definition                  OperationDefinition
	input, output               *jsonschema.Schema
}

// ID returns the provider-qualified operation identity.
func (o *CompiledOperation) ID() string { return o.id }

// Provider returns the configuration's provider identity.
func (o *CompiledOperation) Provider() string { return o.provider }

// Key returns the operation's provider-local identity.
func (o *CompiledOperation) Key() string { return o.key }

// Revision returns the deterministic descriptor digest checked before dispatch.
func (o *CompiledOperation) Revision() string { return o.revision }

// Definition returns a detached descriptor safe for callers to inspect or serialize.
func (o *CompiledOperation) Definition() OperationDefinition { return cloneOperation(o.definition) }

// ValidateInput validates nested input using this snapshot's compiled schema.
func (o *CompiledOperation) ValidateInput(input any) error { return o.input.Validate(input) }

// ValidateOutput validates the actual upstream result using this snapshot's compiled schema.
func (o *CompiledOperation) ValidateOutput(output any) error { return o.output.Validate(output) }

// AppliesToRuntime tests explicit applicability; an empty runtime identifies the base engine.
func (o *CompiledOperation) AppliesToRuntime(runtime string) bool {
	return MatchesRuntime(o.definition.Runtimes, o.provider, runtime)
}

// MatchesRuntime matches a selector against a launch's effective runtime.
func MatchesRuntime(runtimes []string, provider, runtime string) bool {
	if runtime == "" || runtime == provider {
		runtime = "base"
	}
	return slices.Contains(runtimes, runtime)
}

// Operation returns an immutable operation from this configuration snapshot.
func (c *AppsConfig) Operation(id string) (*CompiledOperation, bool) {
	i := strings.LastIndexByte(id, '.')
	if i < 1 {
		return nil, false
	}
	p, ok := c.Find(id[:i]).(*OnDemandProvider)
	if !ok {
		return nil, false
	}
	o, ok := p.compiledOperations[id[i+1:]]
	return o, ok
}

// RangeOperations visits the snapshot's operations in stable provider and key order.
func (c *AppsConfig) RangeOperations(visit func(*CompiledOperation) bool) {
	for _, provider := range slices.Sorted(slices.Values(c.AppNames())) {
		p, ok := c.Find(provider).(*OnDemandProvider)
		if !ok {
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(p.compiledOperations)) {
			if !visit(p.compiledOperations[key]) {
				return
			}
		}
	}
}

var operationKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var operationProviderPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func (p *OnDemandProvider) compileOperations() error {
	if len(p.Operations) == 0 {
		p.compiledOperations = nil
		return nil
	}
	if len(p.Operations) > maxOperations {
		return fmt.Errorf("provider %q: at most %d operations allowed", p.Name, maxOperations)
	}
	compiled := make(map[string]*CompiledOperation, len(p.Operations))
	for key, def := range p.Operations {
		if !operationProviderPattern.MatchString(p.Name) || !operationKeyPattern.MatchString(key) {
			return fmt.Errorf("provider %q: invalid operation identity %q", p.Name, key)
		}
		o, err := p.compileOperation(key, def)
		if err != nil {
			return fmt.Errorf("provider %q operation %q: %w", p.Name, key, err)
		}
		compiled[key] = o
	}
	definitions := make(map[string]OperationDefinition, len(compiled))
	for key, operation := range compiled {
		definitions[key] = operation.Definition()
	}
	p.Operations = definitions
	p.compiledOperations = compiled
	return nil
}

func (p *OnDemandProvider) compileOperation(key string, def OperationDefinition) (*CompiledOperation, error) {
	if def.Version != 1 || def.Adapter != "run_json_read" || def.Audience != "admin" || def.Effect != "read" || def.TargetScope != "run" || def.Transport != "json" {
		return nil, fmt.Errorf("requires version 1 and implemented admin/read/run/json adapter run_json_read")
	}
	if strings.TrimSpace(def.Description) == "" || len(def.Description) > 1024 || strings.ContainsAny(def.Description, "\r\n\x00") {
		return nil, fmt.Errorf("description requires a bounded single line")
	}
	if (def.Binding.Method != "GET" && def.Binding.Method != "POST") || !validOperationPath(def.Binding.Path) {
		return nil, fmt.Errorf("binding requires GET or POST and a fixed bounded relative path")
	}
	if err := ValidateRuntimeSelectors(def.Runtimes, p.Features); err != nil {
		return nil, err
	}
	input, err := compileOperationSchema(def.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("input_schema: %w", err)
	}
	output, err := compileOperationSchema(def.OutputSchema)
	if err != nil {
		return nil, fmt.Errorf("output_schema: %w", err)
	}
	id := p.Name + "." + key
	raw, err := json.Marshal(struct {
		ID         string              `json:"id"`
		Definition OperationDefinition `json:"definition"`
	}{id, def})
	if err != nil {
		return nil, err
	}
	var frozen struct {
		ID         string              `json:"id"`
		Definition OperationDefinition `json:"definition"`
	}
	if err := json.Unmarshal(raw, &frozen); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	return &CompiledOperation{id: id, provider: p.Name, key: key, revision: fmt.Sprintf("sha256:%x", digest), definition: frozen.Definition, input: input, output: output}, nil
}

// ValidateRuntimeSelectors checks an explicit selection against configured feature runtimes.
func ValidateRuntimeSelectors(runtimes []string, features map[string]Feature) error {
	if len(runtimes) == 0 || len(runtimes) > 16 {
		return fmt.Errorf("explicit runtime applicability required")
	}
	for index, runtime := range runtimes {
		known := runtime == "base"
		for _, feature := range features {
			known = known || feature.Runtime != "" && runtime == feature.Runtime
		}
		if !known || slices.Contains(runtimes[:index], runtime) || len(runtime) > 128 {
			return fmt.Errorf("unknown or duplicate runtime %q", runtime)
		}
	}
	return nil
}

func validOperationPath(value string) bool {
	if len(value) == 0 || len(value) > 256 || !ValidHTTPPath(value) || path.Clean(value) != value || strings.ContainsAny(value, "%{}:$ \t\r\n\x00") {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return !strings.Contains(value, "//")
}

func cloneOperation(def OperationDefinition) OperationDefinition {
	def.Runtimes = slices.Clone(def.Runtimes)
	def.InputSchema, _ = cloneOperationValue(map[string]any(def.InputSchema)).(map[string]any)
	def.OutputSchema, _ = cloneOperationValue(map[string]any(def.OutputSchema)).(map[string]any)
	return def
}

func cloneOperations(operations map[string]OperationDefinition) map[string]OperationDefinition {
	if operations == nil {
		return nil
	}
	out := make(map[string]OperationDefinition, len(operations))
	for key, def := range operations {
		out[key] = cloneOperation(def)
	}
	return out
}

func cloneOperationValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		if value == nil {
			return map[string]any(nil)
		}
		out := make(map[string]any, len(value))
		for key, child := range value {
			out[key] = cloneOperationValue(child)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = cloneOperationValue(child)
		}
		return out
	default:
		return value
	}
}
