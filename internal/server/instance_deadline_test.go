package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type deadlineBody struct {
	ctx context.Context
	io.Reader
	delay time.Duration
}

func (b deadlineBody) Read(p []byte) (int, error) {
	select {
	case <-time.After(b.delay):
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
	n, err := b.Reader.Read(p)
	if err == io.EOF && b.delay == 0 {
		<-b.ctx.Done()
		return n, b.ctx.Err()
	}
	return n, err
}
func (deadlineBody) Close() error { return nil }

type deadlineLogHook func(llm.InferenceLogData)

func (f deadlineLogHook) OnInferenceComplete(d llm.InferenceLogData) { f(d) }

func TestInstanceFirstByteDeadline(t *testing.T) {
	for _, preamble := range []bool{false, true} {
		t.Run(map[bool]string{false: "warm", true: "preamble"}[preamble], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, inst := instanceStreamTestServer()
				c, w := streamEndContext(t, openai.New())
				ctx, cancel := context.WithTimeout(c.Request.Context(), 2*constants.DefaultFirstByteTimeout)
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				runRequest := newInstanceRequest(c.Request, []byte(`{"model":"m","stream":true}`))
				var completion []llm.InferenceLogData
				llm.SetInferenceLogHook(deadlineLogHook(func(d llm.InferenceLogData) { completion = append(completion, d) }))
				defer llm.SetInferenceLogHook(nil)
				s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(req *http.Request) (*http.Response, error) {
					<-req.Context().Done()
					assert.ErrorIs(t, context.Cause(req.Context()), instance.ErrFirstByteTimeout)
					return nil, req.Context().Err()
				})}
				if preamble {
					openEventStream(w)
					s.streamFromInstance(w, c.Request, runRequest, inst, openai.New())
				} else {
					s.proxyToInstance(w, c.Request, runRequest, inst, true)
				}
				if !preamble {
					assert.Equal(t, http.StatusGatewayTimeout, w.Code)
				}
				assert.Contains(t, w.Body.String(), `"code":"upstream_timeout"`)
				require.Len(t, completion, 1)
				assert.Equal(t, "upstream_timeout", completion[0].ErrorType)
				assertLeaseReturned(t, inst)
			})
		})
	}
}

func TestInstanceStreamIdleDeadline(t *testing.T) {
	for _, preamble := range []bool{false, true} {
		t.Run(map[bool]string{false: "warm", true: "preamble"}[preamble], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, inst := instanceStreamTestServer()
				c, w := streamEndContext(t, openai.New())
				ctx, cancel := context.WithTimeout(c.Request.Context(), 2*constants.DefaultFirstByteTimeout)
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				runRequest := newInstanceRequest(c.Request, []byte(`{"model":"m","stream":true}`))
				var completion []llm.InferenceLogData
				llm.SetInferenceLogHook(deadlineLogHook(func(d llm.InferenceLogData) { completion = append(completion, d) }))
				defer llm.SetInferenceLogHook(nil)
				s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: deadlineBody{ctx: req.Context(), Reader: strings.NewReader(partialChatStream)}}, nil
				})}
				if preamble {
					openEventStream(w)
					s.streamFromInstance(w, c.Request, runRequest, inst, openai.New())
				} else {
					s.proxyToInstance(w, c.Request, runRequest, inst, true)
				}
				assert.Contains(t, w.Body.String(), `"code":"upstream_stream_interrupted"`)
				assert.NotContains(t, w.Body.String(), "[DONE]")
				require.Len(t, completion, 1)
				assert.Equal(t, "upstream_stream_error", completion[0].ErrorType)
				assert.Equal(t, instance.ErrStreamIdle.Error(), completion[0].ErrorMessage)
				assertLeaseReturned(t, inst)
			})
		})
	}
}

func TestNonStreamHasNoStreamDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, inst := instanceStreamTestServer()
		c, w := streamEndContext(t, openai.New())
		runRequest := newInstanceRequest(c.Request, []byte(`{"model":"m","stream":false}`))
		const response = `{"model":"m","choices":[{"message":{"content":"slow but complete"}}]}`
		s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(req *http.Request) (*http.Response, error) {
			select {
			case <-time.After(2 * constants.DefaultFirstByteTimeout):
			case <-req.Context().Done():
				return nil, context.Cause(req.Context())
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: deadlineBody{ctx: req.Context(), Reader: strings.NewReader(response), delay: 2 * constants.DefaultStreamIdleTimeout}}, nil
		})}
		s.proxyToInstance(w, c.Request, runRequest, inst, true)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "slow but complete")
		assertLeaseReturned(t, inst)
	})
}

func TestSyntheticInstanceRequestDoesNotPublish(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prior := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prior)
		assert.NoError(t, provider.Shutdown(context.Background()))
	})
	spans := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	priorTracer := otel.GetTracerProvider()
	otel.SetTracerProvider(tracer)
	t.Cleanup(func() {
		otel.SetTracerProvider(priorTracer)
		assert.NoError(t, tracer.Shutdown(context.Background()))
	})
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled,
	})
	store := inferencelog.NewStore(10, 0)
	llm.SetInferenceLogHook(NewInferenceLogBridge(store, "test-node", false, nil))
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })

	const usage = `data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"cost":0.1}}` + "\n\n"
	for _, tc := range []struct {
		name      string
		status    int
		stream    string
		errorCode string
	}{
		{"success", http.StatusOK, partialChatStream + usage + "data: [DONE]\n\n", ""},
		{"HTTP upstream error", http.StatusBadGateway, `{"error":{"message":"upstream failed"}}`, ""},
		{"in-band upstream error", http.StatusOK, "event: error\n\n", "upstream_stream_failed"},
		{"first byte timeout", 0, "", "upstream_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, run := instanceStreamTestServer()
				t.Cleanup(run.Cancel)
				run.MarkRunning(false, nil)
				ctx, cancel := context.WithTimeout(trace.ContextWithSpanContext(t.Context(), parent), 2*constants.DefaultFirstByteTimeout)
				defer cancel()
				raw := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions?verbose=0", nil)
				body, _ := wire.ForceUsageReporting([]byte(`{"model":"m","stream":true}`))
				req := newInstanceRequest(raw, body)
				req.Synthetic = true
				s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(upstream *http.Request) (*http.Response, error) {
					assert.Equal(t, "/v1/chat/completions?verbose=0", upstream.URL.RequestURI())
					payload, err := io.ReadAll(upstream.Body)
					assert.NoError(t, err)
					assert.JSONEq(t, `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`, string(payload))
					assert.EqualValues(t, 1, run.GetActiveRequests())
					assert.True(t, run.IdleSnapshot().Leased)
					if tc.status == 0 {
						<-upstream.Context().Done()
						return nil, upstream.Context().Err()
					}
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.stream))}, nil
				})}
				measure := func() {
					lease, target, up, ok := s.openRun(req, run, nil, func(e httperr.Error) { t.Errorf("open run: %v", e) })
					require.True(t, ok)
					defer lease.Release()
					prepared, _ := s.instanceRequest(req, run)
					resp, err := s.sendToInstance(req, lease, prepared, target, up) //nolint:contextcheck // The lease derives its context from req.
					if tc.status == 0 {
						require.ErrorIs(t, err, instance.ErrFirstByteTimeout)
						assert.Equal(t, tc.errorCode, instanceSendFailure(nil, err).Code)
						return
					}
					require.NoError(t, err)
					defer func() { assert.NoError(t, resp.Body.Close()) }()
					if resp.StatusCode >= 400 {
						assert.Equal(t, tc.status, resp.StatusCode)
						wire.RecordPreByteCopyErrorFromStatus(nil, resp.StatusCode)
						return
					}
					m, err := wire.NewStreamMeasurement(utils.Now)
					require.NoError(t, err)
					wire.CopyStream(ctx, io.Discard, resp.Body, m, nil)
					assert.Equal(t, tc.errorCode, m.ErrorCode)
					if tc.errorCode == "" {
						assert.Equal(t, new(4), m.InputTokens)
						assert.Equal(t, new(2), m.OutputTokens)
					}
				}
				measure()
				assert.EqualValues(t, 1, run.ToInfo("test-node").RequestsAdmitted)
				assert.Zero(t, run.GetActiveRequests())
			})
		})
	}
	s, run := instanceStreamTestServer()
	t.Cleanup(run.Cancel)
	raw := httptest.NewRequestWithContext(t.Context(), http.MethodPost, chatCompletionsPath, nil)
	req := newInstanceRequest(raw, nil)
	req.Synthetic = true
	req.RawQuery = "invalid=\n"
	var failure httperr.Error
	_, _, _, ok := s.openRun(req, run, nil, func(e httperr.Error) { failure = e })
	assert.False(t, ok)
	assert.Equal(t, "instance_unavailable", failure.Code)
	assert.Zero(t, run.GetActiveRequests())
	assert.Empty(t, store.Query(inferencelog.QueryFilter{}))
	assert.Empty(t, spans.Started(), "synthetic requests do not create attempt spans")
	var observed metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &observed))
	assert.Empty(t, observed.ScopeMetrics, "synthetic request published an admission, GenAI or spend metric")

	public, err := run.Acquire(t.Context(), nil)
	require.NoError(t, err)
	public.Release()
	require.NoError(t, reader.Collect(t.Context(), &observed))
	require.NotEmpty(t, observed.ScopeMetrics, "the metric reader sees public admission")
}

func TestSyntheticLeaseWithRecorderPublishesRunFailure(t *testing.T) {
	s, run := instanceStreamTestServer()
	t.Cleanup(run.Cancel)
	raw := httptest.NewRequestWithContext(t.Context(), http.MethodPost, chatCompletionsPath, nil)
	req := newInstanceRequest(raw, nil)
	req.Synthetic = true
	req.RawQuery = "invalid=\n"
	var completion []llm.InferenceLogData
	llm.SetInferenceLogHook(deadlineLogHook(func(d llm.InferenceLogData) { completion = append(completion, d) }))
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })
	recorder := llm.NewInferenceRecorder(t.Context(), "m", run.Provider)
	var failure httperr.Error
	_, _, _, ok := s.openRun(req, run, recorder, func(e httperr.Error) { failure = e })
	assert.False(t, ok)
	assert.Equal(t, "instance_unavailable", failure.Code)
	require.Len(t, completion, 1)
	assert.Equal(t, "backend_unreachable", completion[0].ErrorType)
}

func TestInstanceCredentialUpstreamWithEmptyHeader(t *testing.T) {
	s, run := instanceStreamTestServer()
	t.Cleanup(run.Cancel)
	raw := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", nil)
	raw.Header = nil
	req := newInstanceRequest(raw, []byte(`{"model":"m"}`))
	require.NotNil(t, req.Header)
	lease, err := s.openInstance(req, run)
	require.NoError(t, err)
	defer lease.Release()
	const credential = "upstream-test-token"
	target := &url.URL{Scheme: "http", Host: "engine.test", Path: req.Path}
	up := backend.Credential(&config.APIConfig{Token: credential})
	s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(upstream *http.Request) (*http.Response, error) {
		assert.Equal(t, "Bearer "+credential, upstream.Header.Get("Authorization"))
		assert.Equal(t, "application/json", upstream.Header.Get("Content-Type"))
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}
	body, _ := s.instanceRequest(req, run)
	resp, err := s.sendToInstance(req, lease, body, target, up)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	assert.Empty(t, req.Header)
}

func TestSyntheticInstanceRequestUsesWatchStream(t *testing.T) {
	for _, firstByte := range []bool{true, false} {
		t.Run(map[bool]string{true: "first byte", false: "idle"}[firstByte], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, run := instanceStreamTestServer()
				t.Cleanup(run.Cancel)
				ctx, cancel := context.WithTimeout(t.Context(), 2*constants.DefaultFirstByteTimeout)
				defer cancel()
				body, _ := wire.ForceUsageReporting([]byte(`{"model":"m","stream":true}`))
				req := newInstanceRequest(httptest.NewRequestWithContext(ctx, http.MethodPost, chatCompletionsPath, nil), body)
				req.Synthetic = true
				s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(upstream *http.Request) (*http.Response, error) {
					if firstByte {
						<-upstream.Context().Done()
						return nil, upstream.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Body: deadlineBody{ctx: upstream.Context(), Reader: strings.NewReader(partialChatStream)}}, nil
				})}
				lease, target, up, ok := s.openRun(req, run, nil, func(e httperr.Error) { t.Errorf("open run: %v", e) })
				require.True(t, ok)
				defer lease.Release()
				prepared, _ := s.instanceRequest(req, run)
				resp, err := s.sendToInstance(req, lease, prepared, target, up)
				if firstByte {
					require.ErrorIs(t, err, instance.ErrFirstByteTimeout)
					failure := instanceSendFailure(nil, err)
					assert.Equal(t, http.StatusGatewayTimeout, failure.Status)
					assert.Equal(t, "upstream_timeout", failure.Code)
				} else {
					require.NoError(t, err)
					m, err := wire.NewStreamMeasurement(utils.Now)
					require.NoError(t, err)
					result := wire.CopyStream(ctx, io.Discard, resp.Body, m, nil)
					require.NoError(t, resp.Body.Close())
					assert.ErrorIs(t, result.Err, instance.ErrStreamIdle)
					assert.Equal(t, "upstream_stream_interrupted", m.ErrorCode)
				}
				assert.NoError(t, ctx.Err(), "the lease timer expires before the caller timeout")
			})
		})
	}
}
