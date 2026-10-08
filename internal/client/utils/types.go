package client

import (
	"fmt"

	"github.com/stperic/zzrouter/pkg/protocol/openai"
)

// FilteredModelsResponse represents the response from /admin/models?filter=...
type FilteredModelsResponse struct {
	NodeName string                   `json:"host_name"`
	NodeAddr string                   `json:"host_address,omitempty"`
	Apps     []FilteredProviderResult `json:"providers"`
}

// FilteredProviderResult represents filtered results for a single provider
type FilteredProviderResult struct {
	Name     string                `json:"name"`
	Type     string                `json:"type"`
	NodeName string                `json:"host_name,omitempty"`    // Node that provides this provider
	NodeAddr string                `json:"host_address,omitempty"` // Node address
	Models   []FilteredModelResult `json:"models"`
}

// FilteredModelResult represents a single filtered model result
type FilteredModelResult struct {
	Name     string `json:"name"`
	FullID   string `json:"full_id,omitempty"` // Full identifier (org/model/filename for GGUF, org/model for HF)
	Status   string `json:"status,omitempty"`
	MemoryMB int64  `json:"memory_mb,omitempty"`
	// Additional metadata for provider-specific information
	Size     int64  `json:"size,omitempty"`     // Size in bytes
	Modified string `json:"modified,omitempty"` // Last modified time (formatted)
	Digest   string `json:"digest,omitempty"`   // Model digest/ID
	// Running model information (from provider APIs)
	ExpiresAt     string `json:"expires_at,omitempty"`     // expiration timestamp
	ContextLength int    `json:"context_length,omitempty"` // context length
	SizeVRAM      int64  `json:"size_vram,omitempty"`      // VRAM usage in bytes
	Format        string `json:"format,omitempty"`         // model format
	Family        string `json:"family,omitempty"`         // model family
	ParameterSize string `json:"parameter_size,omitempty"` // parameter size string
	QuantLevel    string `json:"quant_level,omitempty"`    // quantization level
}

// LoadModelRequest represents a model load request
type LoadModelRequest struct {
	Node        string            `json:"node,omitempty"` // Target cluster node
	App         string            `json:"provider,omitempty"`
	ModelName   string            `json:"model_name"`
	Force       bool              `json:"force,omitempty"`       // Skip format validation
	Parameters  map[string]string `json:"parameters,omitempty"`  // Model-specific runtime parameters
	Environment map[string]string `json:"environment,omitempty"` // Model-specific environment variables
}

// UnloadModelRequest represents a model unload request
type UnloadModelRequest struct {
	ModelName string `json:"model_name"`
}

// RunPreview represents a preview of what a run would look like without executing it
type RunPreview struct {
	Model            string            `json:"model"`
	App              string            `json:"provider"`
	Node             string            `json:"node"`
	Port             int               `json:"port"`
	Command          string            `json:"command"`
	Args             []string          `json:"args"`
	FullCommand      string            `json:"full_command"`
	WorkingDir       string            `json:"working_dir"`
	Parameters       map[string]string `json:"parameters"`
	Environment      map[string]string `json:"environment"`
	ParameterSources map[string]string `json:"parameter_sources"`
}

// Instance represents a running app instance
type Instance struct {
	ID              string         `json:"id"`
	Node            string         `json:"node"`
	App             string         `json:"provider"`
	LaunchMode      string         `json:"launch_mode"`
	Model           string         `json:"model"`
	SourceRepo      string         `json:"source_repo,omitempty"`    // Source repository (huggingface, ollama, etc.)
	SizeBytes       int64          `json:"size_bytes,omitempty"`     // Model size in bytes
	Processor       string         `json:"processor,omitempty"`      // CPU/GPU usage
	ContextLength   int            `json:"context_length,omitempty"` // Context window size
	Port            int            `json:"port"`
	Status          string         `json:"status"`
	HealthURL       string         `json:"health_url"`
	ContainerID     string         `json:"container_id,omitempty"`
	ProcessID       int            `json:"process_id,omitempty"`
	StartedAt       string         `json:"started_at"`
	LastHealthCheck string         `json:"last_health_check,omitempty"`
	KeepAlive       string         `json:"keep_alive,omitempty"`     // Keep-alive duration
	LastActivity    string         `json:"last_activity,omitempty"`  // Last activity time
	LaunchCommand   *LaunchCommand `json:"launch_command,omitempty"` // Structured launch command
}

// LaunchCommand represents the structured command used to launch a process
type LaunchCommand struct {
	Command     string            `json:"command"`               // The executable command
	Args        []string          `json:"args"`                  // Command arguments
	Environment map[string]string `json:"environment,omitempty"` // Environment variables
	WorkingDir  string            `json:"working_dir,omitempty"` // Working directory
}

// ListInstancesResponse represents the response from /zzrouter/v1/runs
type ListInstancesResponse struct {
	Data    []Instance `json:"data"`
	Total   int        `json:"total"`
	HasMore bool       `json:"has_more"`
}

// ErrorResponse decodes error bodies from the zzrouter HTTP API.
//
// The server speaks RFC 9457 Problem Details (`type`, `title`, `detail`,
// `status`, `errors[]`) on its own endpoints, but OpenAI-compatible routes
// return `{error: {message, type}}` and a few passthrough paths return a
// plain `{error: "..."}` string. ErrorResponse accepts all three.
type ErrorResponse struct {
	Error  any    `json:"error"`  // string or OpenAI {"type":"...","message":"..."} object
	Title  string `json:"title"`  // RFC 9457 Problem Details
	Detail string `json:"detail"` // RFC 9457 Problem Details
}

// APIError is a management-route failure with its status kept.
//
// Several statuses on this API are answers rather than faults — 503 for
// a feature switched off, 409 for "nothing to do", 404 for "no such
// thing yet" — and a caller that has to tell them apart by matching the
// message gets it wrong the first time one is reworded. Error() renders
// exactly what the flat formatting used to, so callers that only print
// it see no change.
type APIError struct {
	Operation  string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s failed with status %d: %s", e.Operation, e.StatusCode, e.Message)
}

// ChatError is a structured error from chat completions with HTTP status context.
type ChatError struct {
	StatusCode int
	Message    string
	// Type is the OpenAI error envelope's `type` (see
	// pkg/protocol/openai.ErrorType). Upstream messages are often too
	// vague to act on — OpenRouter answers a throttled free-tier model
	// with a bare "Provider returned error" — and the type is what
	// separates "wait and retry" from "this model is misconfigured".
	Type string
}

func (e *ChatError) Error() string {
	return e.Message
}

// Explain renders the error in terms a person can act on, falling back to
// the upstream message when the type carries nothing extra.
func (e *ChatError) Explain() string {
	switch openai.ErrorType(e.Type) {
	case openai.ErrorTypeRateLimit:
		return "rate limited by the provider, retry shortly"
	case openai.ErrorTypeInsufficientQuota:
		return "provider quota exhausted"
	case openai.ErrorTypeAuthentication:
		return "provider rejected the API key"
	case openai.ErrorTypePermission:
		return "not permitted to use this model"
	case openai.ErrorTypeNotFound:
		return "the provider does not know this model"
	default:
		return e.Message
	}
}

// ErrorMessage extracts a human-readable message from the error response.
func (e *ErrorResponse) ErrorMessage() string {
	if e.Detail != "" {
		return e.Detail
	}
	if e.Title != "" {
		return e.Title
	}
	if m, ok := e.Error.(map[string]any); ok {
		if msg, _ := m["message"].(string); msg != "" {
			return msg
		}
	}
	if s, ok := e.Error.(string); ok && s != "" {
		return s
	}
	return ""
}

// ErrorType returns the OpenAI envelope's `type`, or "" for the RFC 9457
// and bare-string shapes, which carry no equivalent.
func (e *ErrorResponse) ErrorType() string {
	if m, ok := e.Error.(map[string]any); ok {
		t, _ := m["type"].(string)
		return t
	}
	return ""
}
