package httperr

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Responder emits an HTTP error response in a specific dialect.
//
// Every method MUST:
//
//   - Write the HTTP status code appropriate to the error category.
//   - Set Content-Type appropriate to the dialect (application/json,
//     application/problem+json, ...).
//   - Sanitize any caller-provided detail string so internal paths,
//     stack fragments, and Go type names never reach the client. The
//     only exception is Internal, which MUST NOT emit the detail to
//     the client at all — implementations log it server-side and
//     respond with a fixed generic message.
//   - Propagate the request identifier (read from c.Get("request_id"))
//     as the `X-Request-Id` response header when present.
//   - Leave the gin.Context in an aborted state so no subsequent
//     handler writes to the already-closed response.
//
// Responders are expected to be safe for concurrent use by many
// goroutines. They hold no per-request state.
type Responder interface {
	// Name returns a short, stable identifier for the dialect.
	// Used for logging, metrics, and tests. Example values:
	// "openai", "problem", "ollama".
	Name() string

	// BadRequest emits a 400 for malformed or rejected client input.
	// `param` is the JSON field name (if applicable) that caused the
	// error; pass an empty string when not applicable. Implementations
	// MAY map param into a dialect-specific field.
	BadRequest(c *gin.Context, detail, param string)

	// Unauthorized emits a 401 for missing or invalid credentials.
	// Implementations MUST call SetAuthenticateChallenge so the
	// response carries the RFC 7235 challenge a 401 requires.
	Unauthorized(c *gin.Context, detail string)

	// Forbidden emits a 403 when authentication succeeded but the
	// caller lacks permission for the target resource or action.
	Forbidden(c *gin.Context, detail string)

	// NotFound emits a 404. The `detail` string is a hint the
	// implementation MAY use to choose a more specific error code
	// (e.g. "model X not found" → a model-specific sub-code vs a
	// generic unknown-endpoint sub-code). Implementations MUST still
	// sanitize the detail before emitting it to the client.
	NotFound(c *gin.Context, detail string)

	// MethodNotAllowed emits a 405.
	MethodNotAllowed(c *gin.Context, detail string)

	// RequestTooLarge emits a 413.
	RequestTooLarge(c *gin.Context, detail string)

	// TooManyRequests emits a 429. When `retryAfterSeconds` is > 0
	// implementations MUST set the `Retry-After` HTTP header to the
	// value expressed as delta-seconds. A value of 0 or less omits
	// the header.
	TooManyRequests(c *gin.Context, detail string, retryAfterSeconds int)

	// Internal emits a 500. Implementations MUST NOT echo `detail`
	// to the client — the detail is expected to contain arbitrary
	// runtime state (panic values, unsanitized upstream bodies,
	// stack fragments) and is retained on the interface only so
	// callers can pass information for server-side logging.
	Internal(c *gin.Context, detail string)

	// BadGateway emits a 502 for upstream connection or shape
	// failures. Implementations SHOULD NOT echo `detail` verbatim;
	// the upstream body may contain hostnames, credentials, or
	// other leak-worthy data.
	BadGateway(c *gin.Context, detail string)

	// Unavailable emits a 503. Unlike Internal/BadGateway this
	// method is used for expected, configuration-driven failures
	// ("feature disabled", "draining", "no default backend
	// configured") where echoing the detail is safe and useful.
	Unavailable(c *gin.Context, detail string)

	// GatewayTimeout emits a 504 for slow upstreams. Implementations
	// SHOULD NOT echo `detail` verbatim.
	GatewayTimeout(c *gin.Context, detail string)

	// The two methods below serve code that holds only the response
	// writer and the request -- the proxies, the cold-load path -- rather
	// than a gin context. They neither sanitize nor abort: e.Message is
	// the caller's to make safe, and there is no gin chain to stop.

	// WriteError renders e as the complete response.
	WriteError(w http.ResponseWriter, req *http.Request, e Error)

	// NormalizeUpstreamError rewrites a backend's 4xx/5xx body into this
	// dialect. raw may be nil when the body was unreadable; the status
	// still decides the error class.
	NormalizeUpstreamError(status int, raw []byte) []byte
}

// InBandStreamer is the optional half of a Responder whose clients take
// a failure on a streaming request inside a 200 stream rather than as a
// status, the way http.Flusher is the optional half of a ResponseWriter.
// A responder that implements it lets zzRouter open the stream before
// any backend byte -- to report a failure, or to show progress while a
// cold model loads. One that does not answers with the status, which is
// right for any client that retries a 429 or 529 on its own.
type InBandStreamer interface {
	// StreamStatus writes one progress frame.
	StreamStatus(w http.ResponseWriter, status, message string)

	// StreamError ends the stream with e, including whatever terminator
	// the protocol has, and flushes.
	StreamError(w http.ResponseWriter, e Error)
}

// Error is one error zzRouter reports in its own voice. Each dialect
// renders the fields it has a place for.
type Error struct {
	Status int
	// Type is the error class in OpenAI's vocabulary, which zzRouter's
	// call sites speak natively. A dialect with its own vocabulary
	// derives its class from Status instead.
	Type string
	// Code is the machine-readable discriminator.
	Code    string
	Message string
	// Title is the RFC 9457 title; empty means the status text.
	Title string
	// Extra holds sibling members written beside the error at the top of
	// the body, such as the zzrouter quota block. Clients ignore members
	// they do not know.
	Extra map[string]any
}

// AuthenticateChallenge is the RFC 7235 challenge carried by every 401.
// Bearer is the only one of the two accepted schemes that is nameable
// in this header; the X-API-Key alternative is advertised in the error
// detail instead.
const AuthenticateChallenge = `Bearer realm="zzrouter", charset="UTF-8"`

// SetAuthenticateChallenge attaches the challenge that a 401 response
// must carry. Shared by the dialect responders so all three advertise
// an identical challenge.
func SetAuthenticateChallenge(c *gin.Context) {
	c.Header("WWW-Authenticate", AuthenticateChallenge)
}
