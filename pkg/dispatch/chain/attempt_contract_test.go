package chain

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	pfallback "github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func chainTraceRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	previous := otel.GetTracerProvider()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = tp.Shutdown(context.Background()) })
	return spans
}

type closeObservedBody struct {
	io.Reader
	close func()
}

func (b closeObservedBody) Close() error { b.close(); return nil }

type panicShapeDeps struct{ *fakeDeps }

func (*panicShapeDeps) BufferedShape(wire.RoutingMetadata, *llm.InferenceRecorder) func([]byte) []byte {
	return func([]byte) []byte { panic("synthetic shaping panic") }
}

func TestTraceAttemptPanicClosesBodyAndReleasesLease(t *testing.T) {
	spans := chainTraceRecorder(t)
	ctx, parent := llm.Tracer().Start(t.Context(), "logical")
	deps := newFakeDeps()
	inst := localGroupInstance(t, deps, 0)
	c, _ := buildGinCtx(t, `{"model":"group","stream":false}`)
	c.Request = c.Request.WithContext(ctx)
	closed := false
	deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: closeObservedBody{Reader: strings.NewReader(`{"choices":[],"usage":{"prompt_tokens":7}}`), close: func() {
			closed = true
			assert.EqualValues(t, 1, inst.GetActiveRequests(), "body closes before lease release")
			assert.Empty(t, spans.Ended(), "body closes before attempt end")
		}}}, nil
	}
	p := newProxyWithFake(&panicShapeDeps{deps})
	require.Panics(t, func() {
		p.ProxyWithFallback(c, leaseGroup([]pfallback.Candidate{{Name: "local", App: "engine", Model: "m", OnDemand: true}}), []byte(`{"model":"group"}`), nil, "group", "", false)
	})
	require.True(t, closed)
	assert.Zero(t, inst.GetActiveRequests())
	require.Len(t, spans.Ended(), 1)
	assert.Equal(t, codes.Error, spans.Ended()[0].Status().Code)
	assert.Equal(t, "operation failed", spans.Ended()[0].Status().Description)
	assert.Equal(t, parent.SpanContext().SpanID(), spans.Ended()[0].Parent().SpanID())
	parent.End()
}

func TestTraceAttemptEndsBeforeBackoffAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spans := chainTraceRecorder(t)
		ctx, parent := llm.Tracer().Start(t.Context(), "logical")
		defer parent.End()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		deps := newFakeDeps()
		inst := localGroupInstance(t, deps, 0)
		c, _ := buildGinCtx(t, `{"model":"group"}`)
		c.Request = c.Request.WithContext(ctx)
		closed := false
		deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
			return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: closeObservedBody{Reader: strings.NewReader(`{"error":{"message":"busy"}}`), close: func() {
				closed = true
				assert.EqualValues(t, 1, inst.GetActiveRequests())
				assert.Empty(t, spans.Ended())
			}}}, nil
		}
		dep := pfallback.Candidate{Name: "local", App: "engine", Model: "m", OnDemand: true}
		p := newProxyWithFake(deps)
		done := make(chan attemptDisposition, 1)
		go func() {
			_, disposition := p.tryDeployment(c, dep, []byte(`{"model":"group"}`), nil, leaseGroup([]pfallback.Candidate{dep}), 1, routeMetaCtx{decisionStart: utils.Now()})
			done <- disposition
		}()
		synctest.Wait()
		require.True(t, closed)
		assert.Zero(t, inst.GetActiveRequests(), "no lease during backoff")
		require.Len(t, spans.Ended(), 1, "attempt ended while retry timer is waiting")
		assert.Equal(t, codes.Error, spans.Ended()[0].Status().Code)
		assert.EqualValues(t, 1, deps.forwardCalls.Load())
		cancel()
		synctest.Wait()
		assert.Equal(t, attemptClientGone, <-done)
		assert.EqualValues(t, 1, deps.forwardCalls.Load(), "backoff cancellation must not start another attempt")
		assert.Len(t, spans.Ended(), 1)
	})
}

func TestTraceAttemptRejectedLease(t *testing.T) {
	spans := chainTraceRecorder(t)
	ctx, parent := llm.Tracer().Start(t.Context(), "logical")
	defer parent.End()
	deps := newFakeDeps()
	inst := localGroupInstance(t, deps, 10*time.Millisecond)
	held, err := inst.Acquire(t.Context(), nil)
	require.NoError(t, err)
	defer held.Release()
	c, _ := buildGinCtx(t, `{"model":"group"}`)
	c.Request = c.Request.WithContext(ctx)
	dep := pfallback.Candidate{Name: "local", App: "engine", Model: "m", OnDemand: true}
	p := newProxyWithFake(deps)
	_ = p.tryOnce(c, dep, []byte(`{"model":"group"}`), nil, leaseGroup([]pfallback.Candidate{dep}), routeMetaCtx{decisionStart: utils.Now()}, 0, 0)
	assert.Zero(t, deps.forwardCalls.Load())
	require.Len(t, spans.Ended(), 1)
	assert.Equal(t, codes.Error, spans.Ended()[0].Status().Code)
	assert.Equal(t, parent.SpanContext().SpanID(), spans.Ended()[0].Parent().SpanID())
}

func TestTraceAttemptRawStatus(t *testing.T) {
	spans := chainTraceRecorder(t)
	deps := newFakeDeps()
	classifier := httperr.Classifier{{Pattern: "image input is not supported", Class: httperr.FailureUnsupportedInput}}
	require.NoError(t, classifier.Compile())
	deps.classifier = &classifier
	deps.endpoints["engine"] = "http://engine.test"
	deps.forward["http://engine.test/v1/chat/completions"] = func() (*http.Response, error) {
		return cannedJSON(503, `{"error":{"message":"image input is not supported","type":"api_error"}}`), nil
	}
	c, w := buildGinCtx(t, `{"model":"group","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`)
	ctx, parent := llm.Tracer().Start(c.Request.Context(), "logical")
	defer parent.End()
	c.Request = c.Request.WithContext(ctx)
	dep := pfallback.Candidate{Name: "actual", App: "engine", Model: "m"}
	p := newProxyWithFake(deps)
	outcome := p.tryOnce(c, dep, []byte(`{"model":"group","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`), nil, leaseGroup([]pfallback.Candidate{dep}), routeMetaCtx{decisionStart: utils.Now(), attemptsSoFar: []pfallback.Attempt{
		{Outcome: pfallback.OutcomeOnDemandSkip}, {Outcome: pfallback.OutcomeCooldownSkip},
	}}, 0, 0)
	assert.Equal(t, 400, w.Code)
	assert.Equal(t, 400, outcome.attempt.StatusCode, "normalization still owns retry/logical decisions")
	require.Len(t, spans.Ended(), 1)
	attrs := map[string]int64{}
	for _, a := range spans.Ended()[0].Attributes() {
		attrs[string(a.Key)] = a.Value.AsInt64()
	}
	assert.EqualValues(t, 503, attrs["http.response.status_code"])
}

func TestTraceRetryAttemptsAreSiblings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spans := chainTraceRecorder(t)
		ctx, parent := llm.Tracer().Start(t.Context(), "logical")
		defer parent.End()
		deps := newFakeDeps()
		deps.endpoints["engine"] = "http://engine.test"
		var active []trace.SpanContext
		deps.forward["http://engine.test/v1/chat/completions"] = func() (*http.Response, error) {
			active = append(active, trace.SpanContextFromContext(deps.forwardedContext))
			if len(active) == 1 {
				return cannedJSON(503, `{"error":{"message":"busy"}}`), nil
			}
			return cannedJSON(200, `{"choices":[]}`), nil
		}
		c, w := buildGinCtx(t, `{"model":"group"}`)
		c.Request = c.Request.WithContext(ctx)
		original := c.Request
		dep := pfallback.Candidate{Name: "replica", App: "engine", Model: "m"}
		p := newProxyWithFake(deps)
		_, disposition := p.tryDeployment(c, dep, []byte(`{"model":"group"}`), nil, leaseGroup([]pfallback.Candidate{dep}), 1, routeMetaCtx{decisionStart: utils.Now()})
		require.Equal(t, attemptCommitted, disposition)
		require.Equal(t, 200, w.Code)
		require.Same(t, original, c.Request)
		require.Len(t, active, 2)
		require.Len(t, spans.Ended(), 2)
		for i, span := range spans.Ended() {
			assert.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID())
			assert.Equal(t, span.SpanContext(), active[i])
		}
		assert.NotEqual(t, active[0].SpanID(), active[1].SpanID())
		assert.Equal(t, codes.Error, spans.Ended()[0].Status().Code)
		assert.NotEqual(t, codes.Error, spans.Ended()[1].Status().Code)
	})
}
