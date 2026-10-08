package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// The caller's zzRouter key authenticates the caller to zzRouter only. An
// engine zzRouter launched never sees it, while the protocol headers the
// engine does read still reach it.
func TestLaunchedEngineReceivesNoCallerKey(t *testing.T) {
	var got http.Header
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(engine.Close)
	s := createTestNodeWithDefaults(t)

	body := []byte(`{"model":"org/model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	req := routedRequest(httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)))
	req.Header.Set("X-API-Key", TestAdminKey)
	req.Header.Set("Authorization", "Bearer "+TestUserKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	ctx := context.WithValue(req.Context(), CtxKeyOriginalBody, body)
	req = req.WithContext(context.WithValue(ctx, CtxKeyModel, "org/model"))

	s.proxyToInstance(httptest.NewRecorder(), req, instanceFor(t, engine, "org/model", ""), true)

	require.NotNil(t, got, "the engine was not reached")
	for _, h := range nodeAPIKeyHeaders {
		assert.Emptyf(t, got.Get(h), "the engine received the caller's %s", h)
	}
	assert.Equal(t, "2023-06-01", got.Get("anthropic-version"))
	assert.Equal(t, "interleaved-thinking-2025-05-14", got.Get("anthropic-beta"))
}

// A cold load that outlasts the grace period streams through
// streamFromInstance, which builds its own request. The engine gets what
// the warm path sends it: the protocol headers, and no key.
func TestColdStreamSendsWhatTheWarmPathSends(t *testing.T) {
	var got http.Header
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(engine.Close)
	s := createTestNodeWithDefaults(t)

	body := []byte(`{"model":"org/model","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header = callerRequestHeaders()
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	ctx := context.WithValue(req.Context(), CtxKeyOriginalBody, body)
	req = req.WithContext(context.WithValue(ctx, CtxKeyModel, "org/model"))
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")

	s.streamFromInstance(w, req, instanceFor(t, engine, "org/model", ""), dialectOf(req).(httperr.InBandStreamer))

	require.NotNil(t, got, "the engine was not reached")
	for _, h := range nodeAPIKeyHeaders {
		assert.Emptyf(t, got.Get(h), "the engine received the caller's %s", h)
	}
	assert.Equal(t, "2023-06-01", got.Get("anthropic-version"))
	assert.Equal(t, "interleaved-thinking-2025-05-14", got.Get("anthropic-beta"))
	assert.Equal(t, "application/json", got.Get("Content-Type"))
}

// An external provider authenticates the caller with the caller's own
// key, so that hop keeps it, and only it: no other zzRouter key leaves.
func TestExternalProviderReceivesCallerKey(t *testing.T) {
	backends := map[string]*serviceBackend{"svc": {wire: []string{messagesEndpoint}, status: http.StatusOK}}
	s := newServiceGroupNode(t, []string{"svc"}, backends)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", TestAdminKey)
	req.Header.Set("X-Cluster-API-Key", TestClusterKey)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	hits := backends["svc"].requests()
	require.Len(t, hits, 1)
	assert.Equal(t, TestAdminKey, hits[0].Headers.Get("X-API-Key"))
	assert.Empty(t, hits[0].Headers.Get("X-Cluster-API-Key"), "the cluster key never leaves the cluster")
}
