package openai

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// Responder implements httperr.Responder for the OpenAI /v1/* dialect.
//
// It is safe for concurrent use by many goroutines and holds no
// per-request state — construct one instance at server startup and
// share it across every /v1/* route group.
//
// Construction MUST go through New so options can evolve without
// breaking callers. Direct struct literal instantiation is not
// supported; future fields may carry invariants that only New can
// enforce.
type Responder struct {
	opaque bool
	logger InternalLogger
}

// InternalLogger is invoked by Responder.Internal and Responder.BadGateway
// with the raw, unsanitized detail string and the request identifier
// drawn from the gin context. Implementations land the detail in the
// server's structured log so operators can correlate a client-visible
// request id with the underlying cause.
//
// The client response NEVER contains the detail — only the log does.
// That asymmetry is the whole point of the logger: callers get to
// pass context for debugging without leaking it on the wire.
//
// A nil logger is legal; Responder silently drops the detail in that
// case. Production servers should always supply a logger.
type InternalLogger func(reqID, detail string)

// Option configures a Responder at construction time. Options are
// evaluated in order; later options override earlier ones.
type Option func(*Responder)

// WithOpaqueErrors collapses every error response to its HTTP status
// code with an empty body. Response headers (X-Request-Id, Retry-After)
// are still set when applicable. This matches LocalAI's OPAQUE_ERRORS
// hardening mode and is intended for deployments where even sanitized
// error messages are considered leakage.
//
// OPAQUE mode is incompatible with SDK error classification — openai-python
// cannot construct a typed exception without reading error.type. Enable
// it only for production ingresses that front trusted clients.
func WithOpaqueErrors(opaque bool) Option {
	return func(r *Responder) { r.opaque = opaque }
}

// WithInternalLogger attaches a logger that receives the raw detail
// strings passed to Internal and BadGateway. See the InternalLogger
// documentation for the security contract. Passing nil is equivalent
// to omitting the option.
func WithInternalLogger(l InternalLogger) Option {
	return func(r *Responder) { r.logger = l }
}

// New constructs a Responder with the given options. The default
// configuration emits full (sanitized) error envelopes and has no
// internal logger attached.
func New(opts ...Option) *Responder {
	r := &Responder{}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

// Compile-time check that Responder satisfies the generic interface.
var _ httperr.Responder = (*Responder)(nil)

// Name implements httperr.Responder.
func (*Responder) Name() string { return "openai" }

// writeEnvelope performs the common work of every error method:
//
//   - Propagates the request id to the X-Request-Id response header
//     (always, even in opaque mode — the header does not leak the
//     error body).
//   - In opaque mode, writes the status code with no body.
//   - Otherwise, builds a validated Envelope from typed dialect
//     constants, runs the message through sanitize, and writes it as
//     JSON.
//
// The method panics if the caller passes an ErrorType or ErrorCode
// that is not in the closed vocabulary. That is deliberate — the
// Responder is the enforcement point for the spec contract, and a
// typo in the caller must fail loudly in tests rather than silently
// shipping a non-compliant envelope to production.
func (r *Responder) writeEnvelope(c *gin.Context, status int, errType ErrorType, code ErrorCode, message, param string) {
	if !errType.Valid() {
		panic(fmt.Sprintf("protocol/openai: unknown ErrorType %q", errType))
	}
	if code != "" && !code.Valid() {
		panic(fmt.Sprintf("protocol/openai: unknown ErrorCode %q", code))
	}

	if reqID := requestIDFrom(c); reqID != "" {
		c.Header("X-Request-Id", reqID)
	}

	if r.opaque {
		// AbortWithStatus forces gin to flush the status code
		// immediately and halts the handler chain so no downstream
		// middleware writes to the response. A bare c.Status(status)
		// would be buffered until a first write that never happens.
		c.AbortWithStatus(status)
		return
	}

	env := NewEnvelope(errType, code, sanitize(message), param)
	c.AbortWithStatusJSON(status, env)
}

// BadRequest implements httperr.Responder.
func (r *Responder) BadRequest(c *gin.Context, detail, param string) {
	r.writeEnvelope(c, http.StatusBadRequest,
		ErrorTypeInvalidRequest, ErrorCodeInvalidRequest, detail, param)
}

// Unauthorized implements httperr.Responder.
func (r *Responder) Unauthorized(c *gin.Context, detail string) {
	httperr.SetAuthenticateChallenge(c)
	r.writeEnvelope(c, http.StatusUnauthorized,
		ErrorTypeAuthentication, ErrorCodeInvalidAPIKey, detail, "")
}

// Forbidden implements httperr.Responder.
func (r *Responder) Forbidden(c *gin.Context, detail string) {
	r.writeEnvelope(c, http.StatusForbidden,
		ErrorTypePermission, ErrorCodeInsufficientPermissions, detail, "")
}

// NotFound implements httperr.Responder. It inspects the detail
// string to choose between the generic unknown-endpoint code and the
// model-specific code OpenAI clients recognise.
func (r *Responder) NotFound(c *gin.Context, detail string) {
	code := ErrorCodeUnknownURL
	if looksLikeModelNotFound(detail) {
		code = ErrorCodeModelNotFound
	}
	r.writeEnvelope(c, http.StatusNotFound,
		ErrorTypeInvalidRequest, code, detail, "")
}

// MethodNotAllowed implements httperr.Responder.
func (r *Responder) MethodNotAllowed(c *gin.Context, detail string) {
	r.writeEnvelope(c, http.StatusMethodNotAllowed,
		ErrorTypeInvalidRequest, ErrorCodeMethodNotAllowed, detail, "")
}

// RequestTooLarge implements httperr.Responder.
func (r *Responder) RequestTooLarge(c *gin.Context, detail string) {
	r.writeEnvelope(c, http.StatusRequestEntityTooLarge,
		ErrorTypeInvalidRequest, ErrorCodeRequestTooLarge, detail, "")
}

// TooManyRequests implements httperr.Responder. Sets Retry-After as
// delta-seconds when a positive value is provided.
func (r *Responder) TooManyRequests(c *gin.Context, detail string, retryAfterSeconds int) {
	if retryAfterSeconds > 0 {
		c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSeconds))
	}
	r.writeEnvelope(c, http.StatusTooManyRequests,
		ErrorTypeRateLimit, ErrorCodeRateLimitExceeded, detail, "")
}

// Internal implements httperr.Responder.
//
// SECURITY: the `detail` argument is NEVER written to the client. It
// is passed to the configured InternalLogger (if any) for server-side
// correlation and then discarded. The client sees a fixed generic
// message. This is the single point that enforces zzRouter's policy
// of never leaking 500-class runtime state on the wire.
func (r *Responder) Internal(c *gin.Context, detail string) {
	r.logInternalDetail(c, detail)
	r.writeEnvelope(c, http.StatusInternalServerError,
		ErrorTypeServer, ErrorCodeInternalError, "internal server error", "")
}

// BadGateway implements httperr.Responder. Follows the same
// server-side-only detail policy as Internal.
func (r *Responder) BadGateway(c *gin.Context, detail string) {
	r.logInternalDetail(c, detail)
	r.writeEnvelope(c, http.StatusBadGateway,
		ErrorTypeAPI, ErrorCodeUpstreamError, "upstream error", "")
}

// Unavailable implements httperr.Responder. Unlike Internal/BadGateway
// the detail IS echoed to the client (after sanitization) because
// 503s are expected, configuration-driven states — "feature disabled",
// "draining", "no default backend configured", "backend unavailable" —
// and the detail helps the operator fix their setup.
//
// The code is derived from the detail string via a small pattern
// matcher. Callers should write their detail so the intended code is
// picked up; see the tests in responder_test.go for the exact phrases.
func (r *Responder) Unavailable(c *gin.Context, detail string) {
	code := ErrorCodeFeatureDisabled
	switch {
	case containsFold(detail, "no default backend"):
		code = ErrorCodeNoDefaultBackend
	case containsFold(detail, "draining"):
		code = ErrorCodeDraining
	case containsFold(detail, "unavailable") ||
		containsFold(detail, "not available"):
		code = ErrorCodeBackendUnavailable
	}
	r.writeEnvelope(c, http.StatusServiceUnavailable,
		ErrorTypeServer, code, detail, "")
}

// GatewayTimeout implements httperr.Responder.
func (r *Responder) GatewayTimeout(c *gin.Context, detail string) {
	r.logInternalDetail(c, detail)
	r.writeEnvelope(c, http.StatusGatewayTimeout,
		ErrorTypeAPI, ErrorCodeUpstreamTimeout, "upstream timeout", "")
}

// logInternalDetail forwards the raw detail to the configured
// InternalLogger. No-op when no logger is attached or when the detail
// is empty.
func (r *Responder) logInternalDetail(c *gin.Context, detail string) {
	if r.logger == nil || detail == "" {
		return
	}
	r.logger(requestIDFrom(c), detail)
}

// requestIDFrom extracts the request identifier from the gin context.
// The key name matches the convention used elsewhere in zzRouter
// (see internal/server/problem.go). Returns an empty string when no
// request id is present.
func requestIDFrom(c *gin.Context) string {
	if c == nil {
		return ""
	}
	v, ok := c.Get("request_id")
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// looksLikeModelNotFound reports whether a NotFound detail string
// looks like it refers to a missing model rather than a missing URL.
// The heuristic is intentionally simple — the caller (typically a
// handler that already knows the failure mode) can force the
// specific code by constructing the envelope directly.
func looksLikeModelNotFound(detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "model") && strings.Contains(lower, "not found")
}

// containsFold is a case-insensitive strings.Contains. Kept local so
// the responder file has no standard-library import beyond `strings`.
func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
