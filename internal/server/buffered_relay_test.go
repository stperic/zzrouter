package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/protocol/anthropic"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
)

func checkBufferedDialects(t *testing.T, relay func(*testing.T, *gin.Context)) {
	t.Helper()
	for _, tc := range []struct {
		name, path string
		dialect    httperr.Responder
	}{
		{"openai", "/v1/chat/completions", openaiproto.New()},
		{"anthropic", "/v1/messages", anthropic.New(nil)},
		{"ollama", "/api/chat", newOllamaResponder()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := streamEndContext(t, tc.dialect)
			c.Request.URL.Path = tc.path
			relay(t, c)
			require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
			var envelope map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
			if tc.name == "ollama" {
				message, ok := envelope["error"].(string)
				require.True(t, ok, w.Body.String())
				assert.NotEmpty(t, message)
			} else {
				err, ok := envelope["error"].(map[string]any)
				require.True(t, ok, w.Body.String())
				assert.NotEmpty(t, err["message"])
				if tc.name == "anthropic" {
					assert.Equal(t, "error", envelope["type"])
				}
			}
			assert.NotContains(t, w.Body.String(), "partial")
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
		})
	}
}

const cutBufferedBody = `{"choices":[{"message":{"content":"partial"}}`

func TestCommitWithRouting_CutBodyAnswersDialectError(t *testing.T) {
	checkBufferedDialects(t, func(t *testing.T, c *gin.Context) {
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, ContentLength: -1,
			Body: io.NopCloser(streamFaultReader{strings.NewReader(cutBufferedBody)})}
		defer func() { _ = resp.Body.Close() }()
		s, _ := instanceStreamTestServer()
		deps := newChainServerDeps(s)
		meta := wire.RoutingMetadata{Provider: "engine"}
		result := wire.CommitWithRouting(c.Request.Context(), c.Writer, resp, meta, nil, s.inference.normalizers, deps.BufferedShape(meta, nil))
		deps.FinishRelay(c.Writer, c.Request, resp, result)
	})
}

func TestForwardToBackend_CutBodyAnswersDialectError(t *testing.T) {
	checkBufferedDialects(t, func(t *testing.T, c *gin.Context) {
		closed := false
		client := &http.Client{Transport: streamTestTransport{body: cutBufferedBody, cut: true, contentType: "application/json", closed: &closed}}
		p := NewProxyClient(client, nil, NewResponseNormalizers(), nil, nil, nil, nil)
		p.ForwardToBackend(c.Writer, c.Request, "engine", "http://engine.test"+c.Request.URL.Path, backend.Engine(), nil)
		assert.True(t, closed)
	})
}

func TestProxyToWorkerURL_CutBodyAnswersDialectError(t *testing.T) {
	checkBufferedDialects(t, func(t *testing.T, c *gin.Context) {
		s, _ := instanceStreamTestServer()
		closed := false
		client := &http.Client{Transport: streamTestTransport{body: cutBufferedBody, cut: true, contentType: "application/json", closed: &closed}}
		s.proxyToWorkerURL(c, "https://worker.test", "worker", "engine", nil, client)
		assert.True(t, closed)
	})
}

func TestProxyToInstance_CutBodyAnswersDialectError(t *testing.T) {
	checkBufferedDialects(t, func(t *testing.T, c *gin.Context) {
		s, inst := instanceStreamTestServer()
		closed := false
		s.httpStreamingClient = &http.Client{Transport: streamTestTransport{body: cutBufferedBody, cut: true, contentType: "application/json", closed: &closed}}
		s.proxyToInstance(c.Writer, c.Request, inst, true)
		assert.True(t, closed)
	})
}

func TestFailBrokenBody_CompletedAndClientGoneWriteNothing(t *testing.T) {
	for _, outcome := range []wire.Outcome{wire.Completed, wire.ClientGone, wire.UpstreamFailed, wire.Unknown} {
		w := httptest.NewRecorder()
		failBrokenBody(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), wire.Result{Outcome: outcome})
		assert.Empty(t, w.Body.Bytes())
		assert.Empty(t, w.Header())
	}
}
