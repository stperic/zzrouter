package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config"
)

// ============================================================================
// Parameter Management Client API (Coordinator-Only)
// ============================================================================
//
// Client methods for the parameter editor endpoints.
// All operations execute on the coordinator — no host routing.
// The coordinator's provider config is the single source of truth for parameters.

// ParameterEntry represents a single parameter with metadata (client-side).
type ParameterEntry struct {
	Key              string   `json:"key"`
	Value            string   `json:"value"`
	Source           string   `json:"source,omitempty"`
	Label            string   `json:"label,omitempty"`
	Description      string   `json:"description,omitempty"`
	HelpText         string   `json:"help_text,omitempty"`
	InputType        string   `json:"input_type,omitempty"`
	SupportsAuto     bool     `json:"supports_auto,omitempty"`
	AutoResolvedHint string   `json:"auto_resolved_hint,omitempty"`
	Options          []string `json:"options,omitempty"`
	Ignored          bool     `json:"ignored"`
	IsOverride       bool     `json:"is_override,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

// ProviderParametersResponse is the response for GET /providers/:name/parameters.
type ProviderParametersResponse struct {
	Provider    string           `json:"provider"`
	Parameters  []ParameterEntry `json:"parameters"`
	Environment []ParameterEntry `json:"environment"`
}

// GetProviderParameters retrieves provider-level parameters with schema metadata.
// Set resolve=true to populate auto_resolved_hint for "auto" and "${VAR}" values.
func (c *Client) GetProviderParameters(provider string, resolve bool) (*ProviderParametersResponse, error) {
	endpoint := apipath.ProviderParameters(provider)
	if resolve {
		endpoint += "?resolve=true"
	}
	var result ProviderParametersResponse
	if err := c.doJSON("GET", endpoint, nil, &result, "get provider parameters"); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetCoordinatorNodeNameContext fetches the coordinator's node name from the nodes API.
// Returns the name of the first node with role "coordinator" or "standalone".
func (c *Client) GetCoordinatorNodeNameContext(ctx context.Context) (string, error) {
	resp, err := c.GetNodesWithRefreshContext(ctx, "name,cluster_role", false)
	if err != nil {
		return "", fmt.Errorf("failed to get nodes: %w", err)
	}

	nodes, ok := resp["data"].([]any)
	if !ok || len(nodes) == 0 {
		return "", fmt.Errorf("no nodes found")
	}

	for _, n := range nodes {
		node, ok := n.(map[string]any)
		if !ok {
			continue
		}
		role, _ := node["cluster_role"].(string)
		if role == string(config.ClusterModeCoordinator) || role == string(config.ClusterModeStandalone) {
			if name, ok := node["name"].(string); ok && name != "" {
				return name, nil
			}
		}
	}

	return "", fmt.Errorf("could not determine coordinator node name")
}

const mergePatchContentType = "application/merge-patch+json"

// sendMergePatch uses the same transport and response handling as other API calls.
func (c *Client) sendMergePatch(ctx context.Context, provider string, query url.Values, patch, target any, operation string) error {
	resp, err := c.makeContextRequest(ctx, http.MethodPatch, withQuery(apipath.ProviderParameters(provider), query), patch,
		map[string]string{"Content-Type": mergePatchContentType})
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return readResponse(resp, target, operation)
}

// ParameterSchema describes one key in a provider's merged schema.
type ParameterSchema struct {
	Type        string   `json:"type"`
	Auto        string   `json:"auto,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Description string   `json:"description,omitempty"`
	// Pass is how an asset-typed value reaches the engine: path or content.
	Pass string `json:"pass,omitempty"`
}

// ProviderSchemaResponse is GET /providers/:name/schema.
type ProviderSchemaResponse struct {
	Parameters  map[string]ParameterSchema `json:"parameters"`
	Environment map[string]ParameterSchema `json:"environment"`
}

// GetProviderSchema fetches the merged parameter schema.
func (c *Client) GetProviderSchema(provider string) (*ProviderSchemaResponse, error) {
	return c.GetProviderSchemaContext(context.Background(), provider)
}

// GetProviderSchemaContext reads schema within the caller's lifetime.
func (c *Client) GetProviderSchemaContext(ctx context.Context, provider string) (*ProviderSchemaResponse, error) {
	var out ProviderSchemaResponse
	if err := c.doContextJSON(ctx, "GET", apipath.ProviderSchema(provider), nil, &out, "get provider schema"); err != nil {
		return nil, err
	}
	return &out, nil
}

// typeValues converts the editor's strings into the JSON types the
// Merge-Patch validator requires.
//
// Every value arrives as a string because it came from a text input, but
// the validator type-checks against the schema: sending "6" for an int
// key is rejected with wrong_type. Storage is still strings; it is the
// patch body that has to be typed.
//
// A value that will not parse is left as a string on purpose, so the
// server produces the error. One validator, one set of messages: a
// client that pre-judged would eventually disagree with it.
func typeValues(schema map[string]ParameterSchema, vals map[string]string) map[string]any {
	out := make(map[string]any, len(vals))
	for k, v := range vals {
		spec, ok := schema[k]
		if !ok || (spec.Auto != "" && v == spec.Auto) {
			out[k] = v
			continue
		}
		switch spec.Type {
		case "int":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				out[k] = n
				continue
			}
		case "float", "number":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				out[k] = f
				continue
			}
		case "bool":
			if b, err := strconv.ParseBool(v); err == nil {
				out[k] = b
				continue
			}
		}
		out[k] = v
	}
	return out
}
