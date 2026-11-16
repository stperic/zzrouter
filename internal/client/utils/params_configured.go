package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// ParameterScope selects a configured cell, independently of effective values.
type ParameterScope struct {
	Scope    string `json:"scope"`
	Node     string `json:"node,omitempty"`
	Model    string `json:"model,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

func (s ParameterScope) query() url.Values {
	q := url.Values{"scope": {s.Scope}}
	if s.Node != "" {
		q.Set("node", s.Node)
	}
	if s.Model != "" {
		q.Set("model", s.Model)
	}
	if s.Endpoint != "" {
		q.Set("endpoint", s.Endpoint)
	}
	return q
}

// ConfiguredParameters contains only values explicitly set at the selected scope.
type ConfiguredParameters struct {
	ParameterScope
	Parameters  map[string]string `json:"parameters"`
	Environment map[string]string `json:"environment"`
}

// ParameterChanges holds edited leaves. A nil value removes the override.
type ParameterChanges struct {
	Parameters  map[string]*string
	Environment map[string]*string
}

// GetConfiguredParameters retrieves an editing projection without inheritance.
func (c *Client) GetConfiguredParameters(ctx context.Context, provider string, scope ParameterScope) (*ConfiguredParameters, error) {
	var out ConfiguredParameters
	err := c.doContextJSON(ctx, http.MethodGet, withQuery(apipath.ProviderParameters(provider), scope.query()), nil, &out, "get configured parameters")
	return &out, err
}

// PatchConfiguredParameters changes only supplied leaves; the API validates them.
// The response belongs to this write, without a fallible follow-up GET.
func (c *Client) PatchConfiguredParameters(ctx context.Context, provider string, scope ParameterScope, changes ParameterChanges, schema *ProviderSchemaResponse) error {
	leaf := map[string]any{}
	if len(changes.Parameters) > 0 {
		leaf["parameters"] = typedChanges(schema.Parameters, changes.Parameters)
	}
	if len(changes.Environment) > 0 {
		leaf["environment"] = typedChanges(schema.Environment, changes.Environment)
	}
	if scope.Endpoint != "" {
		leaf = map[string]any{"endpoints": map[string]any{scope.Endpoint: leaf}}
	}
	var patch map[string]any
	switch scope.Scope {
	case "defaults":
		patch = map[string]any{"defaults": leaf}
	case "model":
		patch = map[string]any{"models": map[string]any{scope.Model: leaf}}
	case "node":
		patch = map[string]any{"nodes": map[string]any{scope.Node: leaf}}
	case "node-model":
		patch = map[string]any{"nodes": map[string]any{scope.Node: map[string]any{"models": map[string]any{scope.Model: leaf}}}}
	default:
		return fmt.Errorf("unknown parameter scope %q", scope.Scope)
	}
	return c.sendMergePatch(ctx, provider, nil, patch, nil, "save configured parameters")
}

func typedChanges(schema map[string]ParameterSchema, changes map[string]*string) map[string]any {
	values := make(map[string]string, len(changes))
	for key, value := range changes {
		if value != nil {
			values[key] = *value
		}
	}
	out := typeValues(schema, values)
	for key, value := range changes {
		if value == nil {
			out[key] = nil
		}
	}
	return out
}
