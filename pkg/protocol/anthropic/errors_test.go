package anthropic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
)

type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func decode(t *testing.T, body []byte) anthropicError {
	t.Helper()
	var e anthropicError
	require.NoError(t, json.Unmarshal(body, &e), "body: %s", body)
	require.Equal(t, "error", e.Type, "body: %s", body)
	return e
}

// llama-server answers /v1/messages failures in OpenAI's shape. The
// fixture is a verbatim capture (2026-10-04, chat template rejecting an
// empty conversation).
func TestNormalizeError_OpenAIShapedBackendBecomesAnthropic(t *testing.T) {
	raw, err := os.ReadFile("testdata/llamacpp_messages_error_500.json")
	require.NoError(t, err)

	e := decode(t, NormalizeError(http.StatusInternalServerError, raw))
	assert.Equal(t, ErrorTypeAPI, e.Error.Type)
	assert.Contains(t, e.Error.Message, "No messages provided.")
}

func TestNormalizeError_AnthropicBodyKeepsItsType(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	e := decode(t, NormalizeError(statusOverloaded, raw))
	assert.Equal(t, ErrorTypeOverloaded, e.Error.Type)
	assert.Equal(t, "Overloaded", e.Error.Message)
}

func TestNormalizeError_UnknownTypeFallsBackToStatus(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"type":"made_up","message":"nope"}}`)
	assert.Equal(t, ErrorTypeNotFound, decode(t, NormalizeError(http.StatusNotFound, raw)).Error.Type)
}

func TestNormalizeError_PlainTextAndEmptyBodies(t *testing.T) {
	e := decode(t, NormalizeError(http.StatusBadGateway, []byte("upstream connect error")))
	assert.Equal(t, ErrorTypeAPI, e.Error.Type)
	assert.Equal(t, "upstream connect error", e.Error.Message)

	e = decode(t, NormalizeError(http.StatusServiceUnavailable, nil))
	assert.Equal(t, ErrorTypeAPI, e.Error.Type)
	assert.NotEmpty(t, e.Error.Message)
}

func TestErrorTypeForStatus(t *testing.T) {
	for status, want := range map[int]string{
		400: ErrorTypeInvalidRequest, 401: ErrorTypeAuthentication, 402: ErrorTypeBilling,
		403: ErrorTypePermission, 404: ErrorTypeNotFound, 413: ErrorTypeRequestTooLarge,
		422: ErrorTypeInvalidRequest, 429: ErrorTypeRateLimit, 500: ErrorTypeAPI,
		502: ErrorTypeAPI, 503: ErrorTypeAPI, 504: ErrorTypeTimeout, 529: ErrorTypeOverloaded,
	} {
		assert.Equal(t, want, ErrorTypeForStatus(status), "status %d", status)
	}
}

func TestResponder_WritesAnthropicEnvelopeAndAborts(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	New(nil).TooManyRequests(c, "slow down", 7)

	assert.True(t, c.IsAborted())
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "7", w.Header().Get("Retry-After"))
	e := decode(t, w.Body.Bytes())
	assert.Equal(t, ErrorTypeRateLimit, e.Error.Type)
	assert.Equal(t, "slow down", e.Error.Message)
}

func TestResponder_InternalNeverEchoesDetail(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	var logged string

	New(func(_, detail string) { logged = detail }).Internal(c, "panic: nil map at server.go:12")

	assert.Equal(t, "panic: nil map at server.go:12", logged)
	assert.NotContains(t, w.Body.String(), "nil map")
}

// The Anthropic SDKs retry a 429 or 529 themselves; a failure inside a
// 200 stream would defeat that.
func TestResponder_DoesNotStreamFailuresInBand(t *testing.T) {
	_, inBand := httperr.Responder(New(nil)).(httperr.InBandStreamer)
	assert.False(t, inBand)
}

// Extra rides beside the envelope; it cannot replace it.
func TestResponder_ExtraCannotOverwriteTheEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "req-123")

	New(nil).WriteError(w, nil, httperr.Error{Status: http.StatusBadRequest, Message: "bad",
		Extra: map[string]any{"type": "spoofed", "error": "spoofed"}})

	e := decode(t, w.Body.Bytes())
	assert.Equal(t, "bad", e.Error.Message)
	assert.Contains(t, w.Body.String(), `"request_id":"req-123"`)
}

func TestResponder_WriteErrorCarriesExtraBesideTheError(t *testing.T) {
	w := httptest.NewRecorder()

	New(nil).WriteError(w, nil, httperr.Error{
		Status: http.StatusTooManyRequests, Type: "rate_limit_error", Message: "slow down",
		Extra: map[string]any{"zzrouter": map[string]any{"limit_kind": "rpm"}},
	})

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	e := decode(t, w.Body.Bytes())
	assert.Equal(t, ErrorTypeRateLimit, e.Error.Type)
	assert.Contains(t, w.Body.String(), `"zzrouter":{"limit_kind":"rpm"}`)
}
