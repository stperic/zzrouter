package openai

// ErrorType is the closed vocabulary for the OpenAI error envelope's
// `type` field. See https://platform.openai.com/docs/guides/error-codes.
//
// SDK clients switch on this string to construct typed exception
// classes (openai-python's AuthenticationError, RateLimitError,
// PermissionDeniedError, ...). Emitting a value outside this set
// silently falls through to a generic APIError in every major SDK
// and breaks retry classifiers.
//
// New values may be added in future SpecVersions — see version.go —
// but the string form of each constant is locked in forever once
// shipped.
type ErrorType string

// ErrorType constants. Every /v1/* error response MUST use one of
// these values and nothing else. Order matches the grouping on the
// OpenAI error-codes reference page so reviewers can cross-check.
const (
	// ErrorTypeInvalidRequest covers malformed input, missing
	// required fields, values out of range, and unknown endpoints
	// (OpenAI overloads this type for 404 as well as 400).
	// SDK target: BadRequestError / NotFoundError.
	ErrorTypeInvalidRequest ErrorType = "invalid_request_error"

	// ErrorTypeAuthentication covers missing credentials and
	// invalid API keys. SDK target: AuthenticationError.
	ErrorTypeAuthentication ErrorType = "authentication_error"

	// ErrorTypePermission covers cases where the caller is
	// authenticated but lacks permission for the requested resource
	// or action. SDK target: PermissionDeniedError.
	ErrorTypePermission ErrorType = "permission_error"

	// ErrorTypeNotFound is a dedicated not-found type used by
	// OpenAI for some stateful resources (assistants, threads,
	// files) in addition to the invalid_request_error 404 path.
	// SDK target: NotFoundError.
	ErrorTypeNotFound ErrorType = "not_found_error"

	// ErrorTypeRateLimit covers quota exhaustion and per-key
	// rate limiting. Responders SHOULD set Retry-After on 429.
	// SDK target: RateLimitError.
	ErrorTypeRateLimit ErrorType = "rate_limit_error"

	// ErrorTypeServer covers zzRouter-internal failures: panics,
	// misconfiguration, unrecoverable routing errors. Reserved
	// per backend_error_normalize.go convention for failures that
	// originate inside zzRouter. SDK target: InternalServerError.
	ErrorTypeServer ErrorType = "server_error"

	// ErrorTypeAPI covers upstream failures: a backend (vLLM,
	// llama.cpp, MLX, cloud provider) returned a non-2xx response.
	// Distinct from ErrorTypeServer so clients can tell an
	// upstream outage from a zzRouter bug. SDK target: APIStatusError.
	ErrorTypeAPI ErrorType = "api_error"

	// ErrorTypeInsufficientQuota signals that the caller has
	// exhausted their allowance and must wait or upgrade. Retained
	// for parity with OpenAI even though zzRouter does not
	// currently enforce quotas — future rate-limit work will use it.
	ErrorTypeInsufficientQuota ErrorType = "insufficient_quota"

	// ErrorTypeOverloaded is OpenAI's signal for transient
	// server-side congestion: the request is well-formed but the
	// service cannot process it right now. SDK target: APIStatusError
	// with status 503.
	ErrorTypeOverloaded ErrorType = "overloaded_error"
)

// allErrorTypes is the authoritative list of known ErrorType values,
// sorted for deterministic enumeration in tests and drift checks.
var allErrorTypes = []ErrorType{
	ErrorTypeAPI,
	ErrorTypeAuthentication,
	ErrorTypeInsufficientQuota,
	ErrorTypeInvalidRequest,
	ErrorTypeNotFound,
	ErrorTypeOverloaded,
	ErrorTypePermission,
	ErrorTypeRateLimit,
	ErrorTypeServer,
}

// AllErrorTypes returns the full closed vocabulary of ErrorType
// values in a stable order. Intended for tests, OpenAPI spec drift
// checks, and any runtime validation.
func AllErrorTypes() []ErrorType {
	out := make([]ErrorType, len(allErrorTypes))
	copy(out, allErrorTypes)
	return out
}

// Valid reports whether t is a known ErrorType. A Responder MUST
// panic rather than emit an unknown type — do NOT silently fall back.
func (t ErrorType) Valid() bool {
	for _, v := range allErrorTypes {
		if v == t {
			return true
		}
	}
	return false
}

// ErrorCode is the closed vocabulary for the OpenAI error envelope's
// `code` field. While `type` carries the error family, `code` names
// the specific failure mode and is the field SDK retry logic
// usually branches on.
//
// OpenAI publishes some codes in the API reference (model_not_found,
// context_length_exceeded, rate_limit_exceeded) and leaves others
// implicit. zzRouter adds a small number of zzRouter-specific codes
// for failures OpenAI itself cannot produce (no_default_backend_configured,
// upstream_error, upstream_timeout). These are documented per-constant
// below and are safe to emit because the openai-python SDK treats
// unknown codes as opaque strings — classification happens on `type`,
// not `code`.
type ErrorCode string

// ErrorCode constants. Grouped by the error.type they typically
// accompany so reviewers can audit type/code coherence at a glance.
const (
	// --- authentication_error ---

	// ErrorCodeInvalidAPIKey is emitted on 401 when the presented
	// key is malformed, expired, or unknown.
	ErrorCodeInvalidAPIKey ErrorCode = "invalid_api_key"

	// --- permission_error ---

	// ErrorCodeInsufficientPermissions is emitted on 403 when the
	// caller is authenticated but lacks the required role/scope.
	ErrorCodeInsufficientPermissions ErrorCode = "insufficient_permissions"

	// --- invalid_request_error ---

	// ErrorCodeInvalidRequest is the generic catch-all for 400s
	// when no more specific code applies (malformed JSON, missing
	// required field, unknown field when strict).
	ErrorCodeInvalidRequest ErrorCode = "invalid_request_error"

	// ErrorCodeUnknownURL is emitted on 404 when the request path
	// does not match any registered endpoint. Distinct from
	// ErrorCodeModelNotFound because an unknown URL is a client
	// bug whereas a missing model is a configuration/inventory issue.
	ErrorCodeUnknownURL ErrorCode = "unknown_url"

	// ErrorCodeMethodNotAllowed is emitted on 405 when the path
	// exists but the HTTP method is not registered for it.
	ErrorCodeMethodNotAllowed ErrorCode = "method_not_allowed"

	// ErrorCodeRequestTooLarge is emitted on 413 when the request
	// body exceeds the configured limit.
	ErrorCodeRequestTooLarge ErrorCode = "request_too_large"

	// ErrorCodeModelNotFound is emitted on 404 when the requested
	// model name is not registered. Matches OpenAI's own code for
	// this case.
	ErrorCodeModelNotFound ErrorCode = "model_not_found"

	// ErrorCodeEndpointNotSupported is emitted on 501 when the
	// endpoint is recognised but not implemented (e.g. /v1/realtime
	// in the current zzRouter build).
	ErrorCodeEndpointNotSupported ErrorCode = "endpoint_not_supported"

	// --- rate_limit_error ---

	// ErrorCodeRateLimitExceeded is emitted on 429 for both
	// per-key quota exhaustion and global rate limiting.
	ErrorCodeRateLimitExceeded ErrorCode = "rate_limit_exceeded"

	// ErrorCodeInsufficientQuota is emitted on 429 when the account
	// has exhausted its billing allowance. openai-python classifies
	// this code into BillingError distinct from RateLimitError.
	ErrorCodeInsufficientQuota ErrorCode = "insufficient_quota"

	// --- content / context ---

	// ErrorCodeContextLengthExceeded is emitted on 400 when the
	// input plus generated tokens would exceed the model's context
	// window. openai-python surfaces this as a specific subclass so
	// agents can trim and retry without losing the rest of the
	// request semantics.
	ErrorCodeContextLengthExceeded ErrorCode = "context_length_exceeded"

	// ErrorCodeContentFilter is emitted on 400 when the backend's
	// moderation layer rejected the prompt or the generated output.
	// SDKs present this as a ContentFilterFinishReasonError alongside
	// the partial completion (if any).
	ErrorCodeContentFilter ErrorCode = "content_filter"

	// ErrorCodeModelNameConflict is emitted on 409 when real weights use
	// a reserved variant name. The operator must disambiguate the name.
	ErrorCodeModelNameConflict ErrorCode = "model_name_conflict"

	// --- server_error / api_error ---

	// ErrorCodeInternalError is emitted on 500 for any
	// zzRouter-internal failure: panics, unreachable service
	// dependencies, unrecoverable routing bugs.
	ErrorCodeInternalError ErrorCode = "internal_error"

	// ErrorCodeUpstreamError is emitted on 502 when a backend
	// returned a response that could not be parsed or the
	// connection to the backend failed.
	ErrorCodeUpstreamError ErrorCode = "upstream_error"

	// ErrorCodeUpstreamTimeout is emitted on 504 when a backend
	// did not respond within the configured deadline.
	ErrorCodeUpstreamTimeout ErrorCode = "upstream_timeout"

	// ErrorCodeFeatureDisabled is emitted on 503 when the
	// requested feature is gated off by configuration.
	ErrorCodeFeatureDisabled ErrorCode = "feature_disabled"

	// ErrorCodeDraining is emitted on 503 during server shutdown
	// when new requests are being rejected to drain in-flight work.
	ErrorCodeDraining ErrorCode = "draining"

	// ErrorCodeNoDefaultBackend is emitted on 503 by the stateful
	// /v1/* pass-through routes when openai_compat.default_backend
	// has not been configured. Preserved from the pre-responder
	// implementation for wire compatibility with existing clients.
	ErrorCodeNoDefaultBackend ErrorCode = "no_default_backend_configured"

	// ErrorCodeBackendUnavailable is emitted on 503 when an operator
	// did configure a backend (default, native-wire, or realtime) but
	// the referenced provider is not registered, not enabled, or has
	// no running endpoint. Distinct from NoDefaultBackend (which
	// signals missing config) and FeatureDisabled (which signals a
	// gated feature) so operators can triage the three states
	// independently.
	ErrorCodeBackendUnavailable ErrorCode = "backend_unavailable"

	// ErrorCodeProviderNotRunning identifies a confirmed stopped managed daemon.
	ErrorCodeProviderNotRunning ErrorCode = "provider_not_running"

	// ErrorCodeModelInventoryUnavailable means discovery cannot establish model existence.
	ErrorCodeModelInventoryUnavailable ErrorCode = "model_inventory_unavailable"

	// ErrorCodeIdempotencyKeyConflict is emitted on 422 when a request
	// reuses an Idempotency-Key that was recorded against a different
	// body. Replaying the recorded response would answer a question the
	// caller did not ask, so the conflict is reported instead.
	ErrorCodeIdempotencyKeyConflict ErrorCode = "idempotency_key_conflict"

	// ErrorCodeIdempotencyWaitTimeout is emitted on 504 when a duplicate
	// request waited out its bound for the original, still-in-flight
	// request to publish a response to replay.
	ErrorCodeIdempotencyWaitTimeout ErrorCode = "idempotency_wait_timeout"
)

// allErrorCodes is the authoritative list of known ErrorCode values,
// sorted for deterministic enumeration.
var allErrorCodes = []ErrorCode{
	ErrorCodeBackendUnavailable,
	ErrorCodeContentFilter,
	ErrorCodeContextLengthExceeded,
	ErrorCodeDraining,
	ErrorCodeEndpointNotSupported,
	ErrorCodeFeatureDisabled,
	ErrorCodeInsufficientPermissions,
	ErrorCodeIdempotencyKeyConflict,
	ErrorCodeIdempotencyWaitTimeout,
	ErrorCodeInsufficientQuota,
	ErrorCodeInternalError,
	ErrorCodeInvalidAPIKey,
	ErrorCodeInvalidRequest,
	ErrorCodeMethodNotAllowed,
	ErrorCodeModelNameConflict,
	ErrorCodeModelNotFound,
	ErrorCodeModelInventoryUnavailable,
	ErrorCodeProviderNotRunning,
	ErrorCodeNoDefaultBackend,
	ErrorCodeRateLimitExceeded,
	ErrorCodeRequestTooLarge,
	ErrorCodeUnknownURL,
	ErrorCodeUpstreamError,
	ErrorCodeUpstreamTimeout,
}

// AllErrorCodes returns the full closed vocabulary of ErrorCode
// values in a stable order. Intended for tests and drift checks.
func AllErrorCodes() []ErrorCode {
	out := make([]ErrorCode, len(allErrorCodes))
	copy(out, allErrorCodes)
	return out
}

// Valid reports whether c is a known ErrorCode.
func (c ErrorCode) Valid() bool {
	for _, v := range allErrorCodes {
		if v == c {
			return true
		}
	}
	return false
}
