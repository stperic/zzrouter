package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"gopkg.in/yaml.v3"
)

const (
	maxOperationSchemaBytes      = 64 << 10
	maxOperationSchemaDepth      = 32
	maxOperationSchemaNodes      = 4096
	maxOperationSchemaReferences = 64
)

// OperationSchema preserves JSON numbers when schemas pass through YAML persistence.
type OperationSchema map[string]any

// UnmarshalJSON retains numeric constraints without float64 rounding.
func (s *OperationSchema) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode((*map[string]any)(s))
}

// UnmarshalYAML uses the same numeric representation as synchronized JSON definitions.
func (s *OperationSchema) UnmarshalYAML(node *yaml.Node) error {
	nodes := 0
	value, err := operationSchemaYAMLValue(node, 0, &nodes)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.UnmarshalJSON(raw)
}

func operationSchemaYAMLValue(node *yaml.Node, depth int, nodes *int) (any, error) {
	*nodes++
	if depth > maxOperationSchemaDepth || *nodes > maxOperationSchemaNodes {
		return nil, fmt.Errorf("schema nesting or node limit exceeded")
	}
	switch node.Kind {
	case yaml.MappingNode:
		out := make(map[string]any, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag != "!!str" {
				return nil, fmt.Errorf("schema object keys must be strings")
			}
			if _, duplicate := out[key.Value]; duplicate {
				return nil, fmt.Errorf("duplicate schema object key %q", key.Value)
			}
			value, err := operationSchemaYAMLValue(node.Content[i+1], depth+1, nodes)
			if err != nil {
				return nil, err
			}
			out[key.Value] = value
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, len(node.Content))
		for i, child := range node.Content {
			value, err := operationSchemaYAMLValue(child, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			out[i] = value
		}
		return out, nil
	case yaml.AliasNode:
		return operationSchemaYAMLValue(node.Alias, depth+1, nodes)
	default:
		if (node.Tag == "!!int" || node.Tag == "!!float") && json.Valid([]byte(node.Value)) {
			return json.Number(node.Value), nil
		}
		var value any
		err := node.Decode(&value)
		return value, err
	}
}

// MarshalYAML emits numeric constraints as YAML numbers instead of quoted json.Number values.
func (s OperationSchema) MarshalYAML() (any, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	return node.Content[0], nil
}

func compileOperationSchema(schema OperationSchema) (*jsonschema.Schema, error) {
	if schema == nil {
		return nil, fmt.Errorf("schema object required")
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxOperationSchemaBytes {
		return nil, fmt.Errorf("schema exceeds %d bytes", maxOperationSchemaBytes)
	}
	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil, err
	}
	if err := checkOperationSchema(normalized); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	compiler.LoadURL = func(url string) (io.ReadCloser, error) {
		return nil, fmt.Errorf("external schema resource forbidden: %s", url)
	}
	const resource = "operation.json"
	if err := compiler.AddResource(resource, bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

func checkOperationSchema(root map[string]any) error {
	nodes := 0
	if err := operationSchemaBounds(root, 0, &nodes); err != nil {
		return err
	}
	references := 0
	active := map[string]bool{}
	var walk func(any, string, int) error
	walk = func(value any, pointer string, depth int) error {
		if active[pointer] {
			return fmt.Errorf("cyclic schema reference at %s", pointer)
		}
		if depth > maxOperationSchemaDepth {
			return fmt.Errorf("schema reference nesting limit exceeded")
		}
		active[pointer] = true
		defer delete(active, pointer)
		if value, ok := value.(map[string]any); ok {
			for key, child := range value {
				switch key {
				case "$id", "$anchor", "$dynamicAnchor", "$dynamicRef", "$recursiveAnchor", "$recursiveRef", "dependencies":
					// The validator still executes legacy dependencies under draft 2020.
					// Require dependentSchemas/dependentRequired so every active schema
					// container participates in the reference and recursion bounds.
					return fmt.Errorf("schema keyword %s is unsupported", key)
				case "$schema":
					if child != "https://json-schema.org/draft/2020-12/schema" {
						return fmt.Errorf("only JSON Schema draft 2020-12 is supported")
					}
				case "$ref":
					references++
					ref, ok := child.(string)
					if !ok || !strings.HasPrefix(ref, "#/") || strings.Contains(ref, "%") || references > maxOperationSchemaReferences {
						return fmt.Errorf("only bounded local JSON Pointer references are allowed")
					}
					target, err := operationSchemaPointer(root, ref[1:])
					if err != nil {
						return err
					}
					if err := walk(target, ref[1:], depth+1); err != nil {
						return err
					}
				case "$defs", "definitions", "properties", "patternProperties", "dependentSchemas":
					children, _ := child.(map[string]any)
					for name, subschema := range children {
						if err := walk(subschema, pointer+"/"+key+"/"+operationPointerToken(name), depth+1); err != nil {
							return err
						}
					}
				case "allOf", "anyOf", "oneOf", "prefixItems":
					children, _ := child.([]any)
					for i, subschema := range children {
						if err := walk(subschema, fmt.Sprintf("%s/%s/%d", pointer, key, i), depth+1); err != nil {
							return err
						}
					}
				case "not", "if", "then", "else", "items", "contains", "additionalProperties", "unevaluatedProperties", "propertyNames", "unevaluatedItems", "contentSchema":
					if err := walk(child, pointer+"/"+key, depth+1); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return walk(root, "", 0)
}

func operationSchemaBounds(value any, depth int, nodes *int) error {
	*nodes++
	if depth > maxOperationSchemaDepth || *nodes > maxOperationSchemaNodes {
		return fmt.Errorf("schema nesting or node limit exceeded")
	}
	switch value := value.(type) {
	case map[string]any:
		for _, child := range value {
			if err := operationSchemaBounds(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := operationSchemaBounds(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

func operationPointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func operationSchemaPointer(root map[string]any, pointer string) (any, error) {
	var value any = root
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		var ok bool
		switch object := value.(type) {
		case map[string]any:
			value, ok = object[strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")]
		case []any:
			i, err := strconv.Atoi(token)
			if err == nil && i >= 0 && i < len(object) {
				value, ok = object[i], true
			}
		}
		if !ok {
			return nil, fmt.Errorf("schema reference does not exist: %s", pointer)
		}
	}
	return value, nil
}
