// Package utils provides RFC 9457 Problem Details for HTTP APIs
// This package contains framework-agnostic data structures and utilities.
// For Gin-specific helpers, see internal/server/problem.go
package utils

import (
	"fmt"
	"regexp"
	"strings"
)

// ProblemDetails represents an RFC 9457 Problem Details response.
//
// It is the ONLY error envelope the management API emits. There used to
// be a second one for per-key validation - a bare {"errors": [...]} -
// justified by needing to report several independent key failures at
// once. This envelope already did that: a create with two missing
// fields comes back here with two entries. What the second envelope
// actually bought was a body no client could read, since the Go client
// understands this shape and the OpenAI one and nothing else.
//
// The Errors field is an RFC 9457 extension providing machine-readable
// validation details so API clients (especially AI agents) can react to
// per-key errors programmatically instead of parsing the human-readable
// Detail string.
//
// Code and Node are RFC 9457 extensions serving the same goal for failures
// that are not field-level: Code is a closed-enum discriminator (see
// httperr.RouteErrorCode) and Node names the cluster node a routed request
// targeted. Code is typed as a plain string so this package stays
// dependency-free; construction sites supply the typed constant.
type ProblemDetails struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail,omitempty"`
	Instance  string       `json:"instance,omitempty"`
	RequestID string       `json:"request_id,omitempty"`
	Code      string       `json:"code,omitempty"`
	Node      string       `json:"node,omitempty"`
	Errors    []ParamError `json:"errors,omitempty"`
}

// ParamError is one machine-readable per-key failure, and the only such
// shape on the management surface: a body field that failed validation,
// a provider parameter that failed its schema, and a failed install
// preflight check all arrive in this shape.
//
// Key names what failed - a JSON field name, a dotted parameter path, or
// a check name. Code is the closed-enum discriminator to branch on; it
// is typed as a plain string so this package stays dependency-free, and
// construction sites supply the httperr.ParamErrorCode constant, the
// same arrangement ProblemDetails.Code uses.
//
// Got/Want/Min/Max are the context that makes a failure actionable
// without parsing Message. Got is populated only where the offending
// value is safe to echo: body validation deliberately omits it, because
// the value that failed "required" or "email" may be a credential.
type ParamError struct {
	Key     string `json:"key,omitempty"`
	Code    string `json:"code"`
	Got     any    `json:"got,omitempty"`
	Want    string `json:"want,omitempty"`
	Min     any    `json:"min,omitempty"`
	Max     any    `json:"max,omitempty"`
	Message string `json:"message"`
	// Hint is what to do about it, when the failure has a known remedy
	// (e.g. "install curl"). Message says what is wrong; Hint says how to
	// make it right, and the two are separated so a caller can act on the
	// remedy without parsing the diagnosis.
	Hint string `json:"hint,omitempty"`
}

// NewProblemDetails creates a new RFC 9457 Problem Details error.
//
// Code is always populated, defaulting to a snake_case slug of the title
// ("Not Found" becomes "not_found"). Callers with something more specific
// to say overwrite it with a typed constant. Every management error
// carrying a code is what lets a client branch on one field instead of
// parsing Detail, and a code that is only sometimes there is a field
// nobody can branch on.
//
// The slug is snake_case to match the closed enums that override it
// (httperr.RouteErrorCode, httperr.ParamErrorCode); Type stays kebab-case
// because it is a URI.
func NewProblemDetails(status int, title, detail, instance string) *ProblemDetails {
	typeSlug := strings.ToLower(strings.ReplaceAll(title, " ", "-"))

	return &ProblemDetails{
		Type:     fmt.Sprintf("https://api.zzrouter.com/problems/%s", typeSlug),
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: instance,
		Code:     strings.ReplaceAll(typeSlug, "-", "_"),
	}
}

// NOTE: Gin-specific response helpers have been moved to internal/server/problem.go
// This keeps pkg/utils framework-agnostic and reduces client binary size.

// Pre-compiled regexes for error message sanitization (compiled once at package init)
// Order matters when applying: more specific patterns first
var (
	// Go stack traces (goroutine, .go:line)
	sanitizeStackTraceRe = regexp.MustCompile(`goroutine \d+ \[[^\]]+\]:[\s\S]*?\.go:\d+`)

	// Qualified module paths are unambiguous anywhere they appear.
	sanitizeVCSPathRe = regexp.MustCompile(`github\.com/[a-zA-Z0-9_/.@-]+`)

	// Bare `internal/...` and `pkg/...` are NOT unambiguous: this API
	// serves /zzrouter/v1/internal/*, and matching them anywhere rewrote a
	// peer's own route as "[module]", leaving operators unable to tell
	// which endpoint had failed.
	//
	// What separates the two is the character to the left. A URL path
	// segment is always preceded by "/"; a Go import path never is. RE2
	// has no lookbehind, so group 1 captures that boundary and the
	// replacement puts it back — see sanitizeModulePath.
	sanitizePkgPathRe = regexp.MustCompile(`(^|[^/a-zA-Z0-9_.@-])((?:internal|pkg)/[a-zA-Z0-9_/.-]+(?::\d+)?)`)

	// Windows absolute paths: C:\Users\..., D:\Program Files\..., etc.
	sanitizeWinPathRe = regexp.MustCompile(`[A-Za-z]:\\[^\s:]+`)

	// Home directory references
	sanitizeHomePathRe = regexp.MustCompile(`~[/\\][^\s]+`)

	// Unix absolute paths rooted at a known leak-prefix directory.
	//
	// Previously we matched "any path with ≥3 components" which was a
	// good leak filter but a false positive for legitimate URL paths
	// (e.g. `/v1/chat/completions` got stripped when it appeared in
	// an OpenAI NotFound detail). Requiring the path to start with
	// one of the common system/user/home directories eliminates the
	// false positives while still catching every filesystem leak we
	// actually care about.
	//
	// The list mirrors the directories that appear in Go error text
	// and stack traces: user homes on macOS (`/Users/...`) and Linux
	// (`/home/...`, `/root/...`), system storage (`/var/...`,
	// `/tmp/...`, `/opt/...`, `/usr/...`, `/etc/...`), common
	// container/host mounts (`/mnt/...`, `/srv/...`, `/data/...`),
	// and macOS's `/private/...` real paths.
	sanitizeUnixPathRe = regexp.MustCompile(`/(home|Users|var|tmp|opt|private|etc|root|usr|mnt|srv|data)(/[a-zA-Z0-9_.-]+)+`)

	// Collapse multiple consecutive markers
	sanitizeMultiPathRe   = regexp.MustCompile(`(\[path\]\s*)+`)
	sanitizeMultiModuleRe = regexp.MustCompile(`(\[module\]\s*)+`)
)

// SanitizeErrorMessage removes sensitive information from error messages
// before including them in API responses. This prevents information disclosure
// of file paths, internal structure, and other sensitive details.
//
// SECURITY: This function scrubs:
// - Absolute file paths (Unix and Windows)
// - Home directory paths
// - Internal package/module paths
// - Connection strings with credentials
// - Stack traces
func SanitizeErrorMessage(errMsg string) string {
	if errMsg == "" {
		return errMsg
	}

	// Apply sanitization in order (more specific patterns first)
	result := errMsg
	result = sanitizeStackTraceRe.ReplaceAllString(result, "[internal error]")
	result = sanitizeVCSPathRe.ReplaceAllString(result, "[module]")
	result = sanitizePkgPathRe.ReplaceAllString(result, "${1}[module]")
	result = sanitizeWinPathRe.ReplaceAllString(result, "[path]")
	result = sanitizeHomePathRe.ReplaceAllString(result, "[path]")
	result = sanitizeUnixPathRe.ReplaceAllString(result, "[path]")

	// Collapse multiple consecutive [path] or [module] markers
	result = sanitizeMultiPathRe.ReplaceAllString(result, "[path] ")
	result = sanitizeMultiModuleRe.ReplaceAllString(result, "[module] ")

	// Clean up whitespace
	result = strings.TrimSpace(result)

	return result
}

// OpenAIError represents an OpenAI-compatible error response
type OpenAIError struct {
	Error OpenAIErrorDetail `json:"error"`
}

// OpenAIErrorDetail contains the error details in OpenAI format
type OpenAIErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param,omitempty"`
	Code    *string `json:"code,omitempty"`
}

// NewOpenAIError creates an OpenAI-compatible error response
func NewOpenAIError(errorType, message string, param *string, code *string) *OpenAIError {
	return &OpenAIError{
		Error: OpenAIErrorDetail{
			Message: message,
			Type:    errorType,
			Param:   param,
			Code:    code,
		},
	}
}

// NewOpenAIInvalidRequest creates an invalid request error.
//
// Per the OpenAI spec, `type` names the error family and `code` is a
// machine-readable sub-code like "model_not_found" or "context_length_exceeded"
// that SDK retry/recovery logic branches on. This helper leaves `code` nil
// because the caller typically doesn't know a specific sub-code; callers that
// do (e.g. a "model not found" path) should build the error via NewOpenAIError
// directly and pass the specific code.
func NewOpenAIInvalidRequest(message string, param *string) *OpenAIError {
	return NewOpenAIError("invalid_request_error", message, param, nil)
}

// NewOpenAINodeError creates a server-side error. `code` is left nil because
// the spec reserves it for specific machine-readable sub-codes. See the
// comment on NewOpenAIInvalidRequest for the rationale.
func NewOpenAINodeError(message string) *OpenAIError {
	return NewOpenAIError("server_error", message, nil, nil)
}

// NOTE: Gin-specific response helpers have been moved to internal/server/problem.go
