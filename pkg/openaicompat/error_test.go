package openaicompat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeNormalized unmarshals a NormalizeError result so tests can assert on
// the individual envelope fields instead of brittle string matching.
func decodeNormalized(t *testing.T, b []byte) struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	} `json:"error"`
} {
	t.Helper()
	var out struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Param   *string `json:"param"`
			Code    *string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(b, &out), "normalized output must be valid JSON")
	return out
}

func TestStatusToErrorType(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusForbidden, "permission_error"},
		{http.StatusNotFound, "not_found_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusInternalServerError, "api_error"},
		{http.StatusBadGateway, "api_error"},
		{http.StatusServiceUnavailable, "api_error"},
		{http.StatusGatewayTimeout, "api_error"},
		{422, "invalid_request_error"}, // other 4xx fall through to invalid_request_error
		{200, "api_error"},             // shouldn't be called with 2xx but has a safe default
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			assert.Equal(t, tt.want, StatusToErrorType(tt.status))
		})
	}
}

func TestNormalizeError_ClosedVocabForwarded(t *testing.T) {
	// Sub-case 1: backend returned a well-formed OpenAI envelope with a
	// closed-vocab type. Should be forwarded with message sanitized and
	// code/type preserved.
	body := []byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	out := NormalizeError(http.StatusTooManyRequests, body)
	got := decodeNormalized(t, out)

	assert.Equal(t, "rate_limit_error", got.Error.Type, "closed-vocab type must be preserved")
	assert.Equal(t, "rate limit exceeded", got.Error.Message)
	require.NotNil(t, got.Error.Code)
	assert.Equal(t, "rate_limit_exceeded", *got.Error.Code, "code must be preserved")
}

// TestNormalizeError_ToolUseRejectionReclassified — OpenRouter and similar
// gateway providers respond with 404 not_found_error when the routed
// backend doesn't support tool calling. OpenAI's actual semantic for this
// is 400 invalid_request_error + code:tool_use_failed. The normalizer
// reclassifies based on a tight message-substring heuristic so strict
// clients see the correct error class.
func TestNormalizeError_ToolUseRejectionReclassified(t *testing.T) {
	body := []byte(`{"error":{"message":"No endpoints found that support tool use. Try disabling \"calc\".","type":"not_found_error"}}`)
	out := NormalizeError(http.StatusNotFound, body)
	got := decodeNormalized(t, out)

	assert.Equal(t, "invalid_request_error", got.Error.Type, "tool-use rejection must reclassify type")
	require.NotNil(t, got.Error.Code)
	assert.Equal(t, "tool_use_failed", *got.Error.Code, "code must be tool_use_failed")
	assert.Contains(t, got.Error.Message, "support tool use", "message must be preserved")
}

// TestNormalizeError_ToolUseRejectionNoTypeField — OpenRouter sometimes
// emits the 404 envelope without a `type` field at all (just message +
// numeric code:404). Heuristic must trigger regardless of upstream's
// vocab choice when status is 404 + tool-use language.
func TestNormalizeError_ToolUseRejectionNoTypeField(t *testing.T) {
	body := []byte(`{"error":{"message":"No endpoints found that support tool use. Try disabling \"calc\".","code":404}}`)
	out := NormalizeError(http.StatusNotFound, body)
	got := decodeNormalized(t, out)

	assert.Equal(t, "invalid_request_error", got.Error.Type, "tool-use rejection without type must reclassify")
	require.NotNil(t, got.Error.Code)
	assert.Equal(t, "tool_use_failed", *got.Error.Code, "code must be tool_use_failed")
}

// TestNormalizeError_NonToolUse404PreservesType — a real 404 unrelated to
// tools (e.g. genuine model_not_found) must not get reclassified.
func TestNormalizeError_NonToolUse404PreservesType(t *testing.T) {
	body := []byte(`{"error":{"message":"the model 'foo-bar' does not exist","type":"not_found_error","code":"model_not_found"}}`)
	out := NormalizeError(http.StatusNotFound, body)
	got := decodeNormalized(t, out)

	assert.Equal(t, "not_found_error", got.Error.Type, "non-tool-use 404 must preserve type")
	require.NotNil(t, got.Error.Code)
	assert.Equal(t, "model_not_found", *got.Error.Code)
}

// TestNormalizeError_ToolUseLanguageOn5xxNotReclassified — gate is
// not_found_error-typed bodies only; a 5xx that happens to mention tools
// must not pull the trigger. Pins the gate against future broadening.
func TestNormalizeError_ToolUseLanguageOn5xxNotReclassified(t *testing.T) {
	body := []byte(`{"error":{"message":"upstream crashed: tools are not supported","type":"api_error"}}`)
	out := NormalizeError(http.StatusBadGateway, body)
	got := decodeNormalized(t, out)

	assert.Equal(t, "api_error", got.Error.Type, "5xx must not be reclassified")
}

// TestNormalizeError_ToolUseLanguageInNonEnvelope — heuristic must be
// gated to Sub-case 1 (envelope with closed-vocab type). A raw body
// (Sub-case 3) carrying the marker phrase falls through to backend_error
// wrap, not the tool_use_failed reclassification.
func TestNormalizeError_ToolUseLanguageInNonEnvelope(t *testing.T) {
	body := []byte("502 Bad Gateway: tools are not supported")
	out := NormalizeError(http.StatusBadGateway, body)
	got := decodeNormalized(t, out)

	assert.Equal(t, "api_error", got.Error.Type, "non-envelope body must not trigger reclassification")
	require.NotNil(t, got.Error.Code)
	assert.Equal(t, "backend_error", *got.Error.Code)
}

func TestNormalizeError_NonStandardTypeRepaired(t *testing.T) {
	// Sub-case 2: backend returned an OpenAI-shape envelope but with a
	// non-standard type ("backend_error" is not in the closed vocab).
	// The non-standard type should be moved into code, and type rewritten
	// from the HTTP status.
	body := []byte(`{"error":{"message":"CUDA OOM on device 1","type":"backend_error"}}`)
	out := NormalizeError(http.StatusInternalServerError, body)
	got := decodeNormalized(t, out)

	assert.Equal(t, "api_error", got.Error.Type, "5xx → api_error")
	assert.Equal(t, "CUDA OOM on device 1", got.Error.Message)
	require.NotNil(t, got.Error.Code)
	assert.Equal(t, "backend_error", *got.Error.Code, "non-standard type should be moved to code")
}

func TestNormalizeError_NonStandardTypeCodeAlreadySet(t *testing.T) {
	// Edge case under sub-case 2: when both type is non-standard AND code is
	// already set, code wins. The non-standard type string is discarded.
	body := []byte(`{"error":{"message":"ctx too long","type":"some_custom_type","code":"context_length_exceeded"}}`)
	out := NormalizeError(http.StatusBadRequest, body)
	got := decodeNormalized(t, out)

	assert.Equal(t, "invalid_request_error", got.Error.Type)
	require.NotNil(t, got.Error.Code)
	assert.Equal(t, "context_length_exceeded", *got.Error.Code, "existing code must win over non-standard type")
}

func TestNormalizeError_NonOpenAIBodyWrapped(t *testing.T) {
	// Sub-case 3: backend returned something that isn't an OpenAI envelope
	// at all — e.g. a plain text error from nginx, an HTML page from a
	// misconfigured proxy, or a cloud provider with a native shape.
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"plain text", http.StatusBadGateway, "upstream connect error"},
		{"html page", http.StatusServiceUnavailable, "<!doctype html><title>503</title>"},
		{"cloud-native shape", http.StatusForbidden, `{"errorCode":"AUTH_FAILED","details":"bad key"}`},
		{"empty body", http.StatusInternalServerError, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := NormalizeError(tt.status, []byte(tt.body))
			got := decodeNormalized(t, out)

			// Type must be derived from status.
			assert.Equal(t, StatusToErrorType(tt.status), got.Error.Type)
			// Wrapped errors always get the synthetic "backend_error" code.
			require.NotNil(t, got.Error.Code)
			assert.Equal(t, "backend_error", *got.Error.Code)
			// Message must be non-empty (synthesized from status if body was empty).
			assert.NotEmpty(t, got.Error.Message)
		})
	}
}

func TestNormalizeError_MessageSanitization(t *testing.T) {
	// Sub-case 1 + sanitization: message contains a file path that must
	// be scrubbed by utils.SanitizeErrorMessage before reaching the client.
	body := []byte(`{"error":{"message":"failed to open /home/user/models/llama.gguf: permission denied","type":"invalid_request_error"}}`)
	out := NormalizeError(http.StatusBadRequest, body)
	got := decodeNormalized(t, out)

	assert.NotContains(t, got.Error.Message, "/home/user",
		"file path must be stripped by SanitizeErrorMessage")
	assert.Contains(t, got.Error.Message, "permission denied",
		"sanitization should only scrub paths, not the whole message")
}

func TestNormalizeError_LongBodyTruncated(t *testing.T) {
	// Sub-case 3 with a very large raw body (think: HTML error page from a
	// misconfigured reverse proxy). Message must be truncated to keep the
	// envelope bounded.
	long := strings.Repeat("x", 2000)
	out := NormalizeError(http.StatusBadGateway, []byte(long))
	got := decodeNormalized(t, out)

	assert.True(t, len(got.Error.Message) <= maxNormalizedMessageLen+3, // +3 for trailing "..."
		"message must be truncated, got %d chars", len(got.Error.Message))
	assert.True(t, strings.HasSuffix(got.Error.Message, "..."),
		"truncated messages end with ellipsis")
}

func TestNormalizeError_CodeAsNonString(t *testing.T) {
	// Defensive: some backends put an integer or null in `code`. Our loose
	// parser should ignore non-string codes rather than crash.
	tests := []struct {
		name string
		body string
	}{
		{"integer code", `{"error":{"message":"msg","type":"api_error","code":42}}`},
		{"null code", `{"error":{"message":"msg","type":"api_error","code":null}}`},
		{"object code", `{"error":{"message":"msg","type":"api_error","code":{"x":1}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := NormalizeError(http.StatusInternalServerError, []byte(tt.body))
			got := decodeNormalized(t, out)
			// Should parse, preserve closed-vocab type, leave code nil.
			assert.Equal(t, "api_error", got.Error.Type)
			assert.Equal(t, "msg", got.Error.Message)
			assert.Nil(t, got.Error.Code, "non-string code values must be discarded, not crashed on")
		})
	}
}

func TestNormalizeError_NonStandardTypePromotedWhenCodeIsNonString(t *testing.T) {
	// Interaction: non-standard type AND non-string code. The non-string
	// code is discarded (treated as nil), so the non-standard type must
	// still be promoted into code. Locks in sub-case 2's behaviour under
	// the junk-code defence from TestNormalizeError_CodeAsNonString.
	body := []byte(`{"error":{"message":"ctx too long","type":"some_custom_type","code":42}}`)
	out := NormalizeError(http.StatusBadRequest, body)
	got := decodeNormalized(t, out)

	assert.Equal(t, "invalid_request_error", got.Error.Type, "type rewritten from status")
	require.NotNil(t, got.Error.Code, "non-standard type must be promoted to code when code was junk")
	assert.Equal(t, "some_custom_type", *got.Error.Code)
}

func TestNormalizeError_EnvelopeWithOnlyType(t *testing.T) {
	// Edge case: backend returned an envelope with a type but no message.
	// Should still be recognized as an OpenAI envelope (not wrapped).
	body := []byte(`{"error":{"type":"authentication_error"}}`)
	out := NormalizeError(http.StatusUnauthorized, body)
	got := decodeNormalized(t, out)

	assert.Equal(t, "authentication_error", got.Error.Type)
	// Code should be nil (not "backend_error" — that's only for sub-case 3).
	assert.Nil(t, got.Error.Code)
}

func TestNormalizeError_AlwaysValidJSON(t *testing.T) {
	// Invariant: no matter what garbage the backend sends, the output is
	// always valid JSON that unmarshals into the OpenAI envelope shape.
	garbage := [][]byte{
		nil,
		{},
		[]byte("\x00\x01\x02"),
		[]byte("{broken json"),
		[]byte(`{"error":null}`),
		[]byte(`[]`),
	}
	for i, body := range garbage {
		out := NormalizeError(http.StatusBadGateway, body)
		var envelope struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(out, &envelope), "garbage input %d must produce valid JSON", i)
		assert.NotEmpty(t, envelope.Error.Type, "type must always be set")
		assert.NotEmpty(t, envelope.Error.Message, "message must always be set")
	}
}
