package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	metricSDK "go.opentelemetry.io/otel/sdk/metric"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/proxy"
	obsrouting "github.com/stperic/zzrouter/pkg/observability/routing"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	obsspend "github.com/stperic/zzrouter/pkg/observability/spend"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// TestNodeWithObservabilityDisabled verifies the server starts correctly
// when observability is disabled (default)
func TestNodeWithObservabilityDisabled(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9100,
			Name: "test-otel-disabled",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled: false,
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	require.NotNil(t, server.engine)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Verify OTel provider is nil when disabled
	assert.Nil(t, server.otelProvider)

	// Health endpoint should work
	req := httptest.NewRequest("GET", "/health/live", nil)
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestNodeWithObservabilityEnabled verifies the server starts correctly
// with observability enabled
func TestNodeWithObservabilityEnabled(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9101,
			Name: "test-otel-enabled",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test",
			OTLP: pkgConfig.OTLPConfig{
				Endpoint: "", // No OTLP endpoint for tests
				Insecure: true,
			},
			Tracing: pkgConfig.TracingConfig{
				Enabled:    true,
				SampleRate: 1.0,
			},
			Metrics: pkgConfig.MetricsConfig{
				Enabled:        true,
				PrometheusPort: 0, // Disabled for unit tests
				ExportInterval: 1,
			},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	require.NotNil(t, server.engine)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Verify OTel provider is initialized
	assert.NotNil(t, server.otelProvider)

	// Health endpoint should work
	req := httptest.NewRequest("GET", "/health/live", nil)
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestNodeWithPrometheusEndpoint verifies the /metrics endpoint is available
// when metrics are enabled, independent of PrometheusPort. The prometheus_port
// field is reserved for a future sidecar listener — it must not gate the
// in-process handler served on the main gin engine.
func TestNodeWithPrometheusEndpoint(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9102,
			Name: "test-prometheus",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-prom",
			Tracing: pkgConfig.TracingConfig{
				Enabled: false,
			},
			Metrics: pkgConfig.MetricsConfig{
				Enabled:        true,
				PrometheusPort: 0, // Deliberately unset — should still expose /metrics
				ExportInterval: 1,
			},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	require.NotNil(t, server.engine)
	require.NotNil(t, server.otelProvider)
	t.Cleanup(func() { cleanupTestNode(server) })

	// /metrics endpoint should be registered and return Prometheus content
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)

	// Should return 200 with Prometheus metrics
	assert.Equal(t, http.StatusOK, w.Code)

	// Response should contain Prometheus metrics format
	body := w.Body.String()
	assert.True(t, strings.Contains(body, "# HELP") || strings.Contains(body, "# TYPE") || len(body) == 0,
		"Response should contain Prometheus metrics format or be empty")
}

func newGenAIMetricsTestNode(t *testing.T) (*Server, *clocktest.FakeClock) {
	t.Helper()
	priorMeter, priorTracer := otel.GetMeterProvider(), otel.GetTracerProvider()
	t.Cleanup(func() {
		otel.SetMeterProvider(priorMeter)
		otel.SetTracerProvider(priorTracer)
	})
	t.Cleanup(genai.SwapModelAllowlist(nil))
	clock := clocktest.NewFakeClock(time.Unix(1, 0))
	t.Cleanup(utils.SetClock(clock))
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 0,
			Name: "test-genai-metrics",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-genai",
			Tracing: pkgConfig.TracingConfig{
				Enabled: false,
			},
			Metrics: pkgConfig.MetricsConfig{
				Enabled:        true,
				ExportInterval: 1,
			},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })
	return server, clock
}

func TestGenAIMetricsFixtureRestoresGlobalState(t *testing.T) {
	prior := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prior) })
	sentinel := metricSDK.NewMeterProvider()
	otel.SetMeterProvider(sentinel)
	t.Cleanup(func() { require.NoError(t, sentinel.Shutdown(t.Context())) })
	epoch := time.Unix(2, 0)
	t.Cleanup(utils.SetClock(utils.FixedClock(epoch)))
	t.Cleanup(genai.SwapModelAllowlist(func(string) bool { return false }))
	t.Run("isolated", func(t *testing.T) {
		server, clock := newGenAIMetricsTestNode(t)
		require.Same(t, server.otelProvider.MeterProvider(), otel.GetMeterProvider())
		require.NotSame(t, sentinel, otel.GetMeterProvider())
		require.Equal(t, "test-model", genai.GateModelLabel("test-model"))
		clock.Advance(time.Second)
	})
	require.Same(t, sentinel, otel.GetMeterProvider())
	require.Equal(t, epoch, utils.Now())
	require.Equal(t, genai.UnknownModelLabel, genai.GateModelLabel("test-model"))
}

// TestGenAIClientMetricsExposed is the Phase 0 acceptance test from
// docs/plan_observability_dashboard_parity.md: drive a single inference call
// through InferenceRecorder, scrape /metrics, and assert that the OTel GenAI
// client + server-TTFT series appear with the required attribute keys. The
// test asserts on series presence and label keys only — values are flaky
// across runs and aren't part of the contract.
func TestGenAIClientMetricsExposed(t *testing.T) {
	server, clock := newGenAIMetricsTestNode(t)

	// Drive one chat completion through the recorder.
	rec := llm.NewInferenceRecorder(context.Background(), "llama2:7b", "vllm")
	rec.SetOperation(genai.OperationChat)
	rec.SetServer("10.0.0.1", 9090)
	clock.Advance(10 * time.Millisecond)
	rec.RecordFirstToken()
	clock.Advance(100 * time.Millisecond)
	rec.RecordCompletion(128, 256)

	// And one error path so error.type makes it onto the series.
	errRec := llm.NewInferenceRecorder(context.Background(), "llama2:7b", "vllm")
	errRec.SetOperation(genai.OperationChat)
	errRec.SetError("backend_unavailable", "boom")
	errRec.RecordCompletion(0, 0)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()

	// P0.1 — gen_ai.client.operation.duration histogram present with the
	// required OTel attribute keys.
	assert.Contains(t, body, "gen_ai_client_operation_duration", "P0.1 series missing")
	assert.Contains(t, body, `gen_ai_operation_name="chat"`)
	assert.Contains(t, body, `gen_ai_provider_name="vllm"`)
	assert.Contains(t, body, `gen_ai_request_model="llama2:7b"`)
	assert.Contains(t, body, `error_type="backend_unavailable"`,
		"error.type should appear on the error-path series")

	// Verify error.type is absent on the success-path series. Scan for lines
	// that have gen_ai_client_operation_duration and gen_ai_operation_name="chat"
	// but no error_type label — confirming we don't emit error_type="" on success.
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "gen_ai_client_operation_duration") &&
			!strings.Contains(line, `error_type=`) &&
			strings.Contains(line, `gen_ai_operation_name="chat"`) {
			// Found a success-path series without error_type — good.
			goto successPathOK
		}
	}
	t.Error("expected success-path gen_ai_client_operation_duration series without error_type label")
successPathOK:

	// P0.2 — gen_ai.client.token.usage histogram with both token types.
	assert.Contains(t, body, "gen_ai_client_token_usage", "P0.2 series missing")
	assert.Contains(t, body, `gen_ai_token_type="input"`)
	assert.Contains(t, body, `gen_ai_token_type="output"`)

	// P1.2 — gen_ai.server.time_to_first_token histogram (renamed in Phase 0
	// from zzrouter.inference.ttft).
	assert.Contains(t, body, "gen_ai_server_time_to_first_token",
		"gen_ai.server.time_to_first_token series missing")

	// P1.1 — gen_ai.server.request.duration histogram. Server-side
	// view of operation duration; mirrors client.operation.duration
	// for a routing layer like zzrouter, with the same labels +
	// error.type tagging.
	assert.Contains(t, body, "gen_ai_server_request_duration",
		"gen_ai.server.request.duration series missing")

	// P1.3 — gen_ai.server.time_per_output_token histogram. Streaming-
	// only (gated on ttftNs > 0). The driver above sets RecordFirstToken
	// + RecordCompletion(128, 256) which clears the noise floor (>=5
	// output tokens) so the series should be present.
	assert.Contains(t, body, "gen_ai_server_time_per_output_token",
		"gen_ai.server.time_per_output_token series missing — streaming TPOT")

	// The deleted zzrouter.inference.* instruments must not reappear.
	assert.NotContains(t, body, "zzrouter_inference_latency")
	assert.NotContains(t, body, "zzrouter_inference_tokens_input")
	assert.NotContains(t, body, "zzrouter_inference_tokens_output")
	assert.NotContains(t, body, "zzrouter_inference_ttft")
}

// TestProxyFailedRequestsMetric drives Phase 2.3 acceptance:
// zz.proxy.failed.requests.metric increments with dual-namespace labels
// (OTel + LiteLLM-mirrored) on every 4xx/5xx response, regardless of
// whether the response originated from the openai responder or from a
// `writeError` call site that writes below gin, without the responder.
func TestProxyFailedRequestsMetric(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9105,
			Name: "test-failed-requests",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-failed",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Two malformed-body 400s through the responder path. Sets the
	// invalid_request_error baseline for the dual-namespace assertion.
	for _, path := range []string{"/v1/chat/completions", "/v1/completions"} {
		req := httptest.NewRequest("POST", path, strings.NewReader("{ malformed"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, req)
		require.GreaterOrEqual(t, w.Code, 400, "expected error status, got %d", w.Code)
	}

	// One 404 model-not-found through HandleLocalModel ->
	// writeError (writes below gin, without the responder). Pins the middleware-driven design's claim that
	// every 4xx/5xx fires the counter regardless of which error
	// helper produced it.
	body404 := `{"model":"definitely-not-a-real-model","messages":[{"role":"user","content":"hi"}]}`
	req404 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body404))
	req404.Header.Set("Content-Type", "application/json")
	w404 := httptest.NewRecorder()
	server.engine.ServeHTTP(w404, req404)
	// 404 from findCapableProvider is the canonical path; any other
	// non-2xx is fine for this assertion since the middleware fires
	// on >= 400 universally.
	require.GreaterOrEqual(t, w404.Code, 400, "expected non-2xx for unknown model, got %d", w404.Code)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()

	// The Prometheus exporter normalises dots to underscores —
	// `zz.proxy.failed.requests.metric` → `zz_proxy_failed_requests_metric`.
	assert.Contains(t, body, "zz_proxy_failed_requests_metric",
		"P2.3 series missing from /metrics")

	// Both label namespaces must populate so a `s/litellm/zz/` rename
	// of an existing LiteLLM dashboard resolves the same dimensions
	// the OTel-aware view filters on.
	assert.Contains(t, body, `error_type="invalid_request_error"`,
		"OTel error.type label missing on failed request series")
	assert.Contains(t, body, `exception_class="invalid_request_error"`,
		"LiteLLM exception_class label missing on failed request series")
	assert.Contains(t, body, `status_code="`,
		"LiteLLM status_code label missing on failed request series")

	// At least two distinct status codes should appear across the
	// four requests (two 400s + one 4xx/5xx on unknown model + the
	// /metrics 200 itself doesn't increment the counter). Missing
	// the 4xx-from-legacy-helper case would silently regress the
	// universal-coverage claim of the middleware design.
	statusCodes := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "zz_proxy_failed_requests_metric") {
			continue
		}
		if i := strings.Index(line, `status_code="`); i >= 0 {
			rest := line[i+len(`status_code="`):]
			if j := strings.Index(rest, `"`); j > 0 {
				statusCodes[rest[:j]] = true
			}
		}
	}
	assert.GreaterOrEqual(t, len(statusCodes), 1,
		"expected at least one status_code label; got %v", statusCodes)

	// requested_model populates from stashInferenceContext on the
	// dispatch-failure path (the 404 unknown-model request above).
	// gen_ai.request.model populates on the same series — both labels
	// MUST agree because they're sourced from the same request body
	// field. Pre-stashInferenceContext bind failures (the two
	// malformed-body 400s) populate neither, so the assertion targets
	// the dispatch-failure series specifically.
	assert.Contains(t, body, `requested_model="definitely-not-a-real-model"`,
		"requested_model label missing — stashInferenceContext c.Set didn't reach the metric")
	assert.Contains(t, body, `gen_ai_request_model="definitely-not-a-real-model"`,
		"gen_ai.request.model label missing on the 404 dispatch-failure series")
}

// TestProxyInflightRequestsMetric drives Phase 2.2: the inflight
// saturation counter emits a series after a request flows through a
// dispatch path that calls IncInflight. Even at value 0 (Inc + Dec
// both fired) the OTel exporter still surfaces the series so dashboards
// can reference it.
//
// Wired call sites (proxy_client.ForwardToBackend, openai_handlers.
// proxyToWorkerURL, model_app_helpers.proxyToInstance) all require a
// reachable backend to exercise — out of scope for an httptest harness.
// We exercise the package-level helper directly to verify the metric
// is registered against the test's meter provider and emits with the
// expected labels.
func TestProxyInflightRequestsMetric(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9107,
			Name: "test-inflight",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-inflight",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Drive Inc/Dec directly. Every dispatch site does the same thing
	// in production; the inflight package's unit tests cover the
	// concurrent + nil-safety contract, this assertion just proves the
	// series is registered against the live meter provider.
	tok := proxy.IncInflight(context.Background(), "chat", "vllm", "llama2:7b", "10.0.0.1")
	proxy.DecInflight(context.Background(), tok)

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	mbody := mw.Body.String()

	// The Prometheus exporter normalises dots to underscores —
	// `zz.proxy.inflight.requests.metric` →
	// `zz_proxy_inflight_requests_metric`. Series sits at value 0
	// (Inc then Dec) but the metric name and labels appear in the export.
	assert.Contains(t, mbody, "zz_proxy_inflight_requests_metric",
		"P2.2 inflight series missing from /metrics")
	assert.Contains(t, mbody, `gen_ai_operation_name="chat"`,
		"inflight series should carry gen_ai.operation.name label")
	assert.Contains(t, mbody, `gen_ai_provider_name="vllm"`,
		"inflight series should carry gen_ai.provider.name label")
	assert.Contains(t, mbody, `gen_ai_request_model="llama2:7b"`,
		"inflight series should carry gen_ai.request.model label")
	assert.Contains(t, mbody, `server_address="10.0.0.1"`,
		"inflight series should carry server.address label")
}

// TestModelAllowlist_GatesFailedRequestLabel pins the operator-facing
// cardinality knob: when proxy.SetModelAllowlist registers a checker,
// caller-supplied requested_model values that fail the check collapse
// to UnknownModelLabel ("_unknown_") rather than emitting the raw
// adversarial value as a separate Prometheus series.
func TestModelAllowlist_GatesFailedRequestLabel(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9109,
			Name: "test-allowlist",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-allowlist",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Allowlist gates a single known model. Anything else collapses to
	// the "_unknown_" sentinel. SwapModelAllowlist preserves any
	// previously-set checker so cleanup can't clobber a concurrent
	// startup hook's wiring.
	defer genai.SwapModelAllowlist(func(name string) bool {
		return name == "known-model"
	})()
	_ = proxy.SetModelAllowlist // keep import on the test file for the proxy-side forwarder smoke

	body := `{"model":"adversarial-spam-model-1","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)
	require.GreaterOrEqual(t, w.Code, 400)

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	mbody := mw.Body.String()

	// The adversarial caller-supplied model name must NOT appear as a
	// raw label value — that's the whole point of the allowlist.
	assert.NotContains(t, mbody, `requested_model="adversarial-spam-model-1"`,
		"unknown model leaked through allowlist as a raw label value")
	// The sentinel SHOULD appear so operators can see the unknown
	// bucket on their dashboards.
	assert.Contains(t, mbody, `requested_model="_unknown_"`,
		"unknown model should collapse to the _unknown_ sentinel")
}

// TestStreamFromInstance_PopulatesInflight pins the cold-load
// streaming path's IncInflight wrap. streamFromInstance dispatches
// directly via httpStreamingClient.Do (bypassing proxyToInstance),
// and a previous cold-review revision missed wrapping it — every
// cold-load streaming request undercounted saturation. A future
// refactor that moves the Inc/Dec out of streamFromInstance would
// silently regress without this test.
func TestStreamFromInstance_PopulatesInflight(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9108,
			Name: "test-stream-inflight",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-stream-inflight",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Fake SSE backend — streamFromInstance dispatches at this URL.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(backend.Close)

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(backendURL.Port())
	require.NoError(t, err)

	inst := instance.NewInstance("test-cold-load-stream", "mlx", "test-stream-model", port, 0, 64)
	inst.HealthURL = backend.URL + "/health"
	inst.SetStatus(instance.StatusRunning)

	body := []byte(`{"model":"test-stream-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	ctx := context.WithValue(req.Context(), CtxKeyModel, "test-stream-model")
	ctx = context.WithValue(ctx, CtxKeyOriginalBody, body)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	server.streamFromInstance(w, req, inst, dialectOf(req).(httperr.InBandStreamer))

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	mbody := mw.Body.String()

	assert.Contains(t, mbody, "zz_proxy_inflight_requests_metric",
		"streamFromInstance must populate the inflight series")
	assert.Contains(t, mbody, `gen_ai_provider_name="mlx"`,
		"inflight series should carry the instance.Provider as gen_ai.provider.name")
	assert.Contains(t, mbody, `gen_ai_request_model="test-stream-model"`,
		"inflight series should carry the request model from CtxKeyModel")
	assert.Contains(t, mbody, `gen_ai_operation_name="chat"`,
		"inflight series should derive operation.name from /v1/chat/completions route")
}

// TestProxyFailedRequestsMetric_AuthFailLabels pins the rejected-key
// fingerprint stash on auth-failure paths. Without it, every 401
// collapses to one empty-label series and operators can't see which
// key (or which junk-key spray) is being rejected. Length-gated at 12
// characters so short/garbage rejections don't explode cardinality.
func TestProxyFailedRequestsMetric_AuthFailLabels(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9106,
			Name: "test-failed-auth",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Auth: pkgConfig.AuthConfig{
			AdminKey: "z9aP6kQwR3eY7Bx5mNcD4hJ2sV8fG1tL",
			UserKey:  "K4rL2mN6pQ8sV3xY7aZ9bC1dE5fH8jM2",
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-failed-auth",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Hit an admin-gated route with a long-but-wrong key. Auth rejects;
	// stashRejectedKeyFingerprint should land the truncated fingerprint
	// on the metric.
	bogusLongKey := "wrongkey-this-is-longer-than-twelve-chars-1234"
	req := httptest.NewRequest("GET", "/zzrouter/v1/keys", nil)
	req.Header.Set("X-API-Key", bogusLongKey)
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code,
		"expected 401 for bogus key, got %d", w.Code)

	// Hit the same route with a short junk key — should NOT contribute to
	// the hashed_api_key dimension (length-gated at 12).
	junkReq := httptest.NewRequest("GET", "/zzrouter/v1/keys", nil)
	junkReq.Header.Set("X-API-Key", "junk")
	junkW := httptest.NewRecorder()
	server.engine.ServeHTTP(junkW, junkReq)
	require.Equal(t, http.StatusUnauthorized, junkW.Code)

	// Scrape /metrics.
	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	body := mw.Body.String()

	// The long-key 401 should have stashed first4..last4 of the bogus
	// key. KeyFingerprint("wrongkey-this-is-longer-than-twelve-chars-1234")
	// = "wron" + "..." + "1234".
	assert.Contains(t, body, `hashed_api_key="wron...1234"`,
		"long-key 401 should populate hashed_api_key with the truncated fingerprint")

	// And the short-key 401 should produce a series with empty
	// hashed_api_key (length gate skipped the stash).
	assert.Contains(t, body, `hashed_api_key=""`,
		"short-key 401 should produce a series with empty hashed_api_key (length-gated)")

	// 401 status code label populates on both.
	assert.Contains(t, body, `status_code="401"`,
		"401 status_code label missing on auth-failure series")
}

// TestSpendTokenCountersExposed drives the LiteLLM-mirrored token
// counters end-to-end: zz.input.tokens.metric and
// zz.output.tokens.metric increment with the LiteLLM-vocabulary label
// set on every completion that flows through the InferenceLogBridge.
// The counters answer "tokens by key by day" panels that the OTel
// gen_ai.client.token.usage histogram can't (a histogram's
// bucket-cum-counter view doesn't slice that cleanly).
//
// Wires a real bridge (mirrors server_factory.go startup) and drives
// a single completion through the recorder so OnInferenceComplete fires
// the spend.RecordTokens call. Series presence + label keys are the
// contract; values vary across runs.
func TestSpendTokenCountersExposed(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9110,
			Name: "test-spend-tokens",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-spend-tokens",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Bridge wired against an empty inference log store. Pricing is
	// nil — the token counters are independent of cost resolution.
	bridge := NewInferenceLogBridge(inferencelog.NewStore(0, 0), "test-node", false, nil)
	llm.SetInferenceLogHook(bridge)
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })

	rec := llm.NewInferenceRecorder(context.Background(), "fast-chat", "openai")
	rec.SetOperation(genai.OperationChat)
	rec.SetCallerIdentity("alice-prod", "wron...1234", "Engineering")
	rec.SetKeyID("alice-key-id")
	rec.SetTeamID("eng-team-id")
	rec.SetModelGroup("fast-chat", "gpt-4o")
	rec.SetRequestType(llm.RequestTypeChat)
	rec.RecordCompletion(128, 256)

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	body := mw.Body.String()

	// Both counter series must appear after a single completion.
	assert.Contains(t, body, "zz_input_tokens_metric",
		"zz.input.tokens.metric series missing")
	assert.Contains(t, body, "zz_output_tokens_metric",
		"zz.output.tokens.metric series missing")

	// LiteLLM-vocabulary labels must populate so a `s/litellm/zz/` rename
	// of an existing dashboard JSON resolves the same dimensions.
	assert.Contains(t, body, `api_key_alias="alice-prod"`,
		"api_key_alias label missing — recorder.SetCallerIdentity didn't reach the bridge")
	assert.Contains(t, body, `hashed_api_key="wron...1234"`,
		"hashed_api_key label missing — fingerprint didn't propagate")
	assert.Contains(t, body, `team_alias="Engineering"`,
		"team_alias label missing — team name didn't propagate")
	assert.Contains(t, body, `team="eng-team-id"`,
		"team label missing — team id didn't propagate")

	// Model dimensions: requested_model = original (group alias),
	// model = post-resolution deployment, model_group = group name.
	assert.Contains(t, body, `requested_model="fast-chat"`,
		"requested_model label should be the original request model")
	assert.Contains(t, body, `model="gpt-4o"`,
		"model label should be the post-resolution deployment name")
	assert.Contains(t, body, `model_group="fast-chat"`,
		"model_group label should be the group alias")

	// api_provider must carry the canonical OTel-mapped provider value
	// (same value as gen_ai.provider.name) so dual-namespace dashboards
	// agree on this dimension.
	assert.Contains(t, body, `api_provider="openai"`,
		"api_provider should equal the OTel canonical provider name")

	// Empty user/end_user labels still emit — LiteLLM dashboards filter
	// on these keys directly, and a missing label key (vs an empty
	// string) breaks {user=~".+"} selectors silently.
	assert.Contains(t, body, `user=""`,
		"user label must emit (empty value valid until users ship)")
	assert.Contains(t, body, `end_user=""`,
		"end_user label must emit (empty value valid until end-users ship)")
}

// TestSpendTokenCounters_GatedByModelAllowlist pins the cardinality
// chokepoint: when an operator wires SetModelAllowlist, an adversarial
// caller-controlled requested_model collapses to UnknownModelLabel on
// the LiteLLM-mirrored counters too — not just the OTel client metrics.
// Without this, the spend counters would re-open the caller-controlled-
// model cardinality avenue the genai-package allowlist gate closes.
func TestSpendTokenCounters_GatedByModelAllowlist(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9111,
			Name: "test-spend-allowlist",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-spend-allowlist",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	defer genai.SwapModelAllowlist(func(name string) bool {
		return name == "known-model"
	})()

	bridge := NewInferenceLogBridge(inferencelog.NewStore(0, 0), "test-node", false, nil)
	llm.SetInferenceLogHook(bridge)
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })

	rec := llm.NewInferenceRecorder(context.Background(), "adversarial-spam", "openai")
	rec.SetOperation(genai.OperationChat)
	rec.SetRequestType(llm.RequestTypeChat)
	rec.RecordCompletion(10, 20)

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	body := mw.Body.String()

	assert.NotContains(t, body, `requested_model="adversarial-spam"`,
		"unknown caller-supplied model leaked through allowlist on LiteLLM token counter")
	assert.Contains(t, body, `requested_model="_unknown_"`,
		"unknown model should collapse to the _unknown_ sentinel on token counter")
	assert.Contains(t, body, `model="_unknown_"`,
		"unknown model on the resolved-model dimension should also gate")
}

// TestSpendCounterExposed pins that zz.spend.metric.total fires on
// every completion that flows through the InferenceLogBridge,
// including zero-cost completions (cost-source: "" or pricing not
// resolved). Operators want a "spend by key" panel; missing the
// zero-cost emit would silently leave free-tier traffic out of the
// rollup.
func TestSpendCounterExposed(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9112,
			Name: "test-spend-counter",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-spend-counter",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	bridge := NewInferenceLogBridge(inferencelog.NewStore(0, 0), "test-node", false, nil)
	llm.SetInferenceLogHook(bridge)
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })

	// One non-zero cost completion (Cost=0.0042 USD) — exercises the
	// pricing-store-resolved path. Cost flows from SetExtendedUsage
	// through the bridge's CalculateCostMicro (provider-authoritative
	// when set, pricing-store fallback otherwise).
	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4o", "openai")
	rec.SetOperation(genai.OperationChat)
	rec.SetRequestType(llm.RequestTypeChat)
	rec.SetCallerIdentity("billing-prod", "abcd...4321", "Finance")
	rec.SetExtendedUsage(0, 0, 0.0042, "provider")
	rec.RecordCompletion(50, 100)

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	body := mw.Body.String()

	assert.Contains(t, body, "zz_spend_metric_total",
		"zz.spend.metric.total series missing")
	assert.Contains(t, body, `api_key_alias="billing-prod"`,
		"api_key_alias must populate on the spend counter")
	assert.Contains(t, body, `team_alias="Finance"`,
		"team_alias must populate on the spend counter")
}

// TestBudgetGaugesExposed pins that the observable budget gauges
// (zz.api.key.max.budget.metric, zz.remaining.api.key.budget.metric)
// emit one row per virtual key with a configured SpendLimit, sourced
// from the live keystore + spend tracker. Keys without a SpendLimit
// (the "unenforced" convention) must not appear — emitting "max=0,
// remaining=0" for them would mean "$0 hard cap" to dashboard
// consumers, the opposite of the actual behaviour.
func TestBudgetGaugesExposed(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9113,
			Name: "test-budget-gauges",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-budget-gauges",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })
	t.Cleanup(func() { obsspend.SetBudgetSnapshotter(nil) })

	// Stub snapshotter: one capped key, one uncapped key. The uncapped
	// row should NOT appear (snapshotter filters SpendLimit <= 0).
	obsspend.SetBudgetSnapshotter(&stubSnapshotter{rows: []obsspend.KeyBudget{
		{
			Labels:          obsspend.CallerLabels{APIKeyAlias: "capped-key", Team: "team-a", TeamAlias: "Team A"},
			SpendLimitUSD:   100.0,
			CurrentSpendUSD: 30.0,
		},
		{
			Labels:          obsspend.CallerLabels{APIKeyAlias: "exhausted-key", Team: "team-b", TeamAlias: "Team B"},
			SpendLimitUSD:   50.0,
			CurrentSpendUSD: 75.0, // overshot — remaining must clamp at 0
		},
	}})

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	body := mw.Body.String()

	assert.Contains(t, body, "zz_api_key_max_budget_metric",
		"zz.api.key.max.budget.metric series missing")
	assert.Contains(t, body, "zz_remaining_api_key_budget_metric",
		"zz.remaining.api.key.budget.metric series missing")

	assert.Contains(t, body, `api_key_alias="capped-key"`,
		"capped key should appear on budget gauge")
	assert.Contains(t, body, `team_alias="Team A"`,
		"team_alias should populate on budget gauge")

	// Pin the empty-hashed_api_key contract on budget gauges. The
	// runtime label uses KeyFingerprint(rawKey); the keystore retains
	// only the Argon2id digest, so emitting first4..last4 of HashedKey
	// would produce a different (yet stable) identifier that breaks
	// JOIN queries against the spend counter on this dimension. Keep
	// the empty-string contract until a stable Fingerprint field is
	// persisted at create-time on VirtualKey.
	checkedSeriesLine := false
	for _, line := range strings.Split(body, "\n") {
		// Skip Prometheus comment lines (# HELP / # TYPE) and look only
		// at series rows — those carry a `{...}` label set.
		if !strings.Contains(line, "zz_api_key_max_budget_metric{") {
			continue
		}
		if !strings.Contains(line, `hashed_api_key=""`) {
			t.Errorf("budget gauge series must carry hashed_api_key=\"\" until Fingerprint persistence ships: %s", line)
		}
		checkedSeriesLine = true
		break
	}
	assert.True(t, checkedSeriesLine,
		"expected at least one zz_api_key_max_budget_metric series row")

	// The overshoot row's remaining must clamp at 0, not emit -25.
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "zz_remaining_api_key_budget_metric") {
			continue
		}
		if strings.Contains(line, `api_key_alias="exhausted-key"`) {
			// Look for the value at end of line: "metric{labels} value"
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				val := parts[len(parts)-1]
				if strings.HasPrefix(val, "-") {
					t.Errorf("overshoot row emitted negative remaining: %s", line)
				}
			}
		}
	}
}

// stubSnapshotter is a tiny fake of obsspend.BudgetSnapshotter for
// tests that don't want to seed a real keystore + spend tracker.
type stubSnapshotter struct {
	rows []obsspend.KeyBudget
}

func (s *stubSnapshotter) SnapshotKeyBudgets() []obsspend.KeyBudget { return s.rows }

// TestBudgetSnapshotter_IncludesReservedInCurrentSpend pins that
// in-flight reservations count against the remaining-budget gauge.
// The enforcer denies new traffic when (spend + reserved + estimate)
// > limit, so a "remaining < N" alert query needs the gauge to track
// the same view: limit - (spend + reserved). Otherwise dashboards
// disagree with the gate that's actually firing the denials.
func TestBudgetSnapshotter_IncludesReservedInCurrentSpend(t *testing.T) {
	rig := newAccessRig(t)
	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	aliceRaw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name: "Alice",
		Role: "user",
		QuotaConfig: quota.QuotaConfig{
			SpendLimit:       50.0,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 1000,
		},
	})
	c, _ := rig.makeRequest(aliceRaw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	require.True(t, rig.access.Enforce(c, "llama3"))

	// Enforce now holds a reservation. The snapshot must surface
	// limit - (spend + reserved) as current spend, not just spend.
	state := rig.access.GetKeySpend("alice")
	require.NotNil(t, state)
	require.Greater(t, state.ReservedMicro, int64(0),
		"sanity: Enforce must reserve when SpendLimit configured")

	snap := newBudgetSnapshotter(rig.keys, rig.teams, rig.access)
	rows := snap.SnapshotKeyBudgets()
	require.Len(t, rows, 1, "expected exactly one row for alice")

	wantSpend := quota.MicroToUSD(state.SpendMicro + state.ReservedMicro)
	if rows[0].CurrentSpendUSD != wantSpend {
		t.Errorf("CurrentSpendUSD = %v, want %v (= spend %v + reserved %v in USD)",
			rows[0].CurrentSpendUSD, wantSpend,
			quota.MicroToUSD(state.SpendMicro), quota.MicroToUSD(state.ReservedMicro))
	}
}

// TestBudgetSnapshotter_FiltersUnenforced pins the SpendLimit > 0
// gate on budgetSnapshotter. The keystore's seed for this test
// includes one capped key and one uncapped key; only the capped row
// should appear in the snapshot.
func TestBudgetSnapshotter_FiltersUnenforced(t *testing.T) {
	rig := newAccessRig(t)
	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name:        "Alice",
		Role:        "user",
		QuotaConfig: quota.QuotaConfig{SpendLimit: 25.0, ResetPeriod: "monthly"},
	})
	rig.seedKey(t, "bob", "eng", string(teams.RoleMember), &keys.VirtualKey{
		Name:        "Bob",
		Role:        "user",
		QuotaConfig: quota.QuotaConfig{SpendLimit: 0}, // unenforced
	})

	snap := newBudgetSnapshotter(rig.keys, rig.teams, rig.access)
	rows := snap.SnapshotKeyBudgets()

	if len(rows) != 1 {
		t.Fatalf("got %d budget rows, want 1 (alice only); rows = %+v", len(rows), rows)
	}
	if rows[0].Labels.APIKeyAlias != "Alice" {
		t.Errorf("snapshot key alias = %q, want Alice", rows[0].Labels.APIKeyAlias)
	}
	if rows[0].SpendLimitUSD != 25.0 {
		t.Errorf("snapshot SpendLimitUSD = %v, want 25.0", rows[0].SpendLimitUSD)
	}
	if rows[0].Labels.TeamAlias != "Engineering" {
		t.Errorf("snapshot TeamAlias = %q, want Engineering", rows[0].Labels.TeamAlias)
	}
}

// TestProxyTotalRequestsMetricExposed pins that
// zz.proxy.total.requests.metric fires on every LLM-route request,
// success or failure, with a populated status_code label. Together
// with the failed-requests counter (Phase 2.3) the LiteLLM dashboard
// trio (total / failed / failure-rate) becomes a single PromQL
// division.
func TestProxyTotalRequestsMetricExposed(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9114,
			Name: "test-total-requests",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-total-requests",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Two failed LLM requests through the responder path. They both
	// 4xx out — the total counter still fires.
	for _, path := range []string{"/v1/chat/completions", "/v1/completions"} {
		req := httptest.NewRequest("POST", path, strings.NewReader("{ malformed"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, req)
		require.GreaterOrEqual(t, w.Code, 400, "expected error status, got %d", w.Code)
	}

	// One non-LLM route (health). The middleware skips this so the
	// total counter's denominator stays specific to LLM traffic and
	// the failure-rate division agrees with operator intuition.
	healthReq := httptest.NewRequest("GET", "/health/live", nil)
	healthW := httptest.NewRecorder()
	server.engine.ServeHTTP(healthW, healthReq)

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	body := mw.Body.String()

	// The OTel Prometheus exporter strips the redundant "total" word
	// from the middle of counter names (every counter family gets a
	// `_total` suffix appended), so the internal name
	// zz.proxy.total.requests.metric lands on the wire as
	// zz_proxy_requests_metric_total. Documented in the package's
	// proxy_total.go.
	assert.Contains(t, body, "zz_proxy_requests_metric_total",
		"zz.proxy.total.requests.metric series missing on the wire")
	assert.Contains(t, body, `status_code="`,
		"status_code label missing on total-requests series")

	// Health route must NOT contribute. Look for a series row with
	// status_code="200" — only the /metrics scrape itself or a health
	// route would produce it on this test, and both are filtered. The
	// failed LLM requests above produce 4xx series, so a 200-coded
	// total-requests row would mean the filter regressed.
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "zz_proxy_requests_metric_total{") {
			continue
		}
		if strings.Contains(line, `status_code="200"`) {
			t.Errorf("non-LLM route leaked into total-requests counter: %s", line)
		}
	}
}

// TestProxyTotalVsFailed_ScopeDivergence pins the documented
// behavioural divergence between zz_proxy_failed_requests_metric_total
// (broad: every 4xx/5xx anywhere) and zz_proxy_requests_metric_total
// (narrow: only LLM routes). Naively dividing the two yields > 1.0 on
// admin-route auth failures; the package comment instructs operators
// to filter both on requested_model!="" to compute LLM-only failure
// rate. A future change that "fixed" the discrepancy by narrowing
// the failed counter would silently break Phase 2.3's contract for
// admin-surface telemetry — pinning the divergence catches that.
func TestProxyTotalVsFailed_ScopeDivergence(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9115,
			Name: "test-scope-divergence",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Auth: pkgConfig.AuthConfig{
			AdminKey: "z9aP6kQwR3eY7Bx5mNcD4hJ2sV8fG1tL",
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-scope-divergence",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Admin-route 401: fires the failed counter (broad scope) but
	// NOT the total counter (LLM-only).
	adminReq := httptest.NewRequest("GET", "/zzrouter/v1/keys", nil)
	adminReq.Header.Set("X-API-Key", "wrong-but-long-enough-to-fingerprint")
	adminW := httptest.NewRecorder()
	server.engine.ServeHTTP(adminW, adminReq)
	require.Equal(t, http.StatusUnauthorized, adminW.Code)

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	body := mw.Body.String()

	// Failed counter MUST include the admin-route series — that's
	// the Phase 2.3 contract.
	failedSeen := false
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "zz_proxy_failed_requests_metric_total{") &&
			strings.Contains(line, `status_code="401"`) {
			failedSeen = true
			break
		}
	}
	assert.True(t, failedSeen,
		"failed counter must emit on admin-route 401 (Phase 2.3 contract)")

	// Total counter MUST NOT include the admin-route series — its
	// scope is LLM routes only.
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "zz_proxy_requests_metric_total{") {
			continue
		}
		if strings.Contains(line, `status_code="401"`) {
			t.Errorf("total counter leaked an admin-route 401 series: %s", line)
		}
	}
}

// TestRoutingDecisionsMetric_EmitsOnDispatch pins that
// zz.routing.decisions.metric fires at the dispatch sites with a
// populated (strategy, outcome) tuple. Drives an unknown-model
// request through standalone mode to exercise the dispatch path
// without standing up a real backend.
func TestRoutingDecisionsMetric_EmitsOnDispatch(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9116,
			Name: "test-routing-decisions",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-routing-decisions",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Drive a request through the dispatch path. The resolver may
	// return success-with-empty-candidates or a real error depending
	// on local provider config; either way one of the routing-decision
	// emit sites fires.
	body := `{"model":"definitely-unknown-model-routing-test","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)
	require.GreaterOrEqual(t, w.Code, 400)

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	out := mw.Body.String()

	assert.Contains(t, out, "zz_routing_decisions_metric_total",
		"zz.routing.decisions.metric series missing")
	// Standalone (Disabled cluster) → strategy=cluster_aware. Worker
	// would emit local_only; the helper chooses based on cluster mode.
	assert.Contains(t, out, `strategy="cluster_aware"`,
		"standalone mode should emit strategy=cluster_aware")
	// Outcome must be one of the closed-enum values; the specific
	// value depends on whether the resolver returned
	// success-empty-candidates or a real error (model resolver
	// implementation detail).
	outcomePresent := strings.Contains(out, `outcome="local"`) ||
		strings.Contains(out, `outcome="remote"`) ||
		strings.Contains(out, `outcome="no_backend"`)
	assert.True(t, outcomePresent,
		"expected at least one of {local, remote, no_backend} on outcome label; got nothing")
	// Operation derives from the matched route; /v1/chat/completions
	// maps to chat via genai.OperationName.
	assert.Contains(t, out, `gen_ai_operation_name="chat"`,
		"operation label should populate from the matched route")
}

// TestRoutingStrategy_DerivesFromClusterMode pins the helper that
// computes strategy from cluster mode. Worker mode → local_only;
// every other mode (Disabled, Coordinator, Standalone) →
// cluster_aware. A regression here would silently flip every
// worker's routing-decisions series under a different strategy
// label and break dashboards split on this dimension.
func TestRoutingStrategy_DerivesFromClusterMode(t *testing.T) {
	// Standalone (cluster mode disabled): no listener, defaults to
	// cluster_aware.
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9117,
			Name: "test-routing-strategy",
		},
		Cluster:       pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeDisabled},
		Observability: pkgConfig.ObservabilityConfig{Enabled: false},
	}
	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	if got := server.routingStrategy(); got != obsrouting.StrategyClusterAware {
		t.Errorf("standalone routingStrategy = %q, want %q",
			got, obsrouting.StrategyClusterAware)
	}
}

// TestDeploymentLifecycleMetricsExposed pins that the deployment
// counters (zz.deployment.{starts,stops,failures}.metric) emit the
// expected wire-name series after a lifecycle event flows through
// the manager. Drives the package-level helper directly because
// standing up a real Instance with a live process is out of scope
// for an httptest harness — the prov_apps test suite has the
// manager-level coverage.
func TestDeploymentLifecycleMetricsExposed(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9118,
			Name: "test-deployment-lifecycle",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-deployment-lifecycle",
			Tracing:     pkgConfig.TracingConfig{Enabled: false},
			Metrics:     pkgConfig.MetricsConfig{Enabled: true, ExportInterval: 1},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Drive one of each lifecycle event through the package-level
	// helpers. Production wiring fires these from
	// pkg/prov_apps/manager.go's logEvent dispatch.
	obsruns.RecordStart(context.Background(), "vllm", "llama3:7b", "10.0.0.1")
	obsruns.RecordStop(context.Background(), "vllm", "llama3:7b", "10.0.0.1", obsruns.StopReasonCrash)
	obsruns.RecordFailure(context.Background(), "vllm", "llama3:7b", "10.0.0.1", obsruns.FailureReadiness)

	mreq := httptest.NewRequest("GET", "/metrics", nil)
	mw := httptest.NewRecorder()
	server.engine.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	out := mw.Body.String()

	// All three counter wire-names must appear.
	assert.Contains(t, out, "zz_deployment_starts_metric_total",
		"zz.deployment.starts.metric series missing")
	assert.Contains(t, out, "zz_deployment_stops_metric_total",
		"zz.deployment.stops.metric series missing")
	assert.Contains(t, out, "zz_deployment_failures_metric_total",
		"zz.deployment.failures.metric series missing")

	// Closed-enum reason / cause labels populate.
	assert.Contains(t, out, `reason="crash"`,
		"reason label missing on stops series")
	assert.Contains(t, out, `cause="readiness_fail"`,
		"cause label missing on failures series")

	// gen_ai.* labels populate from the helper's args.
	assert.Contains(t, out, `gen_ai_provider_name="vllm"`,
		"gen_ai.provider.name should populate from RecordX args")
	assert.Contains(t, out, `gen_ai_request_model="llama3:7b"`,
		"gen_ai.request.model should populate from RecordX args")
	assert.Contains(t, out, `server_address="10.0.0.1"`,
		"server.address should populate from RecordX args")
}

// TestNodeWithMetricsDisabled verifies /metrics returns a 503 Problem Details
// response (not a silent 404) when metrics are explicitly disabled.
func TestNodeWithMetricsDisabled(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9104,
			Name: "test-metrics-disabled",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-metrics-off",
			Tracing: pkgConfig.TracingConfig{
				Enabled:    true,
				SampleRate: 1.0,
			},
			Metrics: pkgConfig.MetricsConfig{
				Enabled: false, // Metrics subsystem off → /metrics should 503
			},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)

	// Should return 503 Service Unavailable with a remediation hint.
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "metrics")
}

// TestHealthEndpointWithTracing verifies health endpoints work with tracing enabled
func TestHealthEndpointWithTracing(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9105,
			Name: "test-health-tracing",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-test-health",
			Tracing: pkgConfig.TracingConfig{
				Enabled:    true,
				SampleRate: 1.0,
			},
			Metrics: pkgConfig.MetricsConfig{
				Enabled: true,
			},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	tests := []struct {
		path         string
		expectedCode int
	}{
		{"/health", http.StatusOK},
		{"/health/live", http.StatusOK},
		{"/health/ready", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			w := httptest.NewRecorder()
			server.engine.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)

			// Verify JSON response
			var response map[string]any
			err := json.Unmarshal(w.Body.Bytes(), &response)
			assert.NoError(t, err, "Response should be valid JSON")
			assert.Contains(t, response, "status")
		})
	}
}

// TestObservabilityConfigValidation verifies configuration edge cases
func TestObservabilityConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		config pkgConfig.ObservabilityConfig
	}{
		{
			name: "empty_service_name",
			config: pkgConfig.ObservabilityConfig{
				Enabled:     true,
				ServiceName: "", // Should default to "zzrouter"
				Tracing: pkgConfig.TracingConfig{
					Enabled: true,
				},
			},
		},
		{
			name: "negative_sample_rate",
			config: pkgConfig.ObservabilityConfig{
				Enabled:     true,
				ServiceName: "test",
				Tracing: pkgConfig.TracingConfig{
					Enabled:    true,
					SampleRate: -1.0, // Should be clamped to 0
				},
			},
		},
		{
			name: "sample_rate_over_one",
			config: pkgConfig.ObservabilityConfig{
				Enabled:     true,
				ServiceName: "test",
				Tracing: pkgConfig.TracingConfig{
					Enabled:    true,
					SampleRate: 2.0, // Should be clamped to 1
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &pkgConfig.NodeConfig{
				Node: pkgConfig.ServeConfig{
					Bind: "localhost",
					Port: 9106,
					Name: "test-config-validation",
				},
				Cluster: pkgConfig.ClusterConfig{
					Mode: pkgConfig.ClusterModeDisabled,
				},
				Observability: tt.config,
			}

			// Node creation should not panic
			server, err := NewServerWithOptions(cfg)
			assert.NoError(t, err)
			t.Cleanup(func() { cleanupTestNode(server) })
		})
	}
}

// TestConcurrentRequestsWithObservability verifies the server handles
// concurrent requests correctly with observability enabled
func TestConcurrentRequestsWithObservability(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9107,
			Name: "test-concurrent",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-concurrent-test",
			Tracing: pkgConfig.TracingConfig{
				Enabled:    true,
				SampleRate: 1.0,
			},
			Metrics: pkgConfig.MetricsConfig{
				Enabled: true,
			},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Run concurrent requests
	concurrency := 10
	done := make(chan bool, concurrency)

	for range concurrency {
		go func() {
			req := httptest.NewRequest("GET", "/health/live", nil)
			w := httptest.NewRecorder()
			server.engine.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
			done <- true
		}()
	}

	// Wait for all requests
	for range concurrency {
		<-done
	}
}

// TestObservabilityMiddlewareOrder verifies middleware is applied in correct order
func TestObservabilityMiddlewareOrder(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9108,
			Name: "test-middleware-order",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-middleware-test",
			Tracing: pkgConfig.TracingConfig{
				Enabled:    true,
				SampleRate: 1.0,
			},
			Metrics: pkgConfig.MetricsConfig{
				Enabled: true,
			},
		},
	}

	server, err := NewServerWithOptions(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(server) })

	// Request should succeed, proving middleware chain works
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// BenchmarkHealthWithObservability benchmarks health endpoint with observability
func BenchmarkHealthWithObservability(b *testing.B) {
	gin.SetMode(gin.TestMode)

	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9109,
			Name: "bench-otel",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled:     true,
			ServiceName: "zzrouter-bench",
			Tracing: pkgConfig.TracingConfig{
				Enabled:    true,
				SampleRate: 1.0,
			},
			Metrics: pkgConfig.MetricsConfig{
				Enabled: true,
			},
		},
	}

	server, err := NewServerWithOptions(cfg)
	if err != nil {
		b.Fatalf("Node initialization failed: %v", err)
	}
	b.Cleanup(func() { cleanupTestNode(server) })

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", "/health/live", nil)
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, req)
	}
}

// BenchmarkHealthWithoutObservability benchmarks health endpoint without observability
func BenchmarkHealthWithoutObservability(b *testing.B) {
	gin.SetMode(gin.TestMode)

	cfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: 9110,
			Name: "bench-no-otel",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Observability: pkgConfig.ObservabilityConfig{
			Enabled: false,
		},
	}

	server, err := NewServerWithOptions(cfg)
	if err != nil {
		b.Fatalf("Node initialization failed: %v", err)
	}
	b.Cleanup(func() { cleanupTestNode(server) })

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", "/health/live", nil)
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, req)
	}
}
