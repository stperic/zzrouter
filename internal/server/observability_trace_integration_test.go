package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracecollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func preserveTraceGlobals(t testing.TB) {
	t.Helper()
	tp, mp, prop := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(tp); otel.SetMeterProvider(mp); otel.SetTextMapPropagator(prop) })
}

func TestTraceServerConfigAndParentage(t *testing.T) {
	preserveTraceGlobals(t)
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	exports := make(chan *tracecollector.ExportTraceServiceRequest, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/prefix/v1/traces", r.URL.Path, "metrics.otlp_export=false must prevent metric traffic")
		assert.Equal(t, "synthetic", r.Header.Get("X-Test-Auth"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		data := new(tracecollector.ExportTraceServiceRequest)
		require.NoError(t, proto.Unmarshal(body, data))
		exports <- data
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(receiver.Close)
	remote := false
	cfg := &pkgConfig.NodeConfig{
		Node:          pkgConfig.ServeConfig{Bind: "localhost", Port: getNextTestPort(), Name: "trace-fixture"},
		Cluster:       pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeDisabled},
		Auth:          pkgConfig.AuthConfig{AdminKey: TestAdminKey},
		Observability: pkgConfig.ObservabilityConfig{Enabled: true, ServiceName: "zzrouter-e2e", OTLP: pkgConfig.OTLPConfig{Endpoint: receiver.URL + "/prefix", Protocol: "http/protobuf", Headers: map[string]string{"x-test-auth": "synthetic"}}, Tracing: pkgConfig.TracingConfig{Enabled: true, SampleRate: 0}, Metrics: pkgConfig.MetricsConfig{Enabled: true, OTLPExport: &remote}},
	}
	s, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	require.NotNil(t, s.otelProvider)
	t.Cleanup(func() { cleanupTestNode(s); require.NoError(t, s.otelProvider.Stop(context.Background())) })
	recorder := tracetest.NewSpanRecorder()
	s.otelProvider.TracerProvider().RegisterSpanProcessor(recorder)
	parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
	req := httptest.NewRequest("GET", "/zzrouter/v1", nil)
	req.Header.Set("X-API-Key", TestAdminKey)
	propagation.TraceContext{}.Inject(trace.ContextWithSpanContext(context.Background(), parent), propagation.HeaderCarrier(req.Header))
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	require.Len(t, recorder.Ended(), 1)
	span := recorder.Ended()[0]
	assert.Equal(t, parent.TraceID(), span.SpanContext().TraceID())
	assert.Equal(t, parent.SpanID(), span.Parent().SpanID())
	assert.Equal(t, "GET /zzrouter/v1", span.Name())
	require.NoError(t, s.otelProvider.TracerProvider().ForceFlush(context.Background()))
	require.NoError(t, s.otelProvider.MeterProvider().ForceFlush(context.Background()))
	select {
	case data := <-exports:
		require.NotEmpty(t, data.ResourceSpans)
	case <-time.After(2 * time.Second):
		t.Fatal("server export not received")
	}
	assert.Empty(t, exports, "server conversion lost remote-metric toggle")
	w = httptest.NewRecorder()
	s.engine.ServeHTTP(w, httptest.NewRequest("GET", "/health/live", nil))
	require.Equal(t, 200, w.Code)
	assert.Len(t, recorder.Ended(), 1, "health must not create traces")
}
