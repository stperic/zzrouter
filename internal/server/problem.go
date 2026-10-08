// Package server provides Gin-specific HTTP error response helpers
// These helpers wrap the framework-agnostic utilities from pkg/utils
package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// RespondWithProblem sends an RFC 9457 Problem Details response.
// Includes request_id from the gin context if available (set by RequestIDMiddleware).
func RespondWithProblem(c *gin.Context, status int, title, detail string) {
	problem := utils.NewProblemDetails(status, title, detail, c.Request.URL.Path)
	if reqID, exists := c.Get("request_id"); exists {
		if id, ok := reqID.(string); ok {
			problem.RequestID = id
		}
	}
	c.Header("Content-Type", "application/problem+json")
	c.JSON(status, problem)
}

// respondSurfaceError emits an error in the dialect of the request's
// route group. A management route attaches none and answers
// problem+json.
//
// Middleware is the reason this exists. A handler knows its own surface
// and can call the right responder directly, but a middleware mounted
// across several surfaces cannot: the idempotency store is wired onto
// /v1/* (OpenAI), /zzrouter/v1/runs and /zzrouter/v1/model-groups
// (admin) from three separate call sites. Any single hard-coded shape
// it emits is therefore wrong somewhere, and it was — one branch
// answered problem+json and the other the OpenAI envelope, so each was
// unreadable on some surface it actually served.
func respondSurfaceError(c *gin.Context, e httperr.Error) {
	httperr.FromContextOr(c, defaultAuthResponder).WriteError(c.Writer, c.Request, e)
}

// ProblemOpts carries the optional RFC 9457 extension members.
//
// A struct rather than more string parameters: code and node are both
// short lowercase strings, so a positional signature would let them be
// transposed silently and the wrong field would ship.
type ProblemOpts struct {
	// Code is the machine-actionable discriminator a client branches on.
	// Prefer a closed-enum value (httperr.RouteErrorCode / ParamErrorCode)
	// over an ad-hoc string.
	Code string
	// Node names the cluster node the failure is attributed to, for
	// errors that are specific to one peer rather than the cluster.
	Node string
	// Errors carries per-key detail: one entry per input that failed,
	// so a caller fixing three fields learns about all three from one
	// response. This is where the retired second envelope's payload
	// lives now.
	Errors []utils.ParamError
}

// RespondWithProblemOpts is RespondWithProblem plus the extension
// members. It exists so a handler with real diagnostic context (which
// node, which error code) can keep it inside the problem envelope
// instead of inventing a sibling JSON shape to carry it.
func RespondWithProblemOpts(c *gin.Context, status int, title, detail string, opts ProblemOpts) {
	problem := utils.NewProblemDetails(status, title, detail, c.Request.URL.Path)
	if reqID, exists := c.Get("request_id"); exists {
		if id, ok := reqID.(string); ok {
			problem.RequestID = id
		}
	}
	// An empty opts.Code leaves the status-derived default in place
	// rather than blanking a field clients branch on.
	if opts.Code != "" {
		problem.Code = opts.Code
	}
	problem.Node = opts.Node
	problem.Errors = opts.Errors
	c.Header("Content-Type", "application/problem+json")
	c.JSON(status, problem)
}

// RespondWithParamErrors is the per-key failure path: one entry per
// input that failed, inside the problem envelope. Detail is a join of
// the messages, so a person reading one log line sees what happened
// without the array.
func RespondWithParamErrors(c *gin.Context, status int, title string, errs []utils.ParamError) {
	details := make([]string, 0, len(errs))
	for _, e := range errs {
		details = append(details, e.Message)
	}
	RespondWithParamErrorsDetail(c, status, title, strings.Join(details, "; "), errs)
}

// RespondWithParamErrorsDetail is RespondWithParamErrors with the
// summary supplied, for paths that phrase it better than a join.
//
// The top-level code repeats the per-key code when every entry agrees,
// which is the common case and saves a caller from walking the array to
// learn what kind of failure this was. When the entries disagree it
// stays the status slug: no single code is true, and picking the first
// one would answer a question the body does not support.
func RespondWithParamErrorsDetail(c *gin.Context, status int, title, detail string, errs []utils.ParamError) {
	if len(errs) == 0 {
		RespondWithProblem(c, status, title, detail)
		return
	}
	RespondWithProblemOpts(c, status, title, detail,
		ProblemOpts{Code: sharedParamCode(errs), Errors: errs})
}

// sharedParamCode returns the code every entry carries, or "" when they
// differ, which leaves NewProblemDetails' status-derived default in
// place.
func sharedParamCode(errs []utils.ParamError) string {
	first := errs[0].Code
	for _, e := range errs[1:] {
		if e.Code != first {
			return ""
		}
	}
	return first
}

// Common problem detail constructors
func BadRequest(c *gin.Context, detail string) {
	RespondWithProblem(c, http.StatusBadRequest, "Bad Request", detail)
}

func Unauthorized(c *gin.Context, detail string) {
	httperr.SetAuthenticateChallenge(c)
	RespondWithProblem(c, http.StatusUnauthorized, "Unauthorized", detail)
}

func Forbidden(c *gin.Context, detail string) {
	RespondWithProblem(c, http.StatusForbidden, "Forbidden", detail)
}

func NotFound(c *gin.Context, detail string) {
	RespondWithProblem(c, http.StatusNotFound, "Not Found", detail)
}

func Conflict(c *gin.Context, detail string) {
	RespondWithProblem(c, http.StatusConflict, "Conflict", detail)
}

func InternalNodeError(c *gin.Context, detail string) {
	// SECURITY: Sanitize error messages to prevent information disclosure
	sanitized := utils.SanitizeErrorMessage(detail)
	RespondWithProblem(c, http.StatusInternalServerError, "Internal Server Error", sanitized)
}

func ServiceUnavailable(c *gin.Context, detail string) {
	// SECURITY: Sanitize error messages to prevent information disclosure
	sanitized := utils.SanitizeErrorMessage(detail)
	RespondWithProblem(c, http.StatusServiceUnavailable, "Service Unavailable", sanitized)
}

func RequestTimeout(c *gin.Context, detail string) {
	RespondWithProblem(c, http.StatusRequestTimeout, "Request Timeout", detail)
}

func NotImplemented(c *gin.Context, detail string) {
	RespondWithProblem(c, http.StatusNotImplemented, "Not Implemented", detail)
}

func BadGateway(c *gin.Context, detail string) {
	// SECURITY: Sanitize error messages to prevent information disclosure
	sanitized := utils.SanitizeErrorMessage(detail)
	RespondWithProblem(c, http.StatusBadGateway, "Bad Gateway", sanitized)
}

// RespondToError writes a Problem Details response that preserves the original
// status code and structure of routed errors, falling back to 500 Internal
// Server Error for unrecognized errors.
//
// This is the preferred way for public handlers to propagate errors returned
// from service/executor calls. It eliminates the old pattern of calling
// err.Error() and passing the stringified (and possibly already-escaped-JSON)
// result to RespondWithProblem, which produced nested errors.
//
// When err is a *RoutedError with an attached Problem Details, the inner
// Problem Details is forwarded verbatim (with a fresh instance and request_id
// reflecting the current hop). This lets clients see the real underlying cause
// with its original status code (e.g. a 501 stays 501, not a wrapped 500).
func RespondToError(c *gin.Context, err error) {
	if err == nil {
		return
	}
	var re *RoutedError
	if errors.As(err, &re) {
		re.Respond(c)
		return
	}
	InternalNodeError(c, err.Error())
}

// RespondToLookupError is RespondToError for handlers that fetch one named
// resource from a node.
//
// Those handlers cannot use a bare NotFound: when the node is unreachable
// the service returns a *RoutedError, and flattening it to 404 tells the
// caller "no such provider" for a provider that exists on a node that
// merely timed out. An agent retrying elsewhere on that signal gives up on
// the wrong thing. A routed failure therefore keeps its own status (502
// with an upstream_timeout code); everything else is a genuine 404.
func RespondToLookupError(c *gin.Context, err error) {
	if err == nil {
		return
	}
	var re *RoutedError
	if errors.As(err, &re) {
		re.Respond(c)
		return
	}
	NotFound(c, err.Error())
}

// OpenAI-compatible error response helpers

// OpenAIInvalidRequest responds with an invalid request error
func OpenAIInvalidRequest(c *gin.Context, message string, param *string) {
	c.JSON(http.StatusBadRequest, utils.NewOpenAIInvalidRequest(message, param))
}

// OpenAINodeError responds with a server error
func OpenAINodeError(c *gin.Context, message string) {
	c.JSON(http.StatusInternalServerError, utils.NewOpenAINodeError(message))
}

// Ollama-compatible error helpers. The Ollama protocol uses
// {"error": "<message>"} on the wire.

// OllamaError emits {"error": "..."} at the given status.
func OllamaError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"error": message})
}

// RespondToErrorOllama translates err to an Ollama wire response,
// preserving the status code from *RoutedError and falling back to 500.
func RespondToErrorOllama(c *gin.Context, err error) {
	if err == nil {
		return
	}
	var re *RoutedError
	if errors.As(err, &re) {
		OllamaError(c, re.StatusCode, re.Error())
		return
	}
	OllamaError(c, http.StatusInternalServerError, err.Error())
}
