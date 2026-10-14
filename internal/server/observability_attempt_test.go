package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/observability"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func attemptSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	preserveTraceGlobals(t)
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	return spans
}

// The fixture is synthetic; it blocks after headers to distinguish body lifetime from dial time.
func TestTraceProxyAttemptBodyLifetimeAndPropagation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[stream], func(t *testing.T) {
			spans := attemptSpanRecorder(t)
			ctx, parent := llm.Tracer().Start(t.Context(), "HTTP parent")
			defer parent.End()
			headers, release := make(chan trace.SpanContext, 1), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Empty(t, r.Header.Get("Baggage"))
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				active := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(r.Header))
				headers <- trace.SpanContextFromContext(active) //nolint:contextcheck // Decode the observed wire carrier independently of the server context.
				<-release
				if stream {
					_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
				} else {
					_, _ = io.WriteString(w, `{"choices":[]}`)
				}
			}))
			defer upstream.Close()
			defer close(release)
			pc := NewProxyClient(upstream.Client(), nil, NewResponseNormalizers(), nil, nil, nil, nil)
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`)).WithContext(ctx)
			req.Header.Set("Traceparent", "stale")
			req.Header.Set("Baggage", "secret=private-secret")
			done := make(chan struct{})
			w := httptest.NewRecorder()
			go func() {
				defer close(done)
				pc.ForwardToBackend(w, req, "engine", upstream.URL, backend.Engine(), []byte(`{}`))
			}()
			var active trace.SpanContext
			select {
			case active = <-headers:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream did not receive the request")
			}
			require.True(t, active.IsValid())
			require.Empty(t, spans.Ended(), "attempt must remain open while the body is blocked")
			release <- struct{}{}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("relay did not finish")
			}
			require.Equal(t, 200, w.Code)
			require.Len(t, spans.Ended(), 1)
			attempt := spans.Ended()[0]
			assert.Equal(t, "llm.attempt", attempt.Name())
			assert.Equal(t, active.TraceID(), attempt.SpanContext().TraceID())
			assert.Equal(t, active.SpanID(), attempt.SpanContext().SpanID())
			assert.Equal(t, parent.SpanContext().SpanID(), attempt.Parent().SpanID())
			assert.NotEqual(t, codes.Error, attempt.Status().Code)
		})
	}
}

// Exercise the real worker listener's mTLS/OU gate and compat engine, without a deployed node.
func TestTraceWorkerMTLSParentage(t *testing.T) {
	preserveTraceGlobals(t)
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	paths := clusternode.PathsFromConfigDir(pkgConfig.Paths().GetConfigDir())
	ca, err := clusterid.LoadOrCreateCA(filepath.Join(t.TempDir(), "ca"))
	require.NoError(t, err)
	sign := func(dir string, role clusterid.Role) *clusterid.Identity {
		identity, err := clusterid.LoadOrCreateIdentity(dir)
		require.NoError(t, err)
		csr, err := identity.CSR("trace-node", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
		require.NoError(t, err)
		cert, err := ca.Sign(csr, role)
		require.NoError(t, err)
		require.NoError(t, identity.InstallSignedCert(cert))
		return identity
	}
	sign(paths.IdentityDir, clusterid.RoleWorker)
	coord := sign(filepath.Join(t.TempDir(), "coord"), clusterid.RoleCoordinator)
	require.NoError(t, os.MkdirAll(paths.ClusterDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(paths.ClusterDir, "ca.pem"), ca.CertPEM(), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(paths.ClusterDir, "coordinator_url"), []byte("https://127.0.0.1:1"), 0600))
	cfg := &pkgConfig.NodeConfig{
		Node:          pkgConfig.ServeConfig{Bind: "localhost", Port: getNextTestPort(), Name: "trace-worker"},
		Cluster:       pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeWorker, BindPort: 0, BindAddr: "127.0.0.1"},
		Auth:          pkgConfig.AuthConfig{AdminKey: TestAdminKey},
		Observability: pkgConfig.ObservabilityConfig{Enabled: true, ServiceName: "trace-worker", Tracing: pkgConfig.TracingConfig{Enabled: true, SampleRate: 1}},
	}
	worker, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(worker); require.NoError(t, worker.otelProvider.Stop(context.Background())) })
	require.Equal(t, clusternode.Worker, worker.cluster.listener.Mode())
	spans := tracetest.NewSpanRecorder()
	worker.otelProvider.TracerProvider().RegisterSpanProcessor(spans)
	require.NoError(t, worker.cluster.listener.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, worker.cluster.listener.Stop(context.Background())) })
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{{Certificate: [][]byte{coord.Certificate().Raw}, PrivateKey: coord.Signer()}}}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	pc := NewProxyClient(nil, func() *http.Client { return client }, NewResponseNormalizers(), nil, nil, nil, nil)
	ctx, parent := worker.otelProvider.Tracer("agent-fixture").Start(t.Context(), "agent")
	defer parent.End()
	req := httptest.NewRequest("GET", "/v1/models", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	pc.ForwardToBackend(w, req, "worker", "https://"+worker.cluster.listener.Addr().String()+"/v1/models", backend.Cluster(), nil)
	require.Equal(t, 200, w.Code, w.Body.String())
	var attempt, server sdktrace.ReadOnlySpan
	for _, span := range spans.Ended() {
		if span.Name() == "llm.attempt" {
			attempt = span
		}
		if span.SpanKind() == trace.SpanKindServer {
			server = span
		}
	}
	require.NotNil(t, attempt)
	require.NotNil(t, server)
	assert.Equal(t, parent.SpanContext().SpanID(), attempt.Parent().SpanID())
	assert.Equal(t, attempt.SpanContext().SpanID(), server.Parent().SpanID())
	assert.Equal(t, parent.SpanContext().TraceID(), server.SpanContext().TraceID())
}

func TestTraceRealtimeHandshakeStatusAndPropagation(t *testing.T) {
	for _, status := range []int{403, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := createTestNodeWithDefaults(t)
			spans := attemptSpanRecorder(t)
			var observed trace.SpanContext
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				decoded := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(r.Header))
				observed = trace.SpanContextFromContext(decoded) //nolint:contextcheck // Decode the observed wire parent independently.
				assert.Empty(t, r.Header.Get("Baggage"))
				w.WriteHeader(status)
			}))
			defer upstream.Close()
			copied := *server.config
			copied.OpenAICompat.RealtimeBackend = "realtime-fixture"
			server.config = &copied
			server.appsConfig = &pkgConfig.AppsConfig{Version: "1.0", Name: "realtime-test"}
			require.NoError(t, server.appsConfig.AddApp("realtime-fixture", pkgConfig.ServiceConfig{
				Enabled: new(true), Name: "realtime-fixture", Mode: "service", Protocol: pkgConfig.ProtocolOpenAI,
				Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: upstream.URL},
				Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
			}))
			router := gin.New()
			router.Use(observability.TracingMiddleware("realtime-test"))
			router.GET("/v1/realtime", server.handleRealtimeUpgrade)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/v1/realtime", nil))
			require.NotEqual(t, 200, w.Code)
			require.Len(t, spans.Ended(), 2)
			attempt, httpSpan := spans.Ended()[0], spans.Ended()[1]
			assert.Equal(t, "llm.attempt", attempt.Name())
			assert.Equal(t, httpSpan.SpanContext().SpanID(), attempt.Parent().SpanID())
			assert.Equal(t, observed.SpanID(), attempt.SpanContext().SpanID())
			assert.Equal(t, observed.TraceID(), attempt.SpanContext().TraceID())
			assert.Equal(t, codes.Error, attempt.Status().Code)
			var gotStatus int64
			for _, a := range attempt.Attributes() {
				if a.Key == "http.response.status_code" {
					gotStatus = a.Value.AsInt64()
				}
			}
			assert.EqualValues(t, status, gotStatus)
		})
	}
}
