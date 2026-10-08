package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// ModelRegistryResponse represents the response from the /zzrouter/v1/models endpoint
type ModelRegistryResponse struct {
	Data    []ModelMetadata `json:"data"`
	Total   int             `json:"total"`
	HasMore bool            `json:"has_more"`
}

// ModelMetadata represents a model from the registry in Ollama-compatible format
type ModelMetadata struct {
	// Ollama-compatible fields (primary)
	Name       string         `json:"name"`
	Model      string         `json:"model"`       // Ollama duplicate of name
	ModifiedAt time.Time      `json:"modified_at"` // Ollama format
	Size       int64          `json:"size"`
	Digest     string         `json:"digest"`
	Details    map[string]any `json:"details"` // Ollama details object

	// zzRouter-specific fields (all optional)
	Node         string    `json:"node"`
	IsCloud      bool      `json:"is_cloud,omitempty"`
	FullPath     string    `json:"full_path,omitempty"`
	Format       string    `json:"format,omitempty"`       // Also in details.format
	Quantization string    `json:"quantization,omitempty"` // Also in details.quantization_level
	SourceRepo   string    `json:"source_repo,omitempty"`  // Where it came from (ollama, huggingface)
	SourceID     string    `json:"source_id,omitempty"`
	AssignedApp  string    `json:"assigned_app,omitempty"` // What will run it (mlx, llama.cpp, vllm, test)
	Modified     time.Time `json:"modified"`
	AddedAt      time.Time `json:"added_at"`
}

// GetModifiedTime returns the modification time, preferring modified_at over modified
func (m *ModelMetadata) GetModifiedTime() time.Time {
	if !m.ModifiedAt.IsZero() {
		return m.ModifiedAt
	}
	return m.Modified
}

// GetFormat returns the format, checking details first then falling back to Format field
func (m *ModelMetadata) GetFormat() string {
	if m.Details != nil {
		if format, ok := m.Details["format"].(string); ok && format != "" {
			return format
		}
	}
	return m.Format
}

// GetQuantization returns the quantization level from details or direct field
func (m *ModelMetadata) GetQuantization() string {
	if m.Details != nil {
		if quant, ok := m.Details["quantization_level"].(string); ok && quant != "" {
			return quant
		}
	}
	return m.Quantization
}

// ModelFilter selects models from the catalog. An empty field, or "*",
// matches every value.
type ModelFilter struct {
	Node     string
	Registry string // where the weights came from
	Provider string // the engine that runs the model
	Model    string // a name or glob
	Refresh  bool   // rescan every node before answering
}

// ListModels returns the catalog's models that match f.
func (c *Client) ListModels(f ModelFilter) ([]ModelMetadata, error) {
	q := url.Values{}
	for key, value := range map[string]string{"node": f.Node, "registry": f.Registry, "provider": f.Provider, "model": f.Model} {
		if value != "" && value != "*" {
			q.Set(key, value)
		}
	}
	if f.Refresh {
		q.Set("refresh", "true")
	}
	path := apipath.Models
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var result ModelRegistryResponse
	if err := c.doJSON("GET", path, nil, &result, "list models from registry"); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// DeleteRegistryResponse represents the response from delete models endpoint
type DeleteRegistryResponse struct {
	Deleted int      `json:"deleted"`
	Errors  []string `json:"errors"`
}

// DeleteModelsFromRegistry deletes models from the registry
// For Ollama models, it calls Ollama API. For file-based models, it deletes files.
func (c *Client) DeleteModelsFromRegistry(models []ModelMetadata) (*DeleteRegistryResponse, error) {
	// Convert to the format expected by the server
	type ModelToDelete struct {
		Name     string `json:"name"`
		Registry string `json:"registry"` // Matches list API parameter
		Node     string `json:"node"`     // Which cluster node the model is on
	}

	modelsToDelete := make([]ModelToDelete, len(models))
	for i, m := range models {
		// Use SourceRepo if available, otherwise infer from format
		repo := m.SourceRepo
		if repo == "" {
			// Infer from format - GGUF files are typically from HuggingFace
			format := m.GetFormat()
			if format == "gguf" {
				repo = "huggingface"
			}
			// For other formats, leave empty - server will handle it
		}
		modelsToDelete[i] = ModelToDelete{
			Name:     m.Name,
			Registry: repo,
			Node:     m.Node,
		}
	}

	body := map[string]any{
		"models": modelsToDelete,
	}

	var envelope struct {
		Data struct {
			Deleted int      `json:"deleted"`
			Errors  []string `json:"errors"`
		} `json:"data"`
	}
	if err := c.doJSON("DELETE", apipath.Models, body, &envelope, "delete models from registry"); err != nil {
		return nil, err
	}
	return &DeleteRegistryResponse{
		Deleted: envelope.Data.Deleted,
		Errors:  envelope.Data.Errors,
	}, nil
}

// LoadModelResponse represents the response from loading a model
type LoadModelResponse struct {
	InstanceID string `json:"instance_id"`
	Model      string `json:"model"`
	App        string `json:"provider"`
	Port       int    `json:"port"`
	Status     string `json:"status"`
	Node       string `json:"node"`
	// JobID is populated on async on-demand launches (202). Empty when
	// the provider completed synchronously (e.g. endpoint/cloud).
	JobID string `json:"job_id,omitempty"`
}

// LoadModelWithProviderForceParamsAndEnv loads a model with optional force flag, custom parameters, and environment variables
// Returns the instance ID so the caller can use it directly (e.g., for streaming logs)
func (c *Client) LoadModelWithProviderForceParamsAndEnv(host, modelName, provider string, force bool, parameters, environment map[string]string) (*LoadModelResponse, error) {
	req := LoadModelRequest{
		Node:        host,
		App:         provider,
		ModelName:   modelName,
		Force:       force,
		Parameters:  parameters,
		Environment: environment,
	}
	var envelope struct {
		Data LoadModelResponse `json:"data"`
	}
	if err := c.doJSON("POST", apipath.RunsLoad, req, &envelope, "load model"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// PreviewRun gets a preview of what a run would look like without executing it
func (c *Client) PreviewRun(node, modelName, provider string, parameters, environment map[string]string) (*RunPreview, error) {
	req := struct {
		Node        string            `json:"node,omitempty"`
		App         string            `json:"provider,omitempty"`
		ModelName   string            `json:"model_name"`
		Parameters  map[string]string `json:"parameters,omitempty"`
		Environment map[string]string `json:"environment,omitempty"`
	}{
		Node:        node,
		App:         provider,
		ModelName:   modelName,
		Parameters:  parameters,
		Environment: environment,
	}
	var envelope struct {
		Data RunPreview `json:"data"`
	}
	if err := c.doJSON("POST", apipath.RunsPreview, req, &envelope, "preview run"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// SaveModelDefaults saves model parameters and environment as defaults in provider config
func (c *Client) SaveModelDefaults(node, modelName, provider string, parameters, environment map[string]string) error {
	req := struct {
		Node        string            `json:"node,omitempty"`
		App         string            `json:"provider"`
		ModelName   string            `json:"model_name"`
		Parameters  map[string]string `json:"parameters,omitempty"`
		Environment map[string]string `json:"environment,omitempty"`
	}{
		Node:        node,
		App:         provider,
		ModelName:   modelName,
		Parameters:  parameters,
		Environment: environment,
	}
	return c.doJSON("POST", apipath.ModelsDefaults, req, nil, "save model defaults")
}

// ListInstances lists all running provider instances. Uses
// context.Background() — prefer ListInstancesCtx in new code so
// caller cancellation and deadlines propagate.
func (c *Client) ListInstances() ([]Instance, error) {
	return c.ListInstancesCtx(context.Background())
}

// ListInstancesCtx is the context-aware form of ListInstances.
// Kept custom because doJSON uses context.Background() via makeRequest.
func (c *Client) ListInstancesCtx(ctx context.Context) ([]Instance, error) {
	resp, err := c.makeContextRequest(ctx, "GET", apipath.Runs, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list instances failed with status %d: %s", resp.StatusCode, parseErrorResponse(body))
	}

	var result ListInstancesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("list instances: decode response: %w", err)
	}
	return result.Data, nil
}

// StopInstance stops a running provider instance on the specified host
func (c *Client) StopInstance(instanceID, host string) error {
	endpoint := apipath.Run(instanceID)
	if host != "" {
		endpoint += fmt.Sprintf("?node=%s", url.QueryEscape(host))
	}
	return c.doJSON("DELETE", endpoint, nil, nil, "stop instance")
}

// ShowModel gets detailed model information including capabilities, metadata, etc.
// V2: Uses new /zzrouter/models/show endpoint (clean architecture)
func (c *Client) ShowModel(modelName string, verbose bool, host string, app string) (map[string]any, error) {
	verboseParam := "false"
	if verbose {
		verboseParam = "true"
	}
	path := fmt.Sprintf(apipath.ModelsShow+"?model=%s&verbose=%s",
		url.QueryEscape(modelName), verboseParam)
	if host != "" {
		path = fmt.Sprintf("%s&node=%s", path, url.QueryEscape(host))
	}
	if app != "" {
		path = fmt.Sprintf("%s&provider=%s", path, url.QueryEscape(app))
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := c.doJSON("GET", path, nil, &envelope, "show model"); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}
