// package server provides HTTP handlers for the zzrouter host server.
// Types - API request/response types and helper functions
package server

import (
	"fmt"
)

// ============================================================================
// Context Keys (avoid collisions - staticcheck SA1029)
// ============================================================================

// contextKey is a custom type for context keys to avoid collisions
type contextKey string

const (
	// CtxKeyOriginalBody stores the original request body for forwarding
	CtxKeyOriginalBody contextKey = "original_body"
	// CtxKeyModel stores the model name from the request
	CtxKeyModel contextKey = "zzrouter_model"
	// CtxKeyClientModel stores the model name exactly as the client sent
	// it, before any zzRouter-only decoration (the @node hint) is stripped
	// for the wire. Set only when the two differ, and read on the way out
	// so the response echoes the id the caller has in its own catalog.
	CtxKeyClientModel contextKey = "zzrouter_client_model"

	// CtxKeySuppressUsageFrame marks a request whose caller did not ask to
	// see token usage. Usage is still requested from the engine so the
	// request can be metered; this says the terminal usage frame must be
	// dropped before the response reaches them.
	CtxKeySuppressUsageFrame contextKey = "zzrouter_suppress_usage_frame"
	// CtxKeyInferenceRecorder stores the inference recorder for end-to-end metrics and logging
	CtxKeyInferenceRecorder contextKey = "inference_recorder"
	// CtxKeyAccessContext stores the resolved AccessContext for the authenticated request
	CtxKeyAccessContext contextKey = "access_context"
	// CtxKeyConcurrencyRelease stores the concurrency slot release func() from AccessControl.Enforce
	CtxKeyConcurrencyRelease contextKey = "concurrency_release"
	// CtxKeyReservationID stores the control.ReservationID returned by
	// Enforce when a budget reservation was stashed. Consumed by the
	// inference-log bridge at settle time and by the post-response
	// CancelPendingReservationIfUnsettled fallback.
	CtxKeyReservationID contextKey = "reservation_id"
	// CtxKeyProvider stores the provider key that served the request.
	// Stashed by dispatchTo so post-dispatch observers (e.g.
	// the /v1/responses affinity recorder) can see which backend actually
	// served the request without re-running resolution.
	CtxKeyProvider contextKey = "zzrouter_provider"
	// CtxKeyNotInference marks a request routed like inference that runs
	// none (count_tokens), so no proxy site logs or counts it as one.
	CtxKeyNotInference contextKey = "zzrouter_not_inference"
	// CtxKeyRequestHints stores a *keepalive.Override parsed from the
	// inference request body. Consumed by proxyToInstance to apply
	// per-request keep_alive overrides to the zzRouter-managed
	// instance. Localhost-only: remote cluster routes do not carry overrides.
	CtxKeyRequestHints contextKey = "zzrouter_request_hints"

	// CtxKeyUserRole, CtxKeyAPIKeyFingerprint, CtxKeyAuthenticated — audit
	// fields the access-control layer writes onto gin context alongside
	// the full AccessContext. Consumed by request-logging middleware
	// (today external, hence string keys preserved for wire-compat);
	// exposing them as typed constants kills the "c.GetString(\"user_role\")
	// with wrong casing silently returns empty" drift class.
	CtxKeyUserRole          contextKey = "user_role"
	CtxKeyAPIKeyFingerprint contextKey = "api_key_fingerprint" //nolint:gosec // context key name, not a credential
	CtxKeyAuthenticated     contextKey = "authenticated"
	// CtxKeyClusterTrusted is set by clusterTrustedMiddleware on the
	// worker compat engine (cluster mTLS port). Marks the request as
	// coord-originated proxy traffic so OptionalAuthMiddleware can skip
	// re-validating the X-API-Key forwarded from the original caller —
	// the mTLS transport IS the authentication, the user key is metadata.
	CtxKeyClusterTrusted contextKey = "cluster_trusted"
)

// ============================================================================
// Model API Types
// ============================================================================

// ListModelsRequest represents a request to list models
type ListModelsRequest struct {
	Node     string // Target host (empty = auto-discover/broadcast)
	Registry string // Filter by registry (e.g., "ollama", "huggingface")
	App      string // Filter by assigned_app (e.g., "vllm", "ollama")
	Model    string // Filter by model name pattern
	Refresh  bool   // Force cache invalidation and worker refresh
}

// Validate checks if the request is valid
func (r *ListModelsRequest) Validate() error {
	// All fields are optional for list
	return nil
}

// ShowModelRequest represents a request to show model details
type ShowModelRequest struct {
	ModelName string // Required: model name to show
	Node      string // Optional: target specific host
	App       string // Optional: filter by assigned_app
	Verbose   bool   // Optional: verbose output
}

// Validate checks if the request is valid
func (r *ShowModelRequest) Validate() error {
	if r.ModelName == "" {
		return fmt.Errorf("model name is required")
	}
	return nil
}

// ShowModelResponse + ModelRouteInfo moved to pkg/dispatch/adapter.
// See dispatch_aliases.go for the server-side re-exports.

// ErrMultipleMatches indicates multiple models matched the query
type ErrMultipleMatches struct {
	ModelName string
	Matches   []string
}

func (e ErrMultipleMatches) Error() string {
	return fmt.Sprintf("multiple models match '%s': %v", e.ModelName, e.Matches)
}
