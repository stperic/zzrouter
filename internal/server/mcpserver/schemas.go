package mcpserver

import (
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
)

// objectSchema preserves validation while avoiding SDK boolean subschemas on the wire.
type objectSchema[T any] struct{}

func (objectSchema[T]) MarshalJSON() ([]byte, error) {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(schemaObjects(value))
}

func schemaObjects(value any) any {
	if boolean, ok := value.(bool); ok {
		if boolean {
			return map[string]any{}
		}
		return map[string]any{"not": map[string]any{}}
	}
	if schema, ok := value.(map[string]any); ok {
		for key, child := range schema {
			switch key {
			case "properties", "$defs", "definitions", "patternProperties", "dependentSchemas":
				children, _ := child.(map[string]any)
				for name, sub := range children {
					children[name] = schemaObjects(sub)
				}
			case "items", "additionalProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems", "propertyNames", "contains", "not", "if", "then", "else", "contentSchema":
				schema[key] = schemaObjects(child)
			case "allOf", "anyOf", "oneOf", "prefixItems":
				children, _ := child.([]any)
				for i, sub := range children {
					children[i] = schemaObjects(sub)
				}
			}
		}
	}
	return value
}

type runOutput struct {
	RunID       string `json:"run_id"`
	OldID       string `json:"old_id,omitempty"`
	JobID       string `json:"job_id,omitempty"`
	DeployJobID string `json:"deploy_job_id,omitempty"`
	Node        string `json:"node,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	Status      string `json:"status,omitempty"`
	Port        int    `json:"port,omitempty"`
}
type downloadOutput struct {
	DeploymentID string         `json:"deployment_id"`
	Model        string         `json:"model,omitempty"`
	Status       string         `json:"status,omitempty"`
	RestartJobID string         `json:"restart_job_id,omitempty"`
	Nodes        []downloadNode `json:"nodes,omitempty"`
}
type downloadNode struct {
	Node   string `json:"node,omitempty"`
	JobID  string `json:"job_id,omitempty"`
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}
type chatOutput struct {
	Reply string `json:"reply"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens,omitempty"`
		CompletionTokens int `json:"completion_tokens,omitempty"`
		TotalTokens      int `json:"total_tokens,omitempty"`
	} `json:"usage"`
}
