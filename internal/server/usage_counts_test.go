package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A /v1 stream carries usage only when the caller opts in, so the inference
// log recorded 0 in / 0 out for every stream that didn't — while the same
// model over /api/* logged real counts. Group-routed traffic was already
// counted (chain.ProxyWithFallback injects), so whether a request showed up in
// spend depended on whether it happened to route through a model group.
func TestRequestUsageCounts(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantInject   bool
		wantInclude  any
		wantSuppress bool
	}{
		{
			name:         "streaming request gets usage requested",
			body:         `{"model":"m","stream":true}`,
			wantInject:   true,
			wantInclude:  true,
			wantSuppress: true,
		},
		{
			// Nothing to count per-chunk, and the non-streaming path reads
			// usage off the response body already.
			name:       "non-streaming is left alone",
			body:       `{"model":"m"}`,
			wantInject: false,
		},
		{
			// The hole this closes: an explicit opt-out used to be honoured,
			// so a caller could log 0/0 and be settled against nothing.
			name:         "an explicit opt-out is still counted",
			body:         `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`,
			wantInject:   true,
			wantInclude:  true,
			wantSuppress: true,
		},
		{
			name:        "a caller who asked keeps their frame",
			body:        `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
			wantInject:  false,
			wantInclude: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = req

			got := requestUsageCounts(c, body)

			if !tt.wantInject {
				assert.Equal(t, body, got, "body must be handed on untouched")
			}
			assert.Equal(t, tt.wantSuppress, suppressUsageFrameFromContext(c.Request.Context()),
				"what the caller SEES is a separate question from what we meter")
			if tt.wantInclude == nil {
				return
			}

			var obj map[string]any
			require.NoError(t, json.Unmarshal(got, &obj))
			opts, ok := obj["stream_options"].(map[string]any)
			require.True(t, ok, "stream_options missing from %s", got)
			assert.Equal(t, tt.wantInclude, opts["include_usage"])
		})
	}
}

// Downstream re-reads c.Request.Body, so it has to carry the same bytes the
// dispatch layer sends. Leaving the original body on the request would ship
// the engine a request without stream_options while the log expected counts.
func TestRequestUsageCounts_RewindsRequestBody(t *testing.T) {
	body := []byte(`{"model":"m","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	got := requestUsageCounts(c, body)

	reread, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	assert.Equal(t, string(got), string(reread),
		"the request body and the dispatched bytes must be the same document")
	assert.Contains(t, string(reread), "include_usage")
}
