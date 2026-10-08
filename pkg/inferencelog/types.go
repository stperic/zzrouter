// Package inferencelog provides an in-memory ring buffer for LLM inference request logging.
package inferencelog

import "time"

// LogEntry represents a single inference request/response pair.
type LogEntry struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Model     string    `json:"model"`
	// ResponseModel is the model the upstream backend reported actually
	// using — distinct from Model when model-group routing resolves a
	// group alias to a concrete deployment, or when a backend
	// auto-versions (OpenAI's "gpt-4-turbo" → "gpt-4-turbo-2024-04-09").
	// Lets audit consumers identify which deployment served each
	// request without conflating it with the request-side alias.
	ResponseModel   string `json:"response_model,omitempty"`
	App             string `json:"provider"`
	RequestType     string `json:"request_type"`
	Stream          bool   `json:"stream"`
	RoutingDecision string `json:"routing_decision"`
	Node            string `json:"node"`
	Status          string `json:"status"`

	// Request details
	// PayloadAvailable reports that the full request bodies for this
	// entry are still retained and readable at /inference-logs/:id/payload.
	PayloadAvailable bool      `json:"payload_available,omitempty"`
	SystemPrompt     string    `json:"system_prompt,omitempty"`
	Messages         []Message `json:"messages,omitempty"`
	// Response is the assistant's reply for THIS entry. Without it the
	// reply is only visible as history in the next request's Messages.
	Response    string   `json:"response,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`

	// Response metrics
	TokensIn        int64   `json:"tokens_in"`
	TokensOut       int64   `json:"tokens_out"`
	TokensCached    int64   `json:"tokens_cached,omitempty"`
	TokensReasoning int64   `json:"tokens_reasoning,omitempty"`
	TokensPerSec    float64 `json:"tokens_per_sec"`
	TTFTMs          float64 `json:"ttft_ms"`
	LatencyMs       float64 `json:"latency_ms"`
	Cost            float64 `json:"cost,omitempty"`        // USD cost (cloud providers)
	CostSource      string  `json:"cost_source,omitempty"` // "" | "provider" | "zzrouter"

	// Model group routing
	GroupName      string `json:"group_name,omitempty"`      // Model group alias used (e.g., "fast-chat")
	DeploymentName string `json:"deployment_name,omitempty"` // Deployment that served the request
	FallbackCount  int    `json:"fallback_count,omitempty"`  // Number of deployments tried before success
	FallbackFrom   string `json:"fallback_from,omitempty"`   // First attempted deployment (if fallback occurred)
	KeyID          string `json:"key_id,omitempty"`          // Virtual key that made the request
	TeamID         string `json:"team_id,omitempty"`         // Team the key belongs to (empty if teamless)

	// Error info
	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// Message represents a chat message with role and content.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// QueryFilter controls which log entries are returned by Store.Query.
type QueryFilter struct {
	Model     string
	Status    string
	KeyID     string
	TeamID    string
	GroupName string
	Since     time.Time
	Limit     int
	Offset    int
}

// Status constants for LogEntry.Status.
const (
	StatusSuccess = "success"
	StatusError   = "error"
)
