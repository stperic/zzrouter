package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// The same failure must be identifiable whether it arrives as a status
// code or inside a stream. The code used to be hardcoded nil, so a
// caller switching on error.code got an answer only on the header path.
func TestStreamErrorCarriesCodeThenDone(t *testing.T) {
	rec := httptest.NewRecorder()
	New().StreamError(rec, httperr.Error{Status: http.StatusServiceUnavailable, Type: "api_error",
		Message: "instance failed to start", Code: "model_load_failed"})

	frame, rest, _ := strings.Cut(strings.TrimLeft(rec.Body.String(), "\n"), "\n\n")
	var got struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Code    *string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &got))

	assert.Equal(t, "api_error", got.Error.Type)
	assert.Equal(t, "instance failed to start", got.Error.Message)
	require.NotNil(t, got.Error.Code, "code must survive into the stream")
	assert.Equal(t, "model_load_failed", *got.Error.Code)
	assert.Equal(t, "data: [DONE]\n\n", rest)
}

// An empty code stays null rather than becoming the empty string, so a
// client can still tell "no code" from a code it does not recognise.
func TestStreamErrorKeepsEmptyCodeNull(t *testing.T) {
	rec := httptest.NewRecorder()
	New().StreamError(rec, httperr.Error{Status: http.StatusInternalServerError, Type: "server_error", Message: "boom"})

	assert.Contains(t, rec.Body.String(), `"code":null`)
}

func TestWriteErrorPutsExtraBesideTheError(t *testing.T) {
	rec := httptest.NewRecorder()
	New().WriteError(rec, nil, httperr.Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error",
		Message: "slow down", Code: "rate_limit_exceeded", Extra: map[string]any{"zzrouter": map[string]any{"limit_kind": "rpm"}}})

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.JSONEq(t, `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"},
		"zzrouter":{"limit_kind":"rpm"}}`, rec.Body.String())
}

func TestEndStream_PerDialect(t *testing.T) {
	e := httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_stream_interrupted", Message: "cut"}
	rec := httptest.NewRecorder()
	New().EndStream(rec, e)
	assert.Equal(t, 1, strings.Count(rec.Body.String(), "data: "))
	assert.NotContains(t, rec.Body.String(), "[DONE]")
	assert.Contains(t, rec.Body.String(), `"code":"upstream_stream_interrupted"`)
	assert.Contains(t, rec.Body.String(), `"param":null`)
	assert.True(t, rec.Flushed)
	rec = httptest.NewRecorder()
	New(WithOpaqueErrors(true)).EndStream(rec, e)
	assert.Empty(t, rec.Body.String())
	assert.True(t, rec.Flushed)
}

func TestStreamError_StillEndsWithDone(t *testing.T) {
	for _, opaque := range []bool{false, true} {
		rec := httptest.NewRecorder()
		New(WithOpaqueErrors(opaque)).StreamError(rec, httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Message: "cut"})
		assert.True(t, strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n"))
		assert.Equal(t, 1, strings.Count(rec.Body.String(), "[DONE]"))
		assert.True(t, rec.Flushed)
	}
}
