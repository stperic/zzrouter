// Package openaicompat provides helpers for the /v1/* OpenAI-compatible
// surface. Distinct from pkg/httperr (which owns RFC 7807 problem+json for
// /zzrouter/v1/*): OpenAI error envelopes have a different shape and a
// closed `type` vocabulary that strict SDK clients switch on.
//
// When a backend (vLLM, llama.cpp, MLX, or a cloud provider) returns a non-2xx
// response, its error body is NOT passed through verbatim. Three reasons:
//
//  1. Security: raw backend messages often contain internal hostnames, file
//     paths, stack traces, and CUDA device strings — all of which would leak
//     infrastructure details to the client. This is sanitized via
//     utils.SanitizeErrorMessage, the same helper the RFC 7807 problem
//     responses use.
//
//  2. Closed vocabulary: the OpenAI error envelope's `type` field is a closed
//     set (invalid_request_error, authentication_error, permission_error,
//     not_found_error, rate_limit_error, server_error, api_error,
//     insufficient_quota, overloaded_error). Strict SDK clients (openai-python
//     et al.) switch on this to pick a typed exception class — anything else
//     silently falls through to the generic APIError. Even OpenAI-compatible
//     backends occasionally emit non-standard type values (older llama.cpp
//     builds, custom wrappers), so we repair those.
//
//  3. Shape: some backends — particularly cloud providers like Azure and
//     Gemini — emit their own envelope shapes that are not OpenAI-compatible
//     at all. Those get wrapped in a canonical envelope with the raw text
//     placed into the sanitized `message`.
//
// Backend-authored errors handled here are always classified as `api_error`
// for 5xx, never `server_error`. `server_error` is reserved for zzRouter-
// internal failures.
package openaicompat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/stperic/zzrouter/pkg/utils"
)

// closedErrorTypes is OpenAI's closed vocabulary for `error.type`.
// See https://platform.openai.com/docs/guides/error-codes/api-errors.
var closedErrorTypes = map[string]struct{}{
	"invalid_request_error": {},
	"authentication_error":  {},
	"permission_error":      {},
	"not_found_error":       {},
	"rate_limit_error":      {},
	"server_error":          {},
	"api_error":             {},
	"insufficient_quota":    {},
	"overloaded_error":      {},
}

// maxNormalizedMessageLen caps the length of wrapped backend error messages
// so a 50KB HTML error page doesn't become the OpenAI envelope's `message`.
const maxNormalizedMessageLen = 512

// StatusToErrorType maps an HTTP status code to the canonical OpenAI
// `error.type` value for a BACKEND-authored error. 5xx always maps to
// `api_error` here (not `server_error`) because the failure is upstream
// from zzRouter's perspective — `server_error` is reserved for zzRouter's
// own internal failures.
func StatusToErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	if status >= 500 {
		return "api_error"
	}
	if status >= 400 {
		return "invalid_request_error"
	}
	return "api_error"
}

// errorEnvelope is a loose parse target for whatever the backend returned.
// `Code` is `any` because some backends use strings ("model_not_found")
// while others use integers or null.
type errorEnvelope struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    any     `json:"code"`
	} `json:"error"`
}

// NormalizeError inspects a non-2xx backend response body and returns a
// normalized OpenAI-compatible error envelope as JSON bytes, ready to be
// written to the client as the response body. The caller is responsible for
// writing HTTP status and Content-Type: application/json.
//
// Three sub-cases:
//
//  1. Body parses as OpenAI envelope AND `type` is in the closed vocab →
//     sanitize the message and forward. Preserve `code` and `param`.
//  2. Body parses as OpenAI envelope BUT `type` is non-standard → move the
//     non-standard type into `code` (only if no code is already set),
//     rewrite `type` from HTTP status, sanitize the message.
//  3. Body does NOT parse as an OpenAI envelope → wrap it. The sanitized
//     raw body becomes `message`, `type` is derived from status, and
//     `code` is set to "backend_error".
func NormalizeError(statusCode int, body []byte) []byte {
	fallbackType := StatusToErrorType(statusCode)

	// Try to parse as OpenAI envelope. A body that unmarshals successfully
	// AND has a non-empty message or type is treated as an envelope.
	var env errorEnvelope
	if len(body) > 0 && json.Unmarshal(body, &env) == nil &&
		(env.Error.Message != "" || env.Error.Type != "") {
		msg := utils.SanitizeErrorMessage(env.Error.Message)

		// Code may be string, number, null, or absent. Only strings survive.
		var codePtr *string
		if s, ok := env.Error.Code.(string); ok && s != "" {
			codePtr = &s
		}

		// Gateway providers (OpenRouter et al.) reject tool requests with
		// 404 — sometimes with type:not_found_error, sometimes with no type
		// at all (just message + numeric code). Reclassify to OpenAI's
		// invalid_request_error + tool_use_failed regardless of the
		// upstream's vocab choice. Runs ahead of the closed-vocab forward
		// AND the type-repair Sub-case so both shapes converge.
		if statusCode == http.StatusNotFound && isToolUseRejection(msg) {
			code := "tool_use_failed"
			out, _ := json.Marshal(utils.NewOpenAIError("invalid_request_error", msg, env.Error.Param, &code))
			return out
		}

		if _, ok := closedErrorTypes[env.Error.Type]; ok {
			// Sub-case 1: closed vocab — sanitize and forward.
			// json.Marshal of a struct-typed envelope cannot fail; the
			// AlwaysValidJSON test pins this invariant.
			out, _ := json.Marshal(utils.NewOpenAIError(env.Error.Type, msg, env.Error.Param, codePtr))
			return out
		}

		// Sub-case 2: non-standard (or empty) type. Move the non-standard
		// type into code if no code was provided. If a code was already set,
		// the non-standard type is discarded — code carries higher signal.
		if codePtr == nil && env.Error.Type != "" {
			t := env.Error.Type
			codePtr = &t
		}
		out, _ := json.Marshal(utils.NewOpenAIError(fallbackType, msg, env.Error.Param, codePtr))
		return out
	}

	// Sub-case 3: not an OpenAI envelope. Sanitize the raw body as the
	// message. If the raw body is empty, synthesize a placeholder from the
	// status code so the client still sees something meaningful.
	msg := utils.SanitizeErrorMessage(string(body))
	if msg == "" {
		msg = fmt.Sprintf("backend returned status %d", statusCode)
	}
	if len(msg) > maxNormalizedMessageLen {
		msg = msg[:maxNormalizedMessageLen] + "..."
	}
	code := "backend_error"
	out, _ := json.Marshal(utils.NewOpenAIError(fallbackType, msg, nil, &code))
	return out
}

// Tight markers; non-tool 404s must not match.
func isToolUseRejection(msg string) bool {
	lower := strings.ToLower(msg)
	for _, marker := range []string{
		"support tool use",
		"support tool calling",
		"endpoints found that support tool",
		"tool calling is not supported",
		"tools are not supported",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
