package server

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/dispatch/chain"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/model"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
)

func postReplayProxy(t *testing.T, client *http.Client, endpoint string) (*chain.Proxy, *resolver.Resolved) {
	t.Helper()
	s, _ := instanceStreamTestServer()
	s.model = &model.Subsystem{}
	s.appsConfig = &config.AppsConfig{}
	require.NoError(t, s.appsConfig.AddApp("engine", config.ServiceConfig{
		Enabled: new(true), Mode: "service", Protocol: config.ProtocolOpenAI,
		Runtime:      &config.AppRuntimeConfig{Endpoint: endpoint},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	s.backend = backend.NewResolver(func() *config.AppsConfig { return s.appsConfig })
	s.proxy = NewProxyClient(client, nil, s.inference.normalizers, nil, nil, nil, nil)
	strategies := fallback.NewStrategyRegistry()
	strategies.Register(&fallback.PriorityStrategy{})
	p := chain.New(newChainServerDeps(s), fallback.NewCooldownManager(), fallback.NewCooldownManager(), strategies, nil, nil, nil)
	return p, &resolver.Resolved{OriginalName: "group", GroupName: "group", Strategy: "priority", Candidates: []fallback.Candidate{
		{Name: "first", App: "engine", Model: "m", Priority: 1},
		{Name: "second", App: "engine", Model: "m", Priority: 2},
	}}
}

func TestPostNotReplayedAfterRequestWritten(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read whole request: %v", err)
			return
		}
		hits.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(upstream.Close)
	p, group := postReplayProxy(t, upstream.Client(), upstream.URL)
	c, w := streamEndContext(t, openaiproto.New())
	c.Request.Header.Set("Content-Type", "application/json")
	p.ProxyWithFallback(c, group, []byte(`{"model":"group","messages":[{"role":"user","content":"bill this"}]}`), nil, "group", "", false)
	assert.EqualValues(t, 1, hits.Load(), "the upstream may already have executed this POST")
	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"code":"fallback_exhausted"`)
}

type replayTraceTransport struct {
	hook  string
	calls atomic.Int32
}

func (t *replayTraceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	trace := httptrace.ContextClientTrace(r.Context())
	if t.hook != "unknown" {
		trace.GetConn(r.URL.Host)
	}
	switch t.hook {
	case "dial":
		return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	case "connection":
		trace.GotConn(httptrace.GotConnInfo{})
	case "headers":
		trace.WroteHeaders()
	case "request":
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	case "partial":
		trace.WroteRequest(httptrace.WroteRequestInfo{Err: syscall.EPIPE})
	}
	return nil, &net.OpError{Op: "read", Err: syscall.ECONNRESET}
}

func TestForwardDetachedWriteEvidence(t *testing.T) {
	for _, hook := range []string{"dial", "connection", "headers", "request", "partial", "unknown"} {
		t.Run(hook, func(t *testing.T) {
			transport := &replayTraceTransport{hook: hook}
			p := NewProxyClient(&http.Client{Transport: transport}, nil, nil, nil, nil, nil, nil)
			_, err := p.ForwardDetached(t.Context(), http.MethodPost, "http://engine.test/v1/chat/completions", nil, backend.Engine(), []byte(`{}`)) //nolint:bodyclose // every hook fails the send: no body
			var sent *fallback.RequestError
			require.ErrorAs(t, err, &sent)
			assert.Equal(t, hook != "dial", sent.WroteRequest)
			assert.True(t, errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED))
		})
	}
}

func TestPostWrittenTraceStopsChain(t *testing.T) {
	transport := &replayTraceTransport{hook: "request"}
	p, group := postReplayProxy(t, &http.Client{Transport: transport}, "http://engine.test")
	c, w := streamEndContext(t, openaiproto.New())
	p.ProxyWithFallback(c, group, []byte(`{"model":"group"}`), nil, "group", "", false)
	assert.EqualValues(t, 1, transport.calls.Load())
	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"code":"fallback_exhausted"`)
}

// A POST that never left (the first replica refuses the connection) is safe to send to the next.
func TestPostRefusedDialMovesToNextReplica(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := "http://" + ln.Addr().String()
	require.NoError(t, ln.Close())
	var hits atomic.Int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(live.Close)

	s, _ := instanceStreamTestServer()
	s.model = &model.Subsystem{}
	s.appsConfig = &config.AppsConfig{}
	for name, endpoint := range map[string]string{"down": closed, "up": live.URL} {
		require.NoError(t, s.appsConfig.AddApp(name, config.ServiceConfig{
			Enabled: new(true), Mode: "service", Protocol: config.ProtocolOpenAI,
			Runtime:      &config.AppRuntimeConfig{Endpoint: endpoint},
			Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		}))
	}
	s.backend = backend.NewResolver(func() *config.AppsConfig { return s.appsConfig })
	s.proxy = NewProxyClient(&http.Client{}, nil, s.inference.normalizers, nil, nil, nil, nil)
	strategies := fallback.NewStrategyRegistry()
	strategies.Register(&fallback.PriorityStrategy{})
	cooldowns := fallback.NewCooldownManager()
	p := chain.New(newChainServerDeps(s), cooldowns, fallback.NewCooldownManager(), strategies, nil, nil, nil)
	noRetry := 0
	group := &resolver.Resolved{OriginalName: "group", GroupName: "group", Strategy: "priority", Candidates: []fallback.Candidate{
		{Name: "first", App: "down", Model: "m", Priority: 1, MaxRetries: &noRetry},
		{Name: "second", App: "up", Model: "m", Priority: 2},
	}}
	c, w := streamEndContext(t, openaiproto.New())
	c.Request.Header.Set("Content-Type", "application/json")

	p.ProxyWithFallback(c, group, []byte(`{"model":"group","messages":[{"role":"user","content":"hi"}]}`), nil, "group", "", false)

	assert.EqualValues(t, 1, hits.Load(), "the refused POST never ran, so the next replica serves it")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, cooldowns.InCooldown("first"), "a replica that refuses connections is cooled down")
}
