package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func traceFixture(t *testing.T) (*tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	restoreOTELGlobals(t)
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
		require.NoError(t, mp.Shutdown(context.Background()))
	})
	return recorder, reader
}

func TestTraceHTTPPrivacyAndParentage(t *testing.T) {
	spans, _ := traceFixture(t)
	secret := "private-credential-" + strings.Repeat("x", 10000)
	router := gin.New()
	router.Use(TracingMiddleware("router-test"))
	router.GET("/models/:id", func(c *gin.Context) { _ = c.Error(errors.New(secret)); c.Status(503) })
	parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled, Remote: true})
	req := httptest.NewRequest("GET", "/models/"+secret+"?key="+secret, nil)
	req.Header.Set("User-Agent", secret)
	req.Header.Set("X-Forwarded-For", secret)
	propagation.TraceContext{}.Inject(trace.ContextWithSpanContext(context.Background(), parent), propagation.HeaderCarrier(req.Header))
	original := req.Context()
	router.ServeHTTP(httptest.NewRecorder(), req)
	ended := spans.Ended()
	require.Len(t, ended, 1)
	span := ended[0]
	assert.Equal(t, parent.TraceID(), span.SpanContext().TraceID())
	assert.Equal(t, parent.SpanID(), span.Parent().SpanID())
	assert.Equal(t, "GET /models/:id", span.Name())
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.NotContains(t, span.Status().Description, "private-credential")
	for _, attr := range span.Attributes() {
		assert.NotContains(t, attr.Value.String(), "private-credential", attr.Key)
	}
	assert.Empty(t, span.Events())
	assert.Equal(t, original, req.Context())
}

func TestTraceHTTPStatusAndExclusions(t *testing.T) {
	spans, _ := traceFixture(t)
	router := gin.New()
	router.Use(TracingMiddleware("router-test"))
	router.GET("/bad", func(c *gin.Context) { c.Status(400) })
	router.GET("/health/live", func(c *gin.Context) { c.Status(200) })
	router.GET("/metrics", func(c *gin.Context) { c.Status(200) })
	for _, path := range []string{"/bad", "/unknown?secret=private", "/health/live", "/metrics"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	ended := spans.Ended()
	require.Len(t, ended, 2)
	assert.Equal(t, codes.Unset, ended[0].Status().Code)
	assert.Equal(t, "GET unmatched", ended[1].Name())
}

func TestTraceHTTPOriginalCancellationAndRestoration(t *testing.T) {
	spans, _ := traceFixture(t)
	router := gin.New()
	router.Use(TracingMiddleware("router-test"))
	ctx, cancel := context.WithCancel(context.Background())
	router.GET("/cancel", func(c *gin.Context) { cancel(); c.Request = c.Request.WithContext(context.Background()); c.Status(200) })
	req := httptest.NewRequest("GET", "/cancel", nil).WithContext(ctx)
	router.ServeHTTP(httptest.NewRecorder(), req)
	require.Len(t, spans.Ended(), 1)
	assert.Equal(t, codes.Error, spans.Ended()[0].Status().Code)
	assert.Equal(t, "client cancellation", spans.Ended()[0].Status().Description)
	assert.ErrorIs(t, req.Context().Err(), context.Canceled)
}

func TestTraceHTTPMetricsNestedRequests(t *testing.T) {
	_, reader := traceFixture(t)
	router := gin.New()
	router.Use(TracingMiddleware("router-test"))
	router.POST("/inner", func(c *gin.Context) { c.String(200, "inner") })
	router.POST("/mcp", func(c *gin.Context) {
		inner := httptest.NewRequest("POST", "/inner", strings.NewReader("{}"))
		router.ServeHTTP(httptest.NewRecorder(), inner)
		c.String(200, "outer")
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/mcp", strings.NewReader("{}")))
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	counts := map[string]uint64{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch h := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, p := range h.DataPoints {
					counts[m.Name] += p.Count
				}
			case metricdata.Histogram[int64]:
				for _, p := range h.DataPoints {
					counts[m.Name] += p.Count
				}
			}
		}
	}
	for _, name := range []string{"http.server.request.duration", "http.server.request.body.size", "http.server.response.body.size"} {
		assert.Equal(t, uint64(2), counts[name], name)
	}
}

func TestTraceParentSampling(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rate         float64
		flags        trace.TraceFlags
		parent, want bool
	}{
		{"zero root", 0, 0, false, false}, {"zero sampled parent", 0, trace.FlagsSampled, true, true}, {"one unsampled parent", 1, 0, true, false}, {"one root", 1, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreOTELGlobals(t)
			p, err := NewProvider(context.Background(), &Config{Enabled: true, Tracing: TracingConfig{Enabled: true, SampleRate: tc.rate}}, "test")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, p.Stop(context.Background())) })
			ctx := context.Background()
			if tc.parent {
				ctx = trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: tc.flags}))
			}
			_, s := p.Tracer("test").Start(ctx, "request")
			assert.Equal(t, tc.want, s.IsRecording())
			assert.True(t, s.SpanContext().IsValid())
			s.End()
		})
	}
}

func TestTraceHTTPPanicAndUnknownMethod(t *testing.T) {
	spans, reader := traceFixture(t)
	router := gin.New()
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) { c.AbortWithStatus(500) }), TracingMiddleware("router-test"))
	router.GET("/panic", func(c *gin.Context) { panic("credential-bearing panic") })
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/panic", nil))
	req := httptest.NewRequest("CUSTOM-SECRET", "/credential-path", nil)
	router.ServeHTTP(httptest.NewRecorder(), req)
	ended := spans.Ended()
	require.Len(t, ended, 2)
	assert.Equal(t, codes.Error, ended[0].Status().Code)
	assert.Equal(t, "internal failure", ended[0].Status().Description)
	assert.Equal(t, "_OTHER unmatched", ended[1].Name())
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	var count uint64
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "http.server.request.duration" {
				for _, p := range m.Data.(metricdata.Histogram[float64]).DataPoints {
					count += p.Count
				}
			}
		}
	}
	assert.Equal(t, uint64(2), count)
}

func TestTraceHTTPPanicAfterStreamingHeaders(t *testing.T) {
	spans, _ := traceFixture(t)
	router := gin.New()
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) { c.Abort() }), TracingMiddleware("router-test"))
	router.GET("/stream", func(c *gin.Context) { c.String(200, "partial"); c.Writer.Flush(); panic("private panic") })
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/stream", nil))
	assert.Equal(t, 200, w.Code)
	require.Len(t, spans.Ended(), 1)
	span := spans.Ended()[0]
	assert.Equal(t, codes.Error, span.Status().Code)
	for _, attr := range span.Attributes() {
		if attr.Key == "http.response.status_code" {
			assert.Equal(t, int64(200), attr.Value.AsInt64())
		}
	}
}

func TestTraceHTTPAbortHandlerIsCancellation(t *testing.T) {
	spans, _ := traceFixture(t)
	router := gin.New()
	router.Use(TracingMiddleware("test"))
	router.GET("/abort", func(c *gin.Context) { panic(http.ErrAbortHandler) })
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/abort", nil))
	})
	require.Len(t, spans.Ended(), 1)
	assert.Equal(t, "client cancellation", spans.Ended()[0].Status().Description)
}
