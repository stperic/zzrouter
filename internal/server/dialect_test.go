package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// A failure before any backend byte on a streaming request is the one
// place the dialects disagree on more than shape: OpenAI reports it
// inside a 200 stream (a bare status leaves OpenWebUI stuck), Anthropic
// as the status its SDK retries.
func TestFailBeforeBackend_FollowsTheDialect(t *testing.T) {
	capacity := httperr.Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error",
		Message: "instance at capacity", Code: "capacity_exceeded"}

	streaming := func(path string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		ctx := context.WithValue(req.Context(), CtxKeyOriginalBody, []byte(`{"stream":true}`))
		return routedRequest(req.WithContext(ctx))
	}

	t.Run("openai streams the failure", func(t *testing.T) {
		w := httptest.NewRecorder()
		failBeforeBackend(w, streaming("/v1/chat/completions"), capacity)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
		assert.Contains(t, w.Body.String(), `"code":"capacity_exceeded"`)
		assert.True(t, strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n"))
		assert.Empty(t, w.Header().Get("Retry-After"), "a 200 stream carries no Retry-After")
	})

	t.Run("anthropic answers with the status", func(t *testing.T) {
		w := httptest.NewRecorder()
		failBeforeBackend(w, streaming("/v1/messages"), capacity)

		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.Equal(t, "5", w.Header().Get("Retry-After"))
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
		assert.Equal(t, "error", body.Type)
		assert.Equal(t, "rate_limit_error", body.Error.Type)
	})

	t.Run("a non-streaming request always gets the status", func(t *testing.T) {
		w := httptest.NewRecorder()
		failBeforeBackend(w, routedRequest(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)), capacity)
		assert.Equal(t, http.StatusTooManyRequests, w.Code)
	})
}
