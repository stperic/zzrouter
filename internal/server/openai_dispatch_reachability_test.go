package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOpenAIDispatchReachability is the regression sentinel for the
// gin-trie pathology investigated in 2026-04-24's project memo on
// "POST /v1/chat/completions 404." Five POST endpoints
// (/chat/completions, /responses, /moderations, /rerank, /audio/speech)
// were silently 404'ing live despite engine.Routes() listing the
// handlers — gin's HandleMethodNotAllowed=true combined with sibling
// static + param routes (/v1/chat/completions/:completion_id) and a
// /v1/models/*model wildcard at the trie root made the static POST
// handlers unreachable in dispatch.
//
// The bug is currently not reproducing on the live binary (8/8 clean
// restarts as of 2026-04-25) but the diagnosis explicitly named the
// failure mode as flaky across map-iteration order during gin trie
// construction. This test pins the working state: every endpoint
// either reaches its handler (any non-404 status) or returns 404 with
// a real handler-emitted body (not gin's NoRoute envelope).
func TestOpenAIDispatchReachability(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/chat/completions", `{}`},
		{http.MethodPost, "/v1/completions", `{}`},
		{http.MethodPost, "/v1/embeddings", `{}`},
		{http.MethodPost, "/v1/responses", `{}`},
		{http.MethodPost, "/v1/moderations", `{}`},
		{http.MethodPost, "/v1/rerank", `{}`},
		{http.MethodPost, "/v1/audio/speech", `{}`},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", TestAdminKey)
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, req)

			// 404 with empty body is the gin NoRoute signature — handler
			// never reached. Real handler responses (even error responses)
			// always carry a body.
			if w.Code == http.StatusNotFound {
				assert.NotEmpty(t, w.Body.String(),
					"%s %s returned bare 404 (gin NoRoute) — handler unreachable in dispatch trie",
					tc.method, tc.path)
			}
		})
	}
}
