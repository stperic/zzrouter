package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/stperic/zzrouter/pkg/observability/genai"
)

// ============================================================================
// Span Tests
// ============================================================================

func TestTracer(t *testing.T) {
	tracer := Tracer()
	if tracer == nil {
		t.Fatal("Tracer() returned nil")
	}
}

func TestTracerName(t *testing.T) {
	if TracerName != "zzrouter.llm" {
		t.Errorf("TracerName = %q, want %q", TracerName, "zzrouter.llm")
	}
}

func TestSpanKindConstants(t *testing.T) {
	if SpanKindInference != trace.SpanKindClient {
		t.Errorf("SpanKindInference = %v, want %v", SpanKindInference, trace.SpanKindClient)
	}
	if SpanKindModelLoad != trace.SpanKindInternal {
		t.Errorf("SpanKindModelLoad = %v, want %v", SpanKindModelLoad, trace.SpanKindInternal)
	}
	if SpanKindRouting != trace.SpanKindInternal {
		t.Errorf("SpanKindRouting = %v, want %v", SpanKindRouting, trace.SpanKindInternal)
	}
}

func TestStartInferenceSpan(t *testing.T) {
	ctx := context.Background()
	newCtx, span := StartInferenceSpan(ctx, "llama2:7b", RequestTypeChat, true)
	defer span.End()

	if newCtx == ctx {
		t.Error("StartInferenceSpan should return new context")
	}
	if span == nil {
		t.Fatal("StartInferenceSpan returned nil span")
	}
	// Note: Without a configured tracer provider, span context may not be valid
	// This is expected behavior with noop tracer
	_ = span.SpanContext().IsValid()
}

func TestStartModelLoadSpan(t *testing.T) {
	ctx := context.Background()
	newCtx, span := StartModelLoadSpan(ctx, "llama2:7b", "ollama")
	defer span.End()

	if newCtx == ctx {
		t.Error("StartModelLoadSpan should return new context")
	}
	if span == nil {
		t.Fatal("StartModelLoadSpan returned nil span")
	}
}

func TestSetSpanError(t *testing.T) {
	ctx := context.Background()
	_, span := Tracer().Start(ctx, "test")
	defer span.End()

	testErr := errors.New("test error")
	// Should not panic
	SetSpanError(span, testErr)
}

// TestSetSpanErrorWithType pins that the error.type closed-enum lands
// on the span — used at pre-recorder error sites where no
// InferenceRecorder ever runs to stamp the same attribute.
func TestSetSpanErrorWithType(t *testing.T) {
	sr := installSpanRecorder(t)
	_, span := Tracer().Start(context.Background(), "test")
	SetSpanErrorWithType(span, "invalid_request_error", errors.New("bind failed"))
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("got %d ended spans, want 1", len(ended))
	}
	attrs := attributesByKey(ended[0].Attributes())
	if got, ok := attrs["error.type"]; !ok || got.AsString() != "invalid_request_error" {
		t.Errorf("error.type = %v (present=%v), want invalid_request_error", got, ok)
	}
}

func TestSetSpanError_NilError(t *testing.T) {
	ctx := context.Background()
	_, span := Tracer().Start(ctx, "test")
	defer span.End()

	// Should not panic with nil error
	SetSpanError(span, nil)
}

func TestSetSpanOK(t *testing.T) {
	ctx := context.Background()
	_, span := Tracer().Start(ctx, "test")
	defer span.End()

	// Should not panic
	SetSpanOK(span)
}

func TestAddModelLoadResult(t *testing.T) {
	ctx := context.Background()
	_, span := Tracer().Start(ctx, "test")
	defer span.End()

	// Should not panic
	AddModelLoadResult(span, 5000.0, "inst-123", 11434)
}

// ============================================================================
// Metrics Tests
// ============================================================================

func TestInitMetrics(t *testing.T) {
	// Note: We can't easily reset sync.Once, so we just test that
	// the function can be called without error
	m, err := InitMetrics()
	if err != nil {
		t.Fatalf("InitMetrics() error = %v", err)
	}
	if m == nil {
		t.Fatal("InitMetrics() returned nil")
	}
}

func TestGetMetrics(t *testing.T) {
	m := GetMetrics()
	if m == nil {
		t.Fatal("GetMetrics() returned nil")
	}
}

func TestMetrics_RecordModelLoad(t *testing.T) {
	m := GetMetrics()
	ctx := context.Background()

	// Should not panic
	m.RecordModelLoad(ctx, "llama2:7b", "ollama", "success", 5.0)
	m.RecordModelLoad(ctx, "llama2:7b", "ollama", "failure", 0)
}

func TestMetrics_RecordModelLoad_NilMetrics(t *testing.T) {
	var m *Metrics
	ctx := context.Background()

	// Should not panic with nil receiver
	m.RecordModelLoad(ctx, "llama2:7b", "ollama", "success", 5.0)
}

func TestRequestTypeToOperation(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{RequestTypeChat, "chat"},
		{RequestTypeGenerate, "chat"},
		{RequestTypeCompletion, "text_completion"},
		{RequestTypeEmbedding, "embeddings"},
		{"", ""},
		{"unknown_garbage", ""},
	}
	for _, c := range cases {
		if got := RequestTypeToOperation(c.in); string(got) != c.want {
			t.Errorf("RequestTypeToOperation(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMetrics_RecordGenAIClientCall(t *testing.T) {
	m := GetMetrics()
	ctx := context.Background()

	// Exercise both success and error label paths.
	m.RecordGenAIClientCall(ctx, "chat", "ollama", "llama2:7b", "", "", "10.0.0.1", 9090, 2.5, 100, 200)
	m.RecordGenAIClientCall(ctx, "chat", "ollama", "llama2:7b", "", "backend_unavailable", "", 0, 0.1, 0, 0)
}

func TestMetrics_RecordGenAIClientCall_NilMetrics(t *testing.T) {
	var m *Metrics
	ctx := context.Background()

	// Should not panic with nil receiver
	m.RecordGenAIClientCall(ctx, "chat", "ollama", "llama2:7b", "", "", "", 0, 2.5, 100, 200)
}

func TestMetrics_RecordGenAIServerTimeToFirstToken(t *testing.T) {
	m := GetMetrics()
	ctx := context.Background()

	m.RecordGenAIServerTimeToFirstToken(ctx, "chat", "ollama", "llama2:7b", "", "", 0, 0.15)
}

func TestMetrics_RecordGenAIServerTimeToFirstToken_NilMetrics(t *testing.T) {
	var m *Metrics
	ctx := context.Background()

	// Should not panic with nil receiver
	m.RecordGenAIServerTimeToFirstToken(ctx, "chat", "ollama", "llama2:7b", "", "", 0, 0.15)
}

func TestMetrics_RecordInstanceStart(t *testing.T) {
	m := GetMetrics()
	ctx := context.Background()

	// Should not panic
	m.RecordInstanceStart(ctx, "llama2:7b", "ollama")
}

func TestMetrics_RecordInstanceStart_NilMetrics(t *testing.T) {
	var m *Metrics
	ctx := context.Background()

	// Should not panic with nil receiver
	m.RecordInstanceStart(ctx, "llama2:7b", "ollama")
}

func TestMetrics_RecordInstanceStop(t *testing.T) {
	m := GetMetrics()
	ctx := context.Background()

	// Should not panic
	m.RecordInstanceStop(ctx, "llama2:7b", "ollama")
}

func TestMetrics_RecordInstanceStop_NilMetrics(t *testing.T) {
	var m *Metrics
	ctx := context.Background()

	// Should not panic with nil receiver
	m.RecordInstanceStop(ctx, "llama2:7b", "ollama")
}

// ============================================================================
// InferenceRecorder Tests
// ============================================================================

func TestNewInferenceRecorder(t *testing.T) {
	ctx := context.Background()
	recorder := NewInferenceRecorder(ctx, "llama2:7b", "ollama")

	if recorder == nil {
		t.Fatal("NewInferenceRecorder() returned nil")
	}
	if recorder.model != "llama2:7b" {
		t.Errorf("model = %q, want %q", recorder.model, "llama2:7b")
	}
	if recorder.provider != "ollama" {
		t.Errorf("provider = %q, want %q", recorder.provider, "ollama")
	}
	if recorder.startNs == 0 {
		t.Error("startNs should not be 0")
	}
}

func TestInferenceRecorder_RecordCompletion(t *testing.T) {
	ctx := context.Background()
	recorder := NewInferenceRecorder(ctx, "llama2:7b", "ollama")

	// Wait a bit to ensure measurable duration
	time.Sleep(1 * time.Millisecond)

	// Should not panic
	recorder.RecordCompletion(100, 200)
}

func TestInferenceRecorder_RecordCompletion_NilRecorder(t *testing.T) {
	var recorder *InferenceRecorder

	// Should not panic with nil receiver
	recorder.RecordCompletion(100, 200)
}

// TestInferenceRecorder_KeyAndTeamIDPropagation verifies SetKeyID / SetTeamID
// flow through RecordCompletion into the InferenceLogHook payload. The hook is
// the only place TeamID feeds into the log bridge, so this is the contract.
func TestInferenceRecorder_KeyAndTeamIDPropagation(t *testing.T) {
	var captured InferenceLogData
	var called bool
	SetInferenceLogHook(hookFunc(func(d InferenceLogData) {
		captured = d
		called = true
	}))
	t.Cleanup(func() { SetInferenceLogHook(nil) })

	rec := NewInferenceRecorder(context.Background(), "llama3", "ollama")
	rec.SetKeyID("key-alice")
	rec.SetTeamID("team-engineering")
	rec.RecordCompletion(10, 20)

	if !called {
		t.Fatal("inference log hook not called")
	}
	if captured.KeyID != "key-alice" {
		t.Errorf("KeyID = %q, want %q", captured.KeyID, "key-alice")
	}
	if captured.TeamID != "team-engineering" {
		t.Errorf("TeamID = %q, want %q", captured.TeamID, "team-engineering")
	}
}

// hookFunc is a test-only adapter that turns a bare func into an
// InferenceLogHook so tests can capture payloads inline.
type hookFunc func(InferenceLogData)

func (h hookFunc) OnInferenceComplete(d InferenceLogData) { h(d) }

func TestInferenceRecorder_RecordCompletion_NilMetrics(t *testing.T) {
	ctx := context.Background()
	recorder := &InferenceRecorder{
		ctx:      ctx,
		metrics:  nil, // Explicitly nil
		model:    "test",
		provider: "test",
		startNs:  nanotime(),
	}

	// Should not panic with nil metrics
	recorder.RecordCompletion(100, 200)
}

func TestInferenceRecorder_RecordFirstToken(t *testing.T) {
	ctx := context.Background()
	recorder := NewInferenceRecorder(ctx, "llama2:7b", "ollama")

	// Wait a bit
	time.Sleep(1 * time.Millisecond)

	// Should not panic
	recorder.RecordFirstToken()
}

func TestInferenceRecorder_RecordFirstToken_NilRecorder(t *testing.T) {
	var recorder *InferenceRecorder

	// Should not panic with nil receiver
	recorder.RecordFirstToken()
}

func TestInferenceRecorder_RecordFirstToken_NilMetrics(t *testing.T) {
	ctx := context.Background()
	recorder := &InferenceRecorder{
		ctx:      ctx,
		metrics:  nil, // Explicitly nil
		model:    "test",
		provider: "test",
		startNs:  nanotime(),
	}

	// Should not panic with nil metrics
	recorder.RecordFirstToken()
}

func TestNanotime(t *testing.T) {
	t1 := nanotime()
	time.Sleep(1 * time.Millisecond)
	t2 := nanotime()

	if t2 <= t1 {
		t.Errorf("nanotime() should increase over time: t1=%d, t2=%d", t1, t2)
	}
}

// ============================================================================
// Attribute Key Tests
// ============================================================================

func TestAttributeKeys(t *testing.T) {
	tests := []struct {
		key      string
		expected string
	}{
		// OTel GenAI semconv keys (migrated from llm.* in Phase 6).
		{string(ModelNameKey), "gen_ai.request.model"},
		{string(ModelProviderKey), "gen_ai.provider.name"},
		{string(RequestTypeKey), "gen_ai.operation.name"},
		{string(ResponseModelKey), "gen_ai.response.model"},
		{string(ResponseIDKey), "gen_ai.response.id"},
		{string(ServerAddressKey), "server.address"},
		{string(ServerPortKey), "server.port"},
		{string(ErrorTypeKey), "error.type"},
		{string(UsageInputTokensKey), "gen_ai.usage.input_tokens"},
		{string(UsageOutputTokensKey), "gen_ai.usage.output_tokens"},
		// zzrouter-namespaced (no OTel canonical exists).
		{string(RequestStreamKey), "llm.request.stream"},
		{string(ModelLoadTimeKey), "llm.model_load_time_ms"},
		{string(InstanceIDKey), "llm.instance.id"},
		{string(InstancePortKey), "llm.instance.port"},
		{string(RoutingDecisionKey), "llm.routing.decision"},
		{string(RoutingNodeKey), "llm.routing.host"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if tt.key != tt.expected {
				t.Errorf("Key = %q, want %q", tt.key, tt.expected)
			}
		})
	}
}

// TestRequestType_TranslatesToGenAIOperation pins that the `gen_ai.operation.name`
// attribute carries the OTel closed-enum value, not zzrouter's internal
// request-type string. RequestTypeCompletion ("completion") translates to
// "text_completion"; RequestTypeEmbedding ("embedding") → "embeddings".
func TestRequestType_TranslatesToGenAIOperation(t *testing.T) {
	cases := []struct {
		in      string
		wantVal string
	}{
		{RequestTypeChat, "chat"},
		{RequestTypeGenerate, "chat"},
		{RequestTypeCompletion, "text_completion"},
		{RequestTypeEmbedding, "embeddings"},
		// Unknown inputs map to "" so user-controlled cardinality
		// can't leak into the closed-enum gen_ai.operation.name dimension.
		{"unknown_garbage", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			kv := RequestType(c.in)
			if string(kv.Key) != "gen_ai.operation.name" {
				t.Errorf("key = %q, want gen_ai.operation.name", string(kv.Key))
			}
			if got := kv.Value.AsString(); got != c.wantVal {
				t.Errorf("RequestType(%q) value = %q, want %q", c.in, got, c.wantVal)
			}
		})
	}
}

// ============================================================================
// Codes Usage Tests
// ============================================================================

func TestCodesUsage(t *testing.T) {
	// Verify codes constants are accessible
	_ = codes.Ok
	_ = codes.Error
	_ = codes.Unset
}

// TestSnapshot_TTFTAsDuration pins that ttftNs is treated as a
// DURATION (matching how RecordFirstToken stores it), not as an
// absolute timestamp. Regression-guards a real bug where Snapshot
// computed `now - ttftNs` against absolute Unix nanos and produced
// nonsense tokens_per_second on streaming responses.
func TestSnapshot_TTFTAsDuration(t *testing.T) {
	r := NewInferenceRecorder(context.Background(), "m", "openrouter")
	// Pretend startNs is 1s ago, first token at 200ms, currently at
	// 500ms (so post-TTFT elapsed = 300ms, throughput = 30/0.3s = 100 t/s).
	now := nanotime()
	r.startNs = now - 500_000_000 // 500ms ago
	r.ttftNs = 200_000_000        // 200ms duration
	r.SetTokensOut(30)

	snap := r.Snapshot()

	if snap.TTFTMs != 200 {
		t.Errorf("TTFTMs = %d, want 200 (ttftNs is a duration in ns, not absolute)", snap.TTFTMs)
	}
	if snap.LatencyMs < 490 || snap.LatencyMs > 510 {
		t.Errorf("LatencyMs = %d, want ~500", snap.LatencyMs)
	}
	// post-TTFT elapsed ≈ 300ms ⇒ tps ≈ 100. Allow generous slack for
	// nanotime() drift between the test fixture setup and the read.
	if snap.TokensPerSec < 90 || snap.TokensPerSec > 120 {
		t.Errorf("TokensPerSec = %v, want ~100", snap.TokensPerSec)
	}
}

// installSpanRecorder routes the global tracer through an in-memory
// SpanRecorder for the test's lifetime, restoring the prior provider on
// cleanup. The provider returned isn't directly usable — Tracer() reads the
// global, so callers exercise StartInferenceSpan and the recorder picks up
// the spans.
func installSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

// TestModelAllowlist_GatesInferenceSpan pins the cardinality contract
// at the inference-span level: caller-controlled model names that
// fail an operator-registered allowlist collapse to UnknownModelLabel
// on `gen_ai.request.model`. Without the gate, every successful
// inference request (the dominant volume — far exceeding the failure
// counter exercised in the proxy package) would be wide open to
// adversarial cardinality spraying.
func TestModelAllowlist_GatesInferenceSpan(t *testing.T) {
	sr := installSpanRecorder(t)

	defer genai.SwapModelAllowlist(func(name string) bool {
		return name == "known-baseline"
	})()

	_, span := StartInferenceSpan(context.Background(), "adversarial-spam-name", RequestTypeChat, false)
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("got %d ended spans, want 1", len(ended))
	}
	attrs := attributesByKey(ended[0].Attributes())
	got, ok := attrs["gen_ai.request.model"]
	if !ok {
		t.Fatal("missing gen_ai.request.model on inference span")
	}
	// Adversarial value MUST NOT appear raw — that's the whole point
	// of the allowlist. The sentinel collapses it to a single bucket.
	if v := got.AsString(); v == "adversarial-spam-name" {
		t.Errorf("adversarial caller name leaked: %q", v)
	}
	if v := got.AsString(); v != "_unknown_" {
		t.Errorf("expected sentinel _unknown_, got %q", v)
	}
}

// TestRecordCompletion_EnrichesInferenceSpan asserts that a successful
// RecordCompletion stamps the OTel GenAI semconv conditionally-required +
// recommended attributes onto the active inference span: response.model,
// usage.input_tokens, usage.output_tokens, provider.name, server.address +
// server.port. Pins the Phase 6 contract — span and metric must agree.
func TestRecordCompletion_EnrichesInferenceSpan(t *testing.T) {
	sr := installSpanRecorder(t)

	ctx, span := StartInferenceSpan(context.Background(), "Qwen/Qwen2.5-7B", RequestTypeChat, true)
	rec := NewInferenceRecorder(ctx, "Qwen/Qwen2.5-7B", "llama_cpp")
	rec.SetRequestType(RequestTypeChat)
	rec.SetResponseModel("Qwen/Qwen2.5-7B-Instruct-GGUF")
	rec.SetServer("192.0.2.10", 9091)
	rec.RecordCompletion(42, 100)
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("got %d ended spans, want 1", len(ended))
	}
	attrs := attributesByKey(ended[0].Attributes())

	wantString := map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.request.model":  "Qwen/Qwen2.5-7B",
		"gen_ai.response.model": "Qwen/Qwen2.5-7B-Instruct-GGUF",
		"gen_ai.provider.name":  "llama_cpp",
		"server.address":        "192.0.2.10",
	}
	for k, want := range wantString {
		got, ok := attrs[k]
		if !ok {
			t.Errorf("missing attribute %s", k)
			continue
		}
		if v := got.AsString(); v != want {
			t.Errorf("attr %s = %q, want %q", k, v, want)
		}
	}
	wantInt := map[string]int64{
		"gen_ai.usage.input_tokens":  42,
		"gen_ai.usage.output_tokens": 100,
		"server.port":                9091,
	}
	for k, want := range wantInt {
		got, ok := attrs[k]
		if !ok {
			t.Errorf("missing attribute %s", k)
			continue
		}
		if v := got.AsInt64(); v != want {
			t.Errorf("attr %s = %d, want %d", k, v, want)
		}
	}
	if v, ok := attrs["llm.request.stream"]; !ok || !v.AsBool() {
		t.Errorf("expected llm.request.stream=true, got %v (present=%v)", v, ok)
	}
	// error.type must be ABSENT on success — populating it would corrupt
	// dashboards that filter by `error.type=""` for success-rate panels.
	if _, ok := attrs["error.type"]; ok {
		t.Errorf("error.type should be absent on success path")
	}
}

// TestRecordCompletion_TagsErrorTypeOnSpan asserts the error path: SetError
// followed by RecordCompletion(0,0) (the canonical pre-byte-copy abort
// shape) lands `error.type` on the span. usage tokens must NOT appear when
// both inputs are zero — emitting `gen_ai.usage.output_tokens=0` would
// pollute throughput panels.
func TestRecordCompletion_TagsErrorTypeOnSpan(t *testing.T) {
	sr := installSpanRecorder(t)

	ctx, span := StartInferenceSpan(context.Background(), "missing/model", RequestTypeChat, false)
	rec := NewInferenceRecorder(ctx, "missing/model", "")
	rec.SetRequestType(RequestTypeChat)
	rec.SetError("model_not_found", "no such model")
	rec.RecordCompletion(0, 0)
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("got %d ended spans, want 1", len(ended))
	}
	attrs := attributesByKey(ended[0].Attributes())

	if got, ok := attrs["error.type"]; !ok || got.AsString() != "model_not_found" {
		t.Errorf("error.type = %v (present=%v), want model_not_found", got, ok)
	}
	for _, key := range []string{"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens"} {
		if _, ok := attrs[key]; ok {
			t.Errorf("usage attr %s should be absent when tokensIn=tokensOut=0", key)
		}
	}
}

// attributesByKey indexes a slice of attribute.KeyValue by key string for
// O(1) presence + value lookup. SpanSnapshot.Attributes() returns a slice;
// callers that want map-style access can use this helper.
func attributesByKey(kvs []attribute.KeyValue) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

// TestSnapshot_TokensPerSecBelowNoiseFloor confirms the < 5 token gate.
func TestSnapshot_TokensPerSecBelowNoiseFloor(t *testing.T) {
	r := NewInferenceRecorder(context.Background(), "m", "openrouter")
	r.startNs = nanotime() - 500_000_000
	r.ttftNs = 100_000_000
	r.SetTokensOut(3)

	snap := r.Snapshot()
	if snap.TokensPerSec != 0 {
		t.Errorf("TokensPerSec = %v, want 0 (output_tokens=3 is below noise floor)", snap.TokensPerSec)
	}
}
