// Package routing provides request routing for zzRouter cluster communication.
//
// # Terminology Glossary
//
//   - Node: A zzRouter server instance (the binary is zzrouter-node). Identified by
//     hostname, IP address, or special tokens like "@master" and "*".
//   - Endpoint: A node's network address or URL (e.g., "http://192.0.2.10:9090").
//   - Provider/App: An LLM backend registered on a node (Ollama, vLLM, etc.).
package routing

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ============================================================================
// Request/Response Types
// ============================================================================

// RoutingMode defines how a request should be routed
type RoutingMode int

const (
	// ModeLocal executes the request on the current node
	ModeLocal RoutingMode = iota

	// ModeUnicast routes to a specific target node
	ModeUnicast

	// ModeBroadcast routes to all cluster nodes and aggregates results
	ModeBroadcast

	// ModeAuto performs auto-discovery to find the target node
	ModeAuto
)

// Request represents a routing request
// Note: Context is passed separately to Route() method, not stored in struct
type Request struct {
	// Method is the HTTP method (GET, POST, etc.)
	Method string

	// Path is the request path (e.g., "/api/generate")
	Path string

	// Node is the target node identifier. Accepted values:
	//   - "" (empty): defaults to local node or broadcast depending on context
	//   - "*": wildcard, broadcast to all nodes
	//   - "@master": the coordinator/master node
	//   - "@local": explicitly target the local node
	//   - "localhost": the local node
	//   - A hostname (e.g., "gpu-server") or IP (e.g., "192.0.2.10")
	//   - A hostname:port or full URL (e.g., "http://192.0.2.10:9090")
	Node string

	// ModelName is used for auto-discovery
	ModelName string

	// Body is the request body
	Body []byte

	// Headers are the request headers
	Headers http.Header

	// Timeout for the request
	Timeout time.Duration
}

// Response represents a routing response
type Response struct {
	// StatusCode is the HTTP status code
	StatusCode int

	// Body is the response body
	Body []byte

	// Headers are the response headers
	Headers http.Header

	// Node identifies which node produced this response. Values include:
	//   - A hostname or IP of the responding node (e.g., "gpu-server", "192.0.2.10")
	//   - "localhost" for responses from the local node
	//   - "aggregated" for broadcast responses that combine data from multiple nodes
	Node string

	// Duration is how long the request took
	Duration time.Duration

	// Error if the request failed
	Error error
}

// ModelLocation represents where a model is located
type ModelLocation struct {
	// ModelName is the name of the model
	ModelName string

	// Node is where the model is located
	Node string

	// Provider is the provider type (ollama, vllm, mlx, etc.)
	Provider string

	// LastSeen is when this information was last verified
	LastSeen time.Time
}

// ============================================================================
// Interfaces
// ============================================================================

// Router is the main routing interface
// TWO operations only: Unicast and Broadcast
//
// Usage in handlers:
//
//	// Send to ONE node (local or remote - transparent)
//	resp, err := s.router.Unicast(ctx, node, path, method, body)
//
//	// Send to ALL nodes
//	resp, err := s.router.Broadcast(ctx, path, method, body)
type Router interface {
	// Unicast sends request to ONE node
	// If node is this node -> executes locally
	// If node is remote -> sends to that node
	// The caller doesn't know or care which happens
	Unicast(ctx context.Context, host, path, method string, body []byte) (*Response, error)

	// Broadcast sends request to ALL nodes and aggregates results
	// Always includes local node + all cluster workers
	Broadcast(ctx context.Context, path, method string, body []byte) (*Response, error)

	// Route is a convenience method that chooses Unicast or Broadcast based on target node
	// - Empty or "*" -> Broadcast
	// - Specific node -> Unicast
	Route(ctx context.Context, req *Request) (*Response, error)

	// MultiRoute routes to multiple nodes with different request bodies per node
	// Used for operations where each item has its own target node (e.g., deletion)
	MultiRoute(ctx context.Context, path string, method string, hostBodies map[string][]byte) (*Response, error)
}

// LocalHandler is the function signature for local request processing
type LocalHandler func(ctx context.Context, req *Request) (*Response, error)

// ModelRegistry provides model discovery
type ModelRegistry interface {
	// FindModel returns the node where a model is located
	FindModel(ctx context.Context, modelName string) (*ModelLocation, error)

	// InvalidateCache clears the cache for a specific model
	InvalidateCache(modelName string)

	// InvalidateAll clears the entire cache
	InvalidateAll()
}

// ============================================================================
// Error Types
// ============================================================================

// RoutingError represents an error that occurred during routing
type RoutingError struct {
	Code      int    // HTTP status code
	Message   string // User-facing message
	Details   string // Technical details
	Cause     error  // Underlying error
	Component string // Which component failed (router, executor, etc.)
	Operation string // What operation was being performed
}

// Error implements the error interface
func (e *RoutingError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s (caused by: %v)", e.Message, e.Details, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Message, e.Details)
}

// Unwrap returns the underlying error
func (e *RoutingError) Unwrap() error {
	return e.Cause
}
