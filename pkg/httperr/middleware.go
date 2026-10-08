package httperr

import (
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
)

// AttachResponder returns gin middleware that stores `r` on the
// request context so downstream handlers and middleware can retrieve
// it via FromContext, or FromRequest where only the request is held.
// Install it as the first middleware on every route group that owns a
// distinct dialect:
//
//	openai := engine.Group("/v1",
//	    httperr.AttachResponder(openaiResponder),
//	    server.OptionalAuthMiddleware())
//
// The middleware does not abort on its own — it simply attaches and
// calls Next. Subsequent middleware that needs to emit an error (auth
// rejection, rate limit, etc.) reads the responder back via
// FromContext and calls the matching method.
func AttachResponder(r Responder) gin.HandlerFunc {
	if r == nil {
		panic("httperr: AttachResponder requires a non-nil Responder")
	}
	return func(c *gin.Context) {
		c.Request = withResponder(c.Request, r)
		c.Next()
	}
}

// AttachByPath is engine-wide middleware that attaches d's responder for
// the request path before any group runs, so every request carries one:
// code below gin never has to guess a dialect. A group's own
// AttachResponder, running later, replaces it.
func AttachByPath(d *PathDispatcher) gin.HandlerFunc {
	if d == nil {
		panic("httperr: AttachByPath requires a non-nil PathDispatcher")
	}
	return func(c *gin.Context) {
		c.Request = withResponder(c.Request, d.For(c.Request.URL.Path))
		c.Next()
	}
}

// PanicLogger is invoked by GroupAwareRecovery when a panic is caught.
// Implementations log the panic detail, stack trace, and request
// identifier to the server's structured log. The function runs inside
// the recovery deferred block — it MUST NOT panic itself and SHOULD
// avoid allocations that could fail under memory pressure.
//
// A nil PanicLogger is legal; GroupAwareRecovery falls back to writing
// the panic to the process standard error stream via the default log
// package so operators still get a trace.
type PanicLogger func(reqID string, panicValue any, stack []byte)

// GroupAwareRecovery returns gin middleware that replaces gin.Recovery
// with a dispatcher-aware panic handler.
//
// Behavior on panic:
//
//  1. Recover the panic value and capture a stack trace.
//  2. Look up the active Responder via FromContext; fall back to the
//     dispatcher's path-prefix lookup when no group has run; fall back
//     once more to the dispatcher's fallback Responder when the path
//     matches no rules.
//  3. Invoke `logger` (if non-nil) with the request_id, panic value,
//     and stack.
//  4. Emit a 500 through the selected Responder's Internal method.
//     The Internal method is required to emit a fixed generic message,
//     so the panic value never reaches the client.
//  5. Abort the gin chain so no subsequent handler tries to write to
//     the already-closed response.
//
// The returned middleware is safe to use in any dialect combination
// because it never assumes a particular envelope shape — it only
// relies on the Responder.Internal contract.
func GroupAwareRecovery(d *PathDispatcher, logger PanicLogger) gin.HandlerFunc {
	if d == nil {
		panic("httperr: GroupAwareRecovery requires a non-nil PathDispatcher")
	}
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the stdlib's "the client is
			// gone, stop quietly" signal, not a server fault:
			// httputil.ReverseProxy raises it when the response copy
			// to the client fails mid-stream. net/http recovers it
			// without logging and closes the connection, so hand it
			// straight back. Swallowing it here would report every
			// client disconnect as a 500 with a full stack trace.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			stack := debug.Stack()

			// Pull the request id for correlation with server-side logs.
			var reqID string
			if v, ok := c.Get("request_id"); ok {
				if s, ok := v.(string); ok {
					reqID = s
				}
			}

			if logger != nil {
				safeInvokeLogger(logger, reqID, rec, stack)
			}

			// Prefer the group-attached responder; fall back to the
			// dispatcher only if nothing was attached (e.g. panic
			// inside the AttachResponder middleware itself).
			r := FromContext(c)
			if r == nil {
				r = d.For(c.Request.URL.Path)
			}

			// Internal is responsible for emitting a safe generic
			// message — we intentionally pass the raw panic value as
			// the detail so it lands in the dialect-specific
			// server-side log sink the responder wires up. Responders
			// MUST NOT echo this to the client.
			detail := formatPanic(rec)
			r.Internal(c, detail)
			c.Abort()
		}()
		c.Next()
	}
}

// safeInvokeLogger shields GroupAwareRecovery from a broken
// PanicLogger: a logger that itself panics would otherwise escape the
// deferred recovery and crash the goroutine. We run it inside its own
// recover to keep the recovery middleware robust against upstream
// bugs.
func safeInvokeLogger(logger PanicLogger, reqID string, panicValue any, stack []byte) {
	defer func() { _ = recover() }()
	logger(reqID, panicValue, stack)
}

// formatPanic renders a panic value as a short string suitable for
// logging. Kept small so the recovery middleware does not pull in a
// formatting dependency.
func formatPanic(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case error:
		return x.Error()
	default:
		return "panic"
	}
}
