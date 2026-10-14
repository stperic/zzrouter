package observability

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	collectorMetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectorTrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func restoreOTELGlobals(t *testing.T) {
	t.Helper()
	tp, mp, propagator := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(tp); otel.SetMeterProvider(mp); otel.SetTextMapPropagator(propagator) })
}

// All payloads and credentials in these loopback fixtures are synthetic.
type otlpReceiver struct {
	collectorTrace.UnimplementedTraceServiceServer
	traces  chan *collectorTrace.ExportTraceServiceRequest
	metrics chan *collectorMetric.ExportMetricsServiceRequest
	headers chan string
}

func (r *otlpReceiver) Export(ctx context.Context, req *collectorTrace.ExportTraceServiceRequest) (*collectorTrace.ExportTraceServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	r.headers <- strings.Join(md.Get("x-test-auth"), ",")
	r.traces <- req
	return &collectorTrace.ExportTraceServiceResponse{}, nil
}

type metricReceiver struct {
	collectorMetric.UnimplementedMetricsServiceServer
	receiver *otlpReceiver
}

func (r *metricReceiver) Export(ctx context.Context, req *collectorMetric.ExportMetricsServiceRequest) (*collectorMetric.ExportMetricsServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	r.receiver.headers <- strings.Join(md.Get("x-test-auth"), ",")
	r.receiver.metrics <- req
	return &collectorMetric.ExportMetricsServiceResponse{}, nil
}

func newOTLPReceiver(t *testing.T, protocol string) (OTLPConfig, *otlpReceiver) {
	t.Helper()
	r := &otlpReceiver{traces: make(chan *collectorTrace.ExportTraceServiceRequest, 8), metrics: make(chan *collectorMetric.ExportMetricsServiceRequest, 8), headers: make(chan string, 16)}
	if protocol == "grpc" || protocol == "grpc TLS" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		var opts []grpc.ServerOption
		if protocol == "grpc TLS" {
			certificateServer := httptest.NewTLSServer(nil)
			certPath := filepath.Join(t.TempDir(), "collector-cert.pem")
			require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateServer.Certificate().Raw}), 0600))
			t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", certPath)
			opts = append(opts, grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: certificateServer.TLS.Certificates})))
			certificateServer.Close()
		}
		server := grpc.NewServer(opts...)
		collectorTrace.RegisterTraceServiceServer(server, r)
		collectorMetric.RegisterMetricsServiceServer(server, &metricReceiver{receiver: r})
		done := make(chan struct{})
		go func() { defer close(done); _ = server.Serve(listener) }()
		t.Cleanup(func() { server.Stop(); <-done })
		return OTLPConfig{Endpoint: listener.Addr().String(), Protocol: "grpc", Insecure: protocol == "grpc"}, r
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Equal(t, "application/x-protobuf", req.Header.Get("Content-Type"))
		r.headers <- req.Header.Get("X-Test-Auth")
		switch req.URL.Path {
		case "/collector/v1/traces":
			data := new(collectorTrace.ExportTraceServiceRequest)
			require.NoError(t, proto.Unmarshal(body, data))
			r.traces <- data
		case "/collector/v1/metrics":
			data := new(collectorMetric.ExportMetricsServiceRequest)
			require.NoError(t, proto.Unmarshal(body, data))
			r.metrics <- data
		default:
			t.Errorf("unexpected export path: %s", req.URL.Path)
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	})
	var server *httptest.Server
	if protocol == "https" {
		server = httptest.NewTLSServer(handler)
		certPath := filepath.Join(t.TempDir(), "collector-cert.pem")
		require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
		t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", certPath)
	} else {
		server = httptest.NewServer(handler)
	}
	t.Cleanup(server.Close)
	return OTLPConfig{Endpoint: server.URL + "/collector/", Protocol: "http/protobuf"}, r
}

func TestOTLPExportTransports(t *testing.T) {
	for _, protocol := range []string{"grpc", "grpc TLS", "http/protobuf", "https"} {
		for _, auth := range []bool{false, true} {
			t.Run(protocol+map[bool]string{true: " headers", false: " empty headers"}[auth], func(t *testing.T) {
				restoreOTELGlobals(t)
				for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS"} {
					t.Setenv(name, "x-test-auth=environment-secret")
				}
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
				t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1")
				t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:1")
				t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/json")
				for _, name := range []string{"OTEL_EXPORTER_OTLP_INSECURE", "OTEL_EXPORTER_OTLP_TRACES_INSECURE", "OTEL_EXPORTER_OTLP_METRICS_INSECURE"} {
					t.Setenv(name, "true")
				}
				config, receiver := newOTLPReceiver(t, protocol)
				wantHeader := ""
				if auth {
					wantHeader = "configured"
					config.Headers = map[string]string{"x-test-auth": wantHeader}
				}
				p, err := NewProvider(context.Background(), &Config{Enabled: true, ServiceName: "zzrouter-e2e", OTLP: config, Tracing: TracingConfig{Enabled: true, SampleRate: 1}, Metrics: MetricsConfig{Enabled: true, ExportInterval: 3600}}, "test")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, p.Stop(context.Background())) })
				_, span := p.Tracer("fixture").Start(context.Background(), "synthetic agent")
				span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", 7))
				id := span.SpanContext()
				span.End()
				counter, err := p.Meter("fixture").Int64Counter("fixture.requests")
				require.NoError(t, err)
				counter.Add(context.Background(), 1)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				require.NoError(t, p.TracerProvider().ForceFlush(ctx))
				require.NoError(t, p.MeterProvider().ForceFlush(ctx))
				select {
				case data := <-receiver.traces:
					require.Len(t, data.ResourceSpans, 1)
					require.Len(t, data.ResourceSpans[0].ScopeSpans, 1)
					require.Len(t, data.ResourceSpans[0].ScopeSpans[0].Spans, 1)
					s := data.ResourceSpans[0].ScopeSpans[0].Spans[0]
					traceID, spanID := id.TraceID(), id.SpanID()
					assert.Equal(t, traceID[:], s.TraceId)
					assert.Equal(t, spanID[:], s.SpanId)
					assert.Equal(t, tracepb.Span_SPAN_KIND_INTERNAL, s.Kind)
					assert.Equal(t, int64(7), s.Attributes[0].Value.GetIntValue())
				case <-ctx.Done():
					t.Fatal("trace export not observed")
				}
				select {
				case data := <-receiver.metrics:
					require.NotEmpty(t, data.ResourceMetrics)
				case <-ctx.Done():
					t.Fatal("metric export not observed")
				}
				assert.Equal(t, wantHeader, <-receiver.headers)
				assert.Equal(t, wantHeader, <-receiver.headers)
			})
		}
	}
}

func TestOTLPConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, endpoint string
		insecure, wantErr        bool
	}{
		{"legacy", "", "localhost:4317", true, false}, {"empty", "grpc", "", false, false},
		{"protocol", "http/json", "", false, true}, {"grpc URL", "grpc", "https://host:4317", false, true}, {"grpc missing port", "grpc", "host", false, true},
		{"HTTP prefix", "http/protobuf", "https://host/collector", false, false}, {"userinfo", "http/protobuf", "https://secret@host", false, true},
		{"query", "http/protobuf", "https://host?secret=value", false, true}, {"fragment", "http/protobuf", "https://host#secret", false, true},
		{"TLS conflict", "http/protobuf", "https://host", true, true}, {"relative", "http/protobuf", "host:4318", false, true},
		{"double suffix", "http/protobuf", "https://host/v1/traces/v1/traces", false, true},
		{"mixed suffix", "http/protobuf", "https://host/v1/metrics/v1/traces", false, true},
		{"reverse mixed suffix", "http/protobuf", "https://host/v1/traces/v1/metrics", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Enabled: true, OTLP: OTLPConfig{Protocol: tc.protocol, Endpoint: tc.endpoint, Insecure: tc.insecure}}
			err := c.Validate()
			if tc.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "secret")
			} else {
				require.NoError(t, err)
			}
		})
	}
	for _, base := range []string{"https://host/collector", "https://host/collector/", "https://host/collector/v1/traces", "https://host/collector/v1/metrics/"} {
		c := OTLPConfig{Endpoint: base}
		for _, signal := range []string{"traces", "metrics"} {
			u, err := c.signalURL(signal)
			require.NoError(t, err)
			assert.Equal(t, "https://host/collector/v1/"+signal, u)
		}
	}
	c := OTLPConfig{Endpoint: "https://host/collector%2Ftenant"}
	_, err := c.signalURL("traces")
	require.Error(t, err, "the pinned SDK decodes escaped separators; reject rather than route to another tenant")
}

func TestOTLPEmptyEndpointAndMetricToggle(t *testing.T) {
	for _, endpoint := range []bool{false, true} {
		for _, remote := range []bool{false, true} {
			t.Run(map[bool]string{false: "local", true: "connected"}[endpoint]+map[bool]string{false: " trace only", true: " both"}[remote], func(t *testing.T) {
				restoreOTELGlobals(t)
				config, r := newOTLPReceiver(t, "http/protobuf")
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", config.Endpoint)
				if !endpoint {
					config.Endpoint = ""
				}
				p, err := NewProvider(context.Background(), &Config{Enabled: true, OTLP: config, Tracing: TracingConfig{Enabled: true, SampleRate: 1}, Metrics: MetricsConfig{Enabled: true, OTLPExport: &remote, ExportInterval: 3600}}, "test")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, p.Stop(context.Background())) })
				_, s := p.Tracer("test").Start(context.Background(), "request")
				s.End()
				counter, err := p.Meter("test").Int64Counter("local.counter")
				require.NoError(t, err)
				counter.Add(context.Background(), 1)
				require.NoError(t, p.TracerProvider().ForceFlush(context.Background()))
				require.NoError(t, p.MeterProvider().ForceFlush(context.Background()))
				assert.Equal(t, map[bool]int{true: 1, false: 0}[endpoint], len(r.traces))
				assert.Equal(t, map[bool]int{true: 1, false: 0}[endpoint && remote], len(r.metrics))
				w := httptest.NewRecorder()
				p.PrometheusHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
				assert.Regexp(t, `local_counter_total\{[^}]*\} 1`, w.Body.String())
			})
		}
	}
}

func TestOTLPDiagnosticsAndQueueDrops(t *testing.T) {
	restoreOTELGlobals(t)
	t.Setenv("OTEL_GO_X_OBSERVABILITY", "true")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered); <-release })
		w.WriteHeader(400)
	}))
	t.Cleanup(server.Close)
	remote := false
	p, err := NewProvider(context.Background(), &Config{Enabled: true, OTLP: OTLPConfig{Endpoint: server.URL, Protocol: "http/protobuf"}, Tracing: TracingConfig{Enabled: true, SampleRate: 1}, Metrics: MetricsConfig{Enabled: true, OTLPExport: &remote}}, "test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	tracer := p.Tracer("queue-test")
	for i := 0; i < spanBatchSize; i++ {
		_, s := tracer.Start(context.Background(), "request")
		s.End()
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("export did not begin")
	}
	start := utils.Now()
	for i := 0; i < spanQueueSize*2; i++ {
		_, s := tracer.Start(context.Background(), "request")
		s.End()
	}
	assert.Less(t, time.Since(start), time.Second, "span ending must not wait on exporter recovery")
	families, err := p.promRegistry.Gather()
	require.NoError(t, err)
	var capacity, drops float64
	for _, family := range families {
		for _, m := range family.Metric {
			if family.GetName() == "otel_sdk_processor_span_queue_capacity" {
				capacity += m.GetGauge().GetValue()
			}
			if family.GetName() == "otel_sdk_processor_span_processed_total" {
				for _, label := range m.Label {
					if label.GetName() == "error_type" && label.GetValue() == "queue_full" {
						drops += m.GetCounter().GetValue()
					}
				}
			}
		}
	}
	assert.Equal(t, float64(spanQueueSize), capacity)
	assert.Greater(t, drops, float64(0))
	unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = p.TracerProvider().ForceFlush(ctx)
	families, err = p.promRegistry.Gather()
	require.NoError(t, err)
	var failures float64
	for _, family := range families {
		if family.GetName() == "otel_sdk_exporter_span_exported_total" {
			for _, m := range family.Metric {
				for _, label := range m.Label {
					if label.GetName() == "error_type" && label.GetValue() != "" {
						failures += m.GetCounter().GetValue()
					}
				}
			}
		}
	}
	assert.Greater(t, failures, float64(0), "export failure diagnostics must use the owned meter")
}

func TestOTLPPartialConstructorRollback(t *testing.T) {
	leakBaseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leakBaseline) })
	restoreOTELGlobals(t)
	previousMeter := otel.GetMeterProvider()
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", "OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
	} {
		t.Setenv(name, "")
	}
	certificateServer := httptest.NewTLSServer(nil)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateServer.Certificate().Raw})
	certificateServer.Close()
	certificatePath := filepath.Join(t.TempDir(), "trace-only-ca.pem")
	require.NoError(t, os.WriteFile(certificatePath, certificate, 0600))
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", certificatePath)
	transport, _ := newOTLPReceiver(t, "http/protobuf")
	// Metrics initializes its reader first. Only tracing has TLS configured,
	// which the SDK rejects for this plaintext endpoint during construction.
	provider, err := NewProvider(t.Context(), &Config{
		Enabled: true, OTLP: transport,
		Metrics: MetricsConfig{Enabled: true},
		Tracing: TracingConfig{Enabled: true, SampleRate: 1},
	}, "test")
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to initialize tracing")
	require.ErrorContains(t, err, "insecure HTTP endpoint cannot use TLS client configuration")
	require.Nil(t, provider)
	require.Same(t, previousMeter, otel.GetMeterProvider())
}

func TestOTLPShutdownDeadline(t *testing.T) {
	leakBaseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leakBaseline) })
	restoreOTELGlobals(t)
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		defer close(exited)
		<-release
		w.WriteHeader(400)
	}))
	t.Cleanup(server.Close)
	p, err := NewProvider(context.Background(), &Config{Enabled: true, OTLP: OTLPConfig{Endpoint: server.URL, Protocol: "http/protobuf"}, Tracing: TracingConfig{Enabled: true, SampleRate: 1}}, "test")
	require.NoError(t, err)
	_, s := p.Tracer("test").Start(context.Background(), "request")
	s.End()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("export did not begin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := utils.Now()
	require.ErrorIs(t, p.Stop(ctx), context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second)
	close(release)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("receiver did not exit")
	}
	// Provider Stop is idempotent; SDK drain ownership continues after a deadline.
	require.NoError(t, p.Stop(context.Background()))
}

func TestOTLPOutageAndRecovery(t *testing.T) {
	restoreOTELGlobals(t)
	var recovered atomic.Bool
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !recovered.Load() {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(server.Close)
	p, err := NewProvider(context.Background(), &Config{Enabled: true, OTLP: OTLPConfig{Endpoint: server.URL, Protocol: "http/protobuf"}, Tracing: TracingConfig{Enabled: true, SampleRate: 1}}, "test")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, p.Stop(context.Background())) })
	_, s := p.Tracer("test").Start(context.Background(), "outage")
	s.End()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := utils.Now()
	require.Error(t, p.TracerProvider().ForceFlush(ctx))
	assert.Less(t, time.Since(start), 7*time.Second, "exporter retry must remain bounded")
	assert.GreaterOrEqual(t, calls.Load(), int32(2), "503 should trigger background retry")
	recovered.Store(true)
	_, s = p.Tracer("test").Start(context.Background(), "recovery")
	s.End()
	require.NoError(t, p.TracerProvider().ForceFlush(ctx))
}
