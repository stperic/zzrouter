// Package schema provides JSON Schema validation for provider config
// bodies. ValidateOnDemand / ValidateExternal / ValidateCloud /
// ValidateRegistry each validate the BODY of a single provider against
// its kind's schema. Cross-field semantic checks (env-var resolution,
// port_range vs base_port) live in the config package's Validate()
// methods on the typed providers — schema enforces shape, Go enforces
// meaning.
package schema

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"gopkg.in/yaml.v3"
)

//go:embed on_demand.schema.json
var onDemandSchemaRaw []byte

//go:embed external.schema.json
var externalSchemaRaw []byte

//go:embed cloud.schema.json
var cloudSchemaRaw []byte

//go:embed registry.schema.json
var registrySchemaRaw []byte

type compiledSchema struct {
	once sync.Once
	s    *jsonschema.Schema
	err  error
}

var (
	onDemandCompiled compiledSchema
	externalCompiled compiledSchema
	cloudCompiled    compiledSchema
	registryCompiled compiledSchema
)

func compile(c *compiledSchema, url string, raw []byte) (*jsonschema.Schema, error) {
	c.once.Do(func() {
		comp := jsonschema.NewCompiler()
		comp.Draft = jsonschema.Draft2020
		if err := comp.AddResource(url, strings.NewReader(string(raw))); err != nil {
			c.err = fmt.Errorf("register schema resource %s: %w", url, err)
			return
		}
		s, err := comp.Compile(url)
		if err != nil {
			c.err = fmt.Errorf("compile schema %s: %w", url, err)
			return
		}
		c.s = s
	})
	return c.s, c.err
}

func onDemandSchema() (*jsonschema.Schema, error) {
	return compile(&onDemandCompiled, "on_demand.schema.json", onDemandSchemaRaw)
}

func externalSchema() (*jsonschema.Schema, error) {
	return compile(&externalCompiled, "external.schema.json", externalSchemaRaw)
}

func cloudSchema() (*jsonschema.Schema, error) {
	return compile(&cloudCompiled, "cloud.schema.json", cloudSchemaRaw)
}

func registrySchema() (*jsonschema.Schema, error) {
	return compile(&registryCompiled, "registry.schema.json", registrySchemaRaw)
}

// ValidateOnDemand validates the body of a single on-demand provider (the
// YAML under `providers.on-demand.<name>:`) against on_demand.schema.json.
func ValidateOnDemand(rawYAML []byte) error {
	return validateAgainst(rawYAML, onDemandSchema, "on-demand provider")
}

// ValidateExternal validates the body of a single external provider.
func ValidateExternal(rawYAML []byte) error {
	return validateAgainst(rawYAML, externalSchema, "external provider")
}

// ValidateCloud validates the body of a single cloud provider.
func ValidateCloud(rawYAML []byte) error {
	return validateAgainst(rawYAML, cloudSchema, "cloud provider")
}

// ValidateRegistry validates the body of a single search registry.
func ValidateRegistry(rawYAML []byte) error {
	return validateAgainst(rawYAML, registrySchema, "registry")
}

func validateAgainst(rawYAML []byte, loader func() (*jsonschema.Schema, error), label string) error {
	s, err := loader()
	if err != nil {
		return err
	}
	doc, err := yamlToJSONValue(rawYAML)
	if err != nil {
		return fmt.Errorf("parse yaml: %w", err)
	}
	if err := s.Validate(doc); err != nil {
		return fmt.Errorf("%s schema validation failed: %w", label, err)
	}
	return nil
}

// yamlToJSONValue decodes YAML into a value compatible with the JSON Schema
// validator (map[string]any / []any / primitives). yaml.v3 decodes mapping
// keys as any; convert them to string and coerce nested types.
func yamlToJSONValue(raw []byte) (any, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	var v any
	if err := node.Decode(&v); err != nil {
		return nil, err
	}
	return normalize(v)
}

func normalize(v any) (any, error) {
	// yaml.v3's Node.Decode(*any) produces map[string]any (unlike yaml.v2's
	// map[any]any), so no any-keyed map branch is needed here.
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			n, err := normalize(vv)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			n, err := normalize(vv)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	default:
		// Round-trip through json to normalize numeric types (yaml.v3 yields
		// int, the validator expects json.Number / float64 for numeric
		// constraints). Cheap at config-load time.
		b, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		var out any
		if err := dec.Decode(&out); err != nil {
			return nil, err
		}
		return out, nil
	}
}

// InstallDefinition exposes the same strict shape used for provider-file validation.
func InstallDefinition() (json.RawMessage, error) {
	var document struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(onDemandSchemaRaw, &document); err != nil {
		return nil, err
	}
	definition, ok := document.Defs["install"]
	if !ok {
		return nil, fmt.Errorf("install schema missing")
	}
	return definition, nil
}
