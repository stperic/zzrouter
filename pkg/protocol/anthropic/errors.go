// Package anthropic is the Anthropic Messages surface (/v1/messages):
// its error envelope and the Responder that emits it, and the
// translation to and from Chat Completions for a provider whose engine
// does not speak the Messages API (messages_compat). To a provider that
// does, requests and replies pass through untouched.
package anthropic

import (
	"encoding/json"
	"net/http"

	"github.com/stperic/zzrouter/pkg/openaicompat"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Error types from the Anthropic API's closed vocabulary. Claude Code
// and the SDKs decide retries by status, so the type mostly informs
// humans; it is derived from the status unless the backend sent a valid
// one.
const (
	ErrorTypeInvalidRequest  = "invalid_request_error"
	ErrorTypeAuthentication  = "authentication_error"
	ErrorTypeBilling         = "billing_error"
	ErrorTypePermission      = "permission_error"
	ErrorTypeNotFound        = "not_found_error"
	ErrorTypeRequestTooLarge = "request_too_large"
	ErrorTypeRateLimit       = "rate_limit_error"
	ErrorTypeAPI             = "api_error"
	ErrorTypeTimeout         = "timeout_error"
	ErrorTypeOverloaded      = "overloaded_error"
)

var errorTypes = map[string]struct{}{
	ErrorTypeInvalidRequest: {}, ErrorTypeAuthentication: {}, ErrorTypeBilling: {},
	ErrorTypePermission: {}, ErrorTypeNotFound: {}, ErrorTypeRequestTooLarge: {},
	ErrorTypeRateLimit: {}, ErrorTypeAPI: {}, ErrorTypeTimeout: {}, ErrorTypeOverloaded: {},
}

// statusOverloaded is Anthropic's non-standard "overloaded" status.
const statusOverloaded = 529

// ErrorTypeForStatus maps an HTTP status onto the Anthropic vocabulary.
func ErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return ErrorTypeAuthentication
	case http.StatusPaymentRequired:
		return ErrorTypeBilling
	case http.StatusForbidden:
		return ErrorTypePermission
	case http.StatusNotFound:
		return ErrorTypeNotFound
	case http.StatusRequestEntityTooLarge:
		return ErrorTypeRequestTooLarge
	case http.StatusTooManyRequests:
		return ErrorTypeRateLimit
	case http.StatusGatewayTimeout:
		return ErrorTypeTimeout
	case statusOverloaded:
		return ErrorTypeOverloaded
	}
	if status >= http.StatusInternalServerError {
		return ErrorTypeAPI
	}
	return ErrorTypeInvalidRequest
}

// Envelope renders {"type":"error","error":{"type":...,"message":...}}.
// The type always comes from the status: zzRouter's internal error types
// are OpenAI vocabulary and mean nothing to an Anthropic client.
func Envelope(status int, message string) map[string]any {
	return envelope(ErrorTypeForStatus(status), message)
}

func envelope(errType, message string) map[string]any {
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": utils.SanitizeErrorMessage(message)},
	}
}

// IsErrorEnvelope reports whether raw is already an Anthropic error
// envelope, such as one zzRouter wrote itself, which a caller passes on
// untouched rather than rebuild.
func IsErrorEnvelope(raw []byte) bool {
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal(raw, &e) == nil && e.Type == "error" && e.Error.Message != ""
}

// NormalizeError rewrites a backend's error body into the Anthropic
// envelope. A body already in that shape keeps its type when the type is
// in the vocabulary; anything else (OpenAI envelopes from llama-server
// and vLLM, flat Ollama errors, plain text) keeps its message and takes
// its type from the status.
func NormalizeError(status int, raw []byte) []byte {
	var native struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &native) == nil && native.Type == "error" && native.Error.Message != "" {
		errType := native.Error.Type
		if _, ok := errorTypes[errType]; !ok {
			errType = ErrorTypeForStatus(status)
		}
		out, _ := json.Marshal(envelope(errType, native.Error.Message))
		return out
	}

	// openaicompat already extracts a message from every other shape a
	// backend emits; reuse that rather than parse them all twice.
	var oai struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(openaicompat.NormalizeError(status, raw), &oai)
	out, _ := json.Marshal(Envelope(status, oai.Error.Message))
	return out
}
