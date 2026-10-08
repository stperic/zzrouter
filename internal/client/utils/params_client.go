package client

import (
	"errors"
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

// ModelParametersResponse is the response for GET /providers/:name/models/parameters.
type ModelParametersResponse struct {
	Provider    string           `json:"provider"`
	Model       string           `json:"model"`
	Parameters  []ParameterEntry `json:"parameters"`
	Environment []ParameterEntry `json:"environment"`
}

// ValidationWarning represents a non-blocking validation warning.
type ValidationWarning struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// UpdateParametersResponse is the response for PUT parameter endpoints.
type UpdateParametersResponse struct {
	Provider    string              `json:"provider"`
	Model       string              `json:"model,omitempty"`
	Parameters  []ParameterEntry    `json:"parameters"`
	Environment []ParameterEntry    `json:"environment"`
	Warnings    []ValidationWarning `json:"warnings,omitempty"`
	Saved       bool                `json:"saved"`
}

// UpdateParametersRequest is the request body for PUT parameter endpoints.
type UpdateParametersRequest struct {
	Parameters  map[string]string `json:"parameters,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
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

// UpdateProviderParameters updates provider-level parameters.
//
// dryRun has no server-side equivalent: Merge-Patch is the single
// mutator and it has no preview mode. Returning an error is deliberate.
// Silently saving when the caller asked to validate would be worse than
// the 410 this replaced, because it would look like it worked.
func (c *Client) UpdateProviderParameters(provider string, req *UpdateParametersRequest, dryRun bool) (*UpdateParametersResponse, error) {
	if dryRun {
		return nil, ErrValidateWithoutSaving
	}
	if err := c.patchParameters(provider, tierPatch("defaults", "", c.typedRequest(provider, req)), "update provider parameters"); err != nil {
		return nil, err
	}
	return c.providerParametersAfterPatch(provider)
}

// GetModelParameters retrieves model-level parameters with override tracking.
// Set resolve=true to populate auto_resolved_hint for "auto" values using local hardware.
func (c *Client) GetModelParameters(provider, model string, resolve bool) (*ModelParametersResponse, error) {
	params := url.Values{}
	params.Set("model", model)
	if resolve {
		params.Set("resolve", "true")
	}
	endpoint := apipath.ProviderModelsParameters(provider) + "?" + params.Encode()
	var result ModelParametersResponse
	if err := c.doJSON("GET", endpoint, nil, &result, "get model parameters"); err != nil {
		return nil, err
	}
	return &result, nil
}

// UpdateModelParameters updates model-level parameter overrides.
// See UpdateProviderParameters on dryRun.
func (c *Client) UpdateModelParameters(provider, model string, req *UpdateParametersRequest, dryRun bool) (*UpdateParametersResponse, error) {
	if dryRun {
		return nil, ErrValidateWithoutSaving
	}
	if err := c.patchParameters(provider, tierPatch("models", model, c.typedRequest(provider, req)), "update model parameters"); err != nil {
		return nil, err
	}
	out, err := c.GetModelParameters(provider, model, false)
	if err != nil {
		return nil, err
	}
	return &UpdateParametersResponse{
		Provider: out.Provider, Model: out.Model,
		Parameters: out.Parameters, Environment: out.Environment, Saved: true,
	}, nil
}

// NodeParametersResponse is the response for GET /providers/:name/nodes/parameters.
type NodeParametersResponse struct {
	Provider    string           `json:"provider"`
	Node        string           `json:"node"`
	Parameters  []ParameterEntry `json:"parameters"`
	Environment []ParameterEntry `json:"environment"`
}

// GetNodeParameters retrieves node-level parameters with override tracking.
// Set resolve=true to populate auto_resolved_hint for "auto" and "${VAR}" values.
func (c *Client) GetNodeParameters(provider, node string, resolve bool) (*NodeParametersResponse, error) {
	params := url.Values{}
	params.Set("node", node)
	if resolve {
		params.Set("resolve", "true")
	}
	endpoint := apipath.ProviderNodesParameters(provider) + "?" + params.Encode()
	var result NodeParametersResponse
	if err := c.doJSON("GET", endpoint, nil, &result, "get node parameters"); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetCoordinatorNodeName fetches the coordinator's node name from the nodes API.
// Returns the name of the first node with role "coordinator" or "standalone".
func (c *Client) GetCoordinatorNodeName() (string, error) {
	resp, err := c.GetNodesWithRefresh("name,cluster_role", false)
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

	if first, ok := nodes[0].(map[string]any); ok {
		if name, ok := first["name"].(string); ok && name != "" {
			return name, nil
		}
	}

	return "", fmt.Errorf("could not determine coordinator node name")
}

// UpdateNodeParameters updates node-level parameter overrides.
// See UpdateProviderParameters on dryRun.
func (c *Client) UpdateNodeParameters(provider, node string, req *UpdateParametersRequest, dryRun bool) (*UpdateParametersResponse, error) {
	if dryRun {
		return nil, ErrValidateWithoutSaving
	}
	if err := c.patchParameters(provider, tierPatch("nodes", node, c.typedRequest(provider, req)), "update node parameters"); err != nil {
		return nil, err
	}
	out, err := c.GetNodeParameters(provider, node, false)
	if err != nil {
		return nil, err
	}
	return &UpdateParametersResponse{
		Provider: out.Provider, Parameters: out.Parameters,
		Environment: out.Environment, Saved: true,
	}, nil
}

// DeleteNodeParameter deletes a single node-level parameter override.
func (c *Client) DeleteNodeParameter(provider, node, key string) error {
	return c.patchParameters(provider, tierDelete("nodes", node, sectionParameters, key), "delete node parameter")
}

// DeleteNodeEnvironment deletes a single node-level environment variable override.
func (c *Client) DeleteNodeEnvironment(provider, node, key string) error {
	return c.patchParameters(provider, tierDelete("nodes", node, sectionEnvironment, key), "delete node environment")
}

// DeleteProviderParameter deletes a single provider-level parameter.
func (c *Client) DeleteProviderParameter(provider, key string) error {
	return c.patchParameters(provider, tierDelete("defaults", "", sectionParameters, key), "delete provider parameter")
}

// DeleteProviderEnvironment deletes a single provider-level environment variable.
func (c *Client) DeleteProviderEnvironment(provider, key string) error {
	return c.patchParameters(provider, tierDelete("defaults", "", sectionEnvironment, key), "delete provider environment")
}

// DeleteModelParameter deletes a single model-level parameter override.
func (c *Client) DeleteModelParameter(provider, model, key string) error {
	return c.patchParameters(provider, tierDelete("models", model, sectionParameters, key), "delete model parameter")
}

// DeleteModelEnvironment deletes a single model-level environment variable override.
func (c *Client) DeleteModelEnvironment(provider, model, key string) error {
	return c.patchParameters(provider, tierDelete("models", model, sectionEnvironment, key), "delete model environment")
}

// GetServiceStatus returns the service management status for a provider.
// Includes: detected manager, running state, pending restart flag, running models.
func (c *Client) GetServiceStatus(provider string) (map[string]any, error) {
	endpoint := apipath.ProviderServiceStatus(provider)
	var result map[string]any
	if err := c.doJSON("GET", endpoint, nil, &result, "get service status"); err != nil {
		return nil, err
	}
	return result, nil
}

// ApplyServiceConfig applies env vars via the detected service manager and restarts.
// Returns the result including manual instructions on permission failure.
func (c *Client) ApplyServiceConfig(provider string) (map[string]any, error) {
	endpoint := apipath.ProviderServiceApply(provider)
	var result map[string]any
	if err := c.doJSON("POST", endpoint, nil, &result, "apply service config"); err != nil {
		return nil, err
	}
	return result, nil
}

// ----------------------------------------------------------------------------
// Merge-Patch plumbing
// ----------------------------------------------------------------------------
//
// PATCH /providers/:name/parameters (RFC 7396) is the single mutator.
// The PUT and DELETE routes this replaced answer 410 Gone, which is what
// every parameter write in the TUI was hitting.

const (
	mergePatchContentType = "application/merge-patch+json"

	sectionParameters  = "parameters"
	sectionEnvironment = "environment"
)

// ErrValidateWithoutSaving reports that validate-without-saving is not
// available. Merge-Patch has no preview mode, so a caller asking to
// validate has to be told, not quietly obeyed with a save.
var ErrValidateWithoutSaving = errors.New(
	"validating without saving is not supported: the server applies parameter changes atomically")

// tierPatch wraps an update in the Merge-Patch envelope for one tier.
// scope is the tier key ("defaults", "models", "nodes"); name is the
// model or node it applies to, empty for defaults.
func tierPatch(scope, name string, req *typedUpdate) map[string]any {
	body := map[string]any{}
	if req != nil {
		if len(req.Parameters) > 0 {
			body[sectionParameters] = req.Parameters
		}
		if len(req.Environment) > 0 {
			body[sectionEnvironment] = req.Environment
		}
	}
	if name == "" {
		return map[string]any{scope: body}
	}
	return map[string]any{scope: map[string]any{name: body}}
}

// tierDelete builds the patch that removes one key at one tier. A null
// value is Merge-Patch's delete; the server never persists it, so after
// the patch the key is either present with a value or absent.
func tierDelete(scope, name, section, key string) map[string]any {
	inner := map[string]any{section: map[string]any{key: nil}}
	if name == "" {
		return map[string]any{scope: inner}
	}
	return map[string]any{scope: map[string]any{name: inner}}
}

// patchParameters sends one Merge-Patch and discards the answer.
func (c *Client) patchParameters(provider string, patch map[string]any, operation string) error {
	return c.sendMergePatch(provider, nil, patch, nil, operation)
}

// sendMergePatch sends one Merge-Patch to the provider's parameters and
// decodes the answer into target when there is one. The content type is
// the contract, not decoration: the server routes on it.
func (c *Client) sendMergePatch(provider string, query url.Values, patch, target any, operation string) error {
	resp, err := c.makeRequestWithHeaders(http.MethodPatch, withQuery(apipath.ProviderParameters(provider), query), patch,
		map[string]string{"Content-Type": mergePatchContentType})
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return readResponse(resp, target, operation)
}

// providerParametersAfterPatch re-reads the provider tier so callers get
// entries carrying schema metadata. The patch response is a resolved
// value map with no labels or input types, which is not what the editor
// renders.
func (c *Client) providerParametersAfterPatch(provider string) (*UpdateParametersResponse, error) {
	out, err := c.GetProviderParameters(provider, false)
	if err != nil {
		return nil, err
	}
	return &UpdateParametersResponse{
		Provider:    out.Provider,
		Parameters:  out.Parameters,
		Environment: out.Environment,
		Saved:       true,
	}, nil
}

// ParameterSchema describes one key in a provider's merged schema.
type ParameterSchema struct {
	Type        string   `json:"type"`
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
	var out ProviderSchemaResponse
	if err := c.doJSON("GET", apipath.ProviderSchema(provider), nil, &out, "get provider schema"); err != nil {
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
		if !ok {
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

// typedRequest resolves req's values against the provider's schema.
// A schema fetch that fails is not fatal: the untyped body still reaches
// the validator, which will say what is wrong more precisely than a
// swallowed fetch error would.
func (c *Client) typedRequest(provider string, req *UpdateParametersRequest) *typedUpdate {
	out := &typedUpdate{}
	if req == nil {
		return out
	}
	schema, err := c.GetProviderSchema(provider)
	if err != nil || schema == nil {
		schema = &ProviderSchemaResponse{}
	}
	if len(req.Parameters) > 0 {
		out.Parameters = typeValues(schema.Parameters, req.Parameters)
	}
	if len(req.Environment) > 0 {
		out.Environment = typeValues(schema.Environment, req.Environment)
	}
	return out
}

// typedUpdate is UpdateParametersRequest after schema coercion.
type typedUpdate struct {
	Parameters  map[string]any
	Environment map[string]any
}
