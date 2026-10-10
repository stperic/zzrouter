package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/protocol/anthropic"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

const partialChatStream = `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n"

func streamEndContext(t *testing.T, dialect httperr.Responder) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	httperr.AttachResponder(dialect)(c)
	return c, rec
}

func TestEndStream_PerDialect(t *testing.T) {
	e := httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_stream_interrupted", Message: "cut"}
	for _, tc := range []struct {
		name  string
		ender httperr.StreamEnder
		want  string
	}{
		{"openai", openaiproto.New(), "data: "},
		{"anthropic", anthropic.New(nil), "event: error\ndata: "},
		{"ollama", newOllamaResponder(), "{\"error\":\"cut\"}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.ender.EndStream(rec, e)
			assert.Contains(t, rec.Body.String(), tc.want)
			assert.Contains(t, rec.Body.String(), "cut")
			assert.NotContains(t, rec.Body.String(), "[DONE]")
			assert.True(t, rec.Flushed)
		})
	}
	for _, r := range []httperr.Responder{anthropic.New(nil), newOllamaResponder()} {
		_, inBand := r.(httperr.InBandStreamer)
		assert.False(t, inBand)
	}
}

func TestEndBrokenStream_NoEnderAborts(t *testing.T) {
	c, rec := streamEndContext(t, newProblemResponder())
	assert.PanicsWithValue(t, http.ErrAbortHandler, func() { endBrokenStream(c.Writer, c.Request, wire.Result{Outcome: wire.Truncated}) })
	assert.Empty(t, rec.Body.String())
}

func TestEndBrokenStream_CompletedAndClientGoneWriteNothing(t *testing.T) {
	for _, outcome := range []wire.Outcome{wire.Completed, wire.ClientGone, wire.UpstreamFailed, wire.Unknown} {
		c, rec := streamEndContext(t, openaiproto.New())
		endBrokenStream(c.Writer, c.Request, wire.Result{Outcome: outcome})
		assert.Empty(t, rec.Body.String())
		assert.False(t, rec.Flushed)
	}
}

func TestMessagesCompat_CutStreamEndsWithErrorEvent(t *testing.T) {
	c, rec := streamEndContext(t, anthropic.New(nil))
	w := newMessagesCompatWriter(c.Writer, "m", true, anthropic.FromChatReply)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(partialChatStream))}
	streamWithUsageInject(c.Request, w, resp, NewResponseNormalizers(), "engine", wire.RoutingMetadata{}, nil)
	w.finish(c)
	body := rec.Body.String()
	assert.Contains(t, body, "event: error\n")
	assert.Contains(t, body, "The upstream stream ended before completing.")
	assert.Equal(t, 1, strings.Count(body, "event: error\n"))
	assert.NotContains(t, body, "message_stop")
	assert.NotContains(t, body, "[DONE]")
}

func TestResponsesCompat_CutStreamIsFailedNotCompleted(t *testing.T) {
	c, rec := streamEndContext(t, openaiproto.New())
	w := newResponsesStreamWriter(c.Writer, "m", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(partialChatStream))}
	streamWithUsageInject(c.Request, w, resp, NewResponseNormalizers(), "engine", wire.RoutingMetadata{}, nil)
	w.Close()
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	body := rec.Body.String()
	assert.Contains(t, body, "event: response.failed\n")
	assert.Contains(t, body, `"code":"upstream_stream_interrupted"`)
	assert.NotContains(t, body, "response.completed")
	assert.NotContains(t, body, "[DONE]")
	assert.Equal(t, 1, strings.Count(body, "event: response.failed\n"))
}

type streamFaultReader struct{ io.Reader }

func (r streamFaultReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

type streamTestTransport struct {
	body        string
	cut         bool
	closed      *bool
	contentType string
}

func (tr streamTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var r io.Reader = strings.NewReader(tr.body)
	if tr.cut {
		r = streamFaultReader{r}
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {tr.contentType}}, Body: streamTestBody{Reader: r, closed: tr.closed}, Request: req}, nil
}

type streamTestBody struct {
	io.Reader
	closed *bool
}

func (b streamTestBody) Close() error {
	if b.closed != nil {
		*b.closed = true
	}
	return nil
}

func instanceStreamTestServer() (*Server, *instance.Instance) {
	s := &Server{config: &config.NodeConfig{}, node: &NodeIdentity{name: "test-node"}}
	s.inference.normalizers = NewResponseNormalizers()
	s.providers.appMgr, _ = prov_apps.NewProviderAppManager(nil) // a launched run is reached on its port
	// The health host names the fake transport's engine.
	inst := &instance.Instance{ID: "test", Provider: "engine", HealthURL: "http://engine.test/health", Port: 80}
	inst.InitConcurrencyLimit(instance.ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: 0})
	return s, inst
}

// assertLeaseReturned checks a broken relay gave its slot back to a cap-1 instance.
func assertLeaseReturned(t *testing.T, inst *instance.Instance) {
	t.Helper()
	assert.Zero(t, inst.GetActiveRequests())
	lease, err := inst.Acquire(context.Background(), nil)
	require.NoError(t, err)
	lease.Release()
}

func TestStreamFromInstance_CutStreamEndsWithDialectError(t *testing.T) {
	for _, cut := range []bool{false, true} {
		t.Run(map[bool]string{false: "eof", true: "read_error"}[cut], func(t *testing.T) {
			s, inst := instanceStreamTestServer()
			closed := false
			s.httpStreamingClient = &http.Client{Transport: streamTestTransport{body: partialChatStream, cut: cut, closed: &closed, contentType: "text/event-stream"}}
			c, rec := streamEndContext(t, openaiproto.New())
			s.streamFromInstance(c.Writer, c.Request, inst, openaiproto.New())
			assert.True(t, closed)
			assert.Contains(t, rec.Body.String(), `"code":"upstream_stream_interrupted"`)
			assert.NotContains(t, rec.Body.String(), "[DONE]")
			assertLeaseReturned(t, inst)
		})
	}
}

func TestProxyToInstance_CutStreamEndsWithDialectError(t *testing.T) {
	for _, cut := range []bool{false, true} {
		t.Run(map[bool]string{false: "eof", true: "read_error"}[cut], func(t *testing.T) {
			s, inst := instanceStreamTestServer()
			closed := false
			s.httpStreamingClient = &http.Client{Transport: streamTestTransport{body: partialChatStream, cut: cut, closed: &closed, contentType: "text/event-stream"}}
			c, rec := streamEndContext(t, openaiproto.New())
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), http.ServerContextKey, &http.Server{}))
			s.proxyToInstance(c.Writer, c.Request, inst, true)
			assert.True(t, closed)
			assert.Contains(t, rec.Body.String(), `"code":"upstream_stream_interrupted"`)
			assert.NotContains(t, rec.Body.String(), "[DONE]")
			assertLeaseReturned(t, inst)
		})
	}
}

func TestOllamaNativeRelay_CutStreamEndsWithErrorLine(t *testing.T) {
	const partial = "{\"response\":\"partial\",\"done\":false}\n"
	for _, path := range []string{"response", "provider", "mutation"} {
		t.Run(path, func(t *testing.T) {
			c, rec := streamEndContext(t, newOllamaResponder())
			c.Request.URL.Path = "/api/generate"
			switch path {
			case "response":
				writeOllamaResponse(c, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/x-ndjson"}}, Body: io.NopCloser(strings.NewReader(partial))})
			case "provider":
				s, _ := instanceStreamTestServer()
				s.httpStreamingClient = &http.Client{Transport: streamTestTransport{body: partial, contentType: "application/x-ndjson"}}
				s.proxyToOllamaProvider(c, &backend.Resolved{Endpoint: "http://engine.test", Upstream: backend.Engine()}, "/api/generate", nil, nil)
			case "mutation":
				err := writeOllamaMutation(c, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/x-ndjson"}}, Body: io.NopCloser(strings.NewReader(partial))}, func() error { t.Fatal("cut stream must not publish"); return nil })
				require.Error(t, err)
			}
			assert.Contains(t, rec.Body.String(), "{\"error\":\"The upstream stream ended before completing.\"}\n")
			assert.Equal(t, 1, strings.Count(rec.Body.String(), `"error":`))
			assert.NotContains(t, rec.Body.String(), "[DONE]")
		})
	}
}

type failingStreamWriter struct{ *httptest.ResponseRecorder }

func (w failingStreamWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestProxyToInstance_ClientWriteFailureAborts(t *testing.T) {
	s, inst := instanceStreamTestServer()
	s.httpStreamingClient = &http.Client{Transport: mcpRoundTripper(func(req *http.Request) (*http.Response, error) {
		resp, err := (streamTestTransport{body: partialChatStream, cut: true, contentType: "text/event-stream"}).RoundTrip(req)
		// Return bytes and the upstream error together to exercise both failures.
		resp.Body = io.NopCloser(iotest.DataErrReader(resp.Body))
		return resp, err
	})}
	c, rec := streamEndContext(t, openaiproto.New())
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), http.ServerContextKey, &http.Server{}))
	assert.PanicsWithValue(t, http.ErrAbortHandler, func() { s.proxyToInstance(failingStreamWriter{rec}, c.Request, inst, true) })
	assert.Empty(t, rec.Body.String())
}

func TestProxyToInstance_CancelledStreamAborts(t *testing.T) {
	s, inst := instanceStreamTestServer()
	c, rec := streamEndContext(t, openaiproto.New())
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(context.WithValue(ctx, http.ServerContextKey, &http.Server{}))
	s.httpStreamingClient = &http.Client{Transport: mcpRoundTripper(func(req *http.Request) (*http.Response, error) {
		cancel()
		return streamTestTransport{body: partialChatStream, cut: true, contentType: "text/event-stream"}.RoundTrip(req)
	})}
	assert.PanicsWithValue(t, http.ErrAbortHandler, func() { s.proxyToInstance(c.Writer, c.Request, inst, true) })
	assert.NotContains(t, rec.Body.String(), "upstream_stream_interrupted")
}

func TestOllamaMutation_NonStreamingReadFailureWritesError(t *testing.T) {
	c, rec := streamEndContext(t, newOllamaResponder())
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(streamFaultReader{strings.NewReader("partial")})}
	err := writeOllamaMutation(c, resp, func() error { t.Fatal("must not publish a cut body"); return nil })
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), `"error":`)
}

func TestOllamaMutation_HoldsSplitTerminalUntilPublication(t *testing.T) {
	for _, publishFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[publishFails], func(t *testing.T) {
			c, rec := streamEndContext(t, newOllamaResponder())
			const progress = "{\"status\":\"pulling\"}\n"
			const terminal = "{\"status\":\"success\"}"
			body := io.MultiReader(strings.NewReader(progress), strings.NewReader(terminal[:12]), strings.NewReader(terminal[12:]))
			err := writeOllamaMutation(c, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/x-ndjson"}}, Body: io.NopCloser(body)}, func() error {
				assert.Equal(t, progress, rec.Body.String())
				if publishFails {
					return io.ErrUnexpectedEOF
				}
				return nil
			})
			if publishFails {
				require.Error(t, err)
				assert.NotContains(t, rec.Body.String(), `"success"`)
				assert.Equal(t, 1, strings.Count(rec.Body.String(), `"error":`))
			} else {
				require.NoError(t, err)
				assert.Equal(t, progress+terminal, rec.Body.String())
			}
		})
	}
}

func TestEndBrokenStream_SeparatesNativeCutFrames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dialect  httperr.Responder
		partial  string
		boundary string
	}{
		{"openai", openaiproto.New(), `data: {"choices":`, "\n\ndata: "},
		{"anthropic", anthropic.New(nil), `data: {"type":`, "\n\nevent: error\ndata: "},
		{"ollama", newOllamaResponder(), `{"response":"par`, "\n{\"error\":"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := streamEndContext(t, tc.dialect)
			resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(streamFaultReader{strings.NewReader(tc.partial)})}
			streamWithUsageInject(c.Request, c.Writer, resp, NewResponseNormalizers(), "engine", wire.RoutingMetadata{}, nil)
			assert.Contains(t, rec.Body.String(), tc.partial+tc.boundary)
			assert.NotContains(t, rec.Body.String(), "[DONE]")
		})
	}
}

func TestProxyToInstance_SeparatesCutFrame(t *testing.T) {
	s, inst := instanceStreamTestServer()
	s.httpStreamingClient = &http.Client{Transport: streamTestTransport{body: `data: {"choices":`, cut: true, contentType: "text/event-stream"}}
	c, rec := streamEndContext(t, openaiproto.New())
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), http.ServerContextKey, &http.Server{}))
	s.proxyToInstance(c.Writer, c.Request, inst, true)
	assert.Contains(t, rec.Body.String(), `data: {"choices":`+"\n\ndata: {\"error\":")
}

// The Ollama client JSON-parses every line, so a cut on a line boundary must not gain a blank line.
func TestEndBrokenStream_OllamaLineBoundaryHasNoBlankLine(t *testing.T) {
	c, rec := streamEndContext(t, newOllamaResponder())
	complete := "{\"response\":\"a\",\"done\":false}\n"
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(streamFaultReader{strings.NewReader(complete)})}
	streamWithUsageInject(c.Request, c.Writer, resp, NewResponseNormalizers(), "engine", wire.RoutingMetadata{}, nil)
	for _, line := range strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n") {
		assert.NotEmpty(t, line, rec.Body.String())
	}
	assert.Contains(t, rec.Body.String(), complete+`{"error":`)
}

// An error ending runs past any length the upstream declared, so a streaming relay declares none.
func TestOllamaRelay_DropsUpstreamContentLength(t *testing.T) {
	c, rec := streamEndContext(t, newOllamaResponder())
	partial := "{\"status\":\"pulling\"}\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/x-ndjson"}, "Content-Length": []string{strconv.Itoa(len(partial))}},
		Body:       io.NopCloser(streamFaultReader{strings.NewReader(partial)}),
	}
	writeOllamaResponse(c, resp)
	assert.Empty(t, rec.Header().Get("Content-Length"))
	assert.Contains(t, rec.Body.String(), partial+`{"error":`)
}
