package wire

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

func TestParseOllamaUsage(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		wantIn  int64
		wantOut int64
		wantOK  bool
	}{
		{
			name:   "generate response",
			body:   []byte(`{"model":"llama3","response":"hello","done":true,"prompt_eval_count":15,"eval_count":25}`),
			wantIn: 15, wantOut: 25, wantOK: true,
		},
		{
			name:   "chat response",
			body:   []byte(`{"model":"llama3","message":{"role":"assistant","content":"hi"},"done":true,"prompt_eval_count":8,"eval_count":12}`),
			wantIn: 8, wantOut: 12, wantOK: true,
		},
		{
			name:   "streaming partial (done=false)",
			body:   []byte(`{"model":"llama3","response":"h","done":false}`),
			wantOK: false,
		},
		{
			name:   "no eval fields",
			body:   []byte(`{"model":"llama3","done":true}`),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, out, ok := ParseOllamaUsage(tt.body)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.wantIn, in)
				assert.Equal(t, tt.wantOut, out)
			}
		})
	}
}

func TestExtractProviderCost(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCost float64
	}{
		{"OpenRouter x_openrouter cost", `{"x_openrouter":{"cost":0.00123}}`, 0.00123},
		{"Cloudflare x_cloudflare cost", `{"x_cloudflare":{"cost":0.00045}}`, 0.00045},
		{"Groq x_groq cost", `{"x_groq":{"cost":0.001}}`, 0.001},
		{"no cost field", `{"usage":{"prompt_tokens":10}}`, 0},
		{"non-x_ fields ignored", `{"metadata":{"cost":999}}`, 0},
		{"x_ field without cost subfield", `{"x_provider":{"latency":42}}`, 0},
		{"empty body", `{}`, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(tt.body), &raw))
			cost := extractProviderCost(raw)
			assert.InDelta(t, tt.wantCost, cost, 0.0000001)
		})
	}
}

func TestParseUsageExtended_WithCost(t *testing.T) {
	body := []byte(`{
		"usage":{"prompt_tokens":100,"completion_tokens":50},
		"x_openrouter":{"cost":0.0025}
	}`)
	u := ParseUsageExtended(body)
	assert.Equal(t, int64(100), u.In)
	assert.Equal(t, int64(50), u.Out)
	assert.InDelta(t, 0.0025, u.Cost, 0.0000001)
}

// TestParseUsageExtended_OpenRouterUsageCost is a regression test
// for the bug where extractProviderCost only scanned x_*.cost — every
// OpenRouter request undercounted spend because OpenRouter actually
// puts cost at usage.cost. Fixture captured live 2026-04-26.
func TestParseUsageExtended_OpenRouterUsageCost(t *testing.T) {
	body := []byte(`{
		"id": "gen-1777174469-LxK6JdI3MdRTIzIjfS7e",
		"usage": {
			"prompt_tokens": 19,
			"completion_tokens": 3,
			"total_tokens": 22,
			"cost": 0.000001989,
			"is_byok": false,
			"prompt_tokens_details": {"cached_tokens": 0, "cache_write_tokens": 0},
			"cost_details": {"upstream_inference_cost": 0.000001989}
		}
	}`)
	u := ParseUsageExtended(body)
	assert.Equal(t, int64(19), u.In)
	assert.Equal(t, int64(3), u.Out)
	assert.InDelta(t, 0.000001989, u.Cost, 1e-12)
	assert.Equal(t, CostSourceProvider, u.CostSource,
		"upstream usage.cost is authoritative ⇒ source must be provider")
}

// TestParseUsageExtended_NoCost confirms an empty CostSource
// when neither usage.cost nor x_*.cost is present, so downstream
// callers (quota.CalculateCostMicro) can fall back to pricing-store
// compute and tag the source as "zzrouter".
func TestParseUsageExtended_NoCost(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	u := ParseUsageExtended(body)
	assert.Equal(t, int64(10), u.In)
	assert.Equal(t, int64(5), u.Out)
	assert.Equal(t, float64(0), u.Cost)
	assert.Equal(t, CostSource(""), u.CostSource)
}

func TestParseUsageExtended_CachedAndReasoning(t *testing.T) {
	body := []byte(`{
		"usage":{
			"prompt_tokens":100,
			"completion_tokens":50,
			"prompt_tokens_details":{"cached_tokens":30},
			"completion_tokens_details":{"reasoning_tokens":10}
		}
	}`)
	u := ParseUsageExtended(body)
	assert.Equal(t, int64(100), u.In)
	assert.Equal(t, int64(50), u.Out)
	assert.Equal(t, int64(30), u.CachedTokens)
	assert.Equal(t, int64(10), u.ReasoningTokens)
}

// TestParseUsageExtended_ResponseModel pins the gen_ai.response.model
// extraction: the upstream's `model` field — distinct from the
// request.model the caller asked for — feeds the response.model
// attribute on metrics + spans. Backends that auto-version (OpenAI's
// "gpt-4-turbo" → "gpt-4-turbo-2024-04-09") need this dimension to
// distinguish actual deployment from the alias.
func TestParseUsageExtended_ResponseModel(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-1",
		"object":"chat.completion",
		"model":"gpt-4-turbo-2024-04-09",
		"choices":[],
		"usage":{"prompt_tokens":10,"completion_tokens":5}
	}`)
	u := ParseUsageExtended(body)
	assert.Equal(t, "gpt-4-turbo-2024-04-09", u.Model)
	assert.Equal(t, int64(10), u.In)
	assert.Equal(t, int64(5), u.Out)
}

// TestParseUsageExtended_ModelOnlyNoUsage pins that an envelope
// carrying model but no usage (e.g. a 4xx error frame that still echoes
// the model) returns Model populated even when In/Out are zero. Wire
// layer can then call SetResponseModel even on error paths.
func TestParseUsageExtended_ModelOnlyNoUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-4-turbo","error":{"message":"bad input"}}`)
	u := ParseUsageExtended(body)
	assert.Equal(t, "gpt-4-turbo", u.Model)
	assert.Equal(t, int64(0), u.In)
	assert.Equal(t, int64(0), u.Out)
}

// TestParseUsageExtended_NoModel pins the empty-string default
// when model is absent — callers gate SetResponseModel on non-empty,
// so an empty Model leaves the recorder untouched rather than blanking
// it out.
func TestParseUsageExtended_NoModel(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	u := ParseUsageExtended(body)
	assert.Equal(t, "", u.Model)
}

// TestParseOllamaUsageExtended_ResponseModel pins the Ollama-shape model
// extraction. ParseOllamaUsage is the back-compat-stable form; callers
// that need the model use ParseOllamaUsageExtended.
func TestParseOllamaUsageExtended_ResponseModel(t *testing.T) {
	body := []byte(`{"model":"llama3:latest","done":true,"prompt_eval_count":8,"eval_count":12}`)
	in, out, model, ok := ParseOllamaUsageExtended(body)
	assert.True(t, ok)
	assert.Equal(t, int64(8), in)
	assert.Equal(t, int64(12), out)
	assert.Equal(t, "llama3:latest", model)

	// Streaming partial (done=false) returns nothing.
	_, _, _, ok = ParseOllamaUsageExtended([]byte(`{"model":"llama3","done":false}`))
	assert.False(t, ok)
}

// TestErrorTypeFromStatus pins the closed-enum mapping: dashboards rely
// on these strings being predictable. 429 splits out from generic 4xx
// because rate-limit alerts watch it as a distinct signal.
func TestErrorTypeFromStatus(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{200, ""},
		{301, ""},
		{400, "upstream_client_error"},
		{401, "upstream_client_error"},
		{404, "upstream_client_error"},
		{422, "upstream_client_error"},
		{429, "upstream_rate_limited"},
		{500, "upstream_server_error"},
		{502, "upstream_server_error"},
		{503, "upstream_server_error"},
		{504, "upstream_server_error"},
	}
	for _, c := range cases {
		got := errorTypeFromStatus(c.status)
		assert.Equal(t, c.want, got, "status=%d", c.status)
	}
}

// TestSetErrorFromStatus_NoOpOnSuccess regression-pins that 2xx leaves
// the recorder untouched so success-path series never carry an
// error_type label. The Phase 0 acceptance test +
// TestRing2_OTel_GenAIEmission both depend on this contract.
func TestSetErrorFromStatus_NoOpOnSuccess(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "p")
	SetErrorFromStatus(rec, 200)
	// Drive RecordCompletion and assert no error_type bled into the
	// payload via the inference log hook.
	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	defer llm.SetInferenceLogHook(nil)
	rec.RecordCompletion(0, 0)
	assert.Empty(t, captured.ErrorType, "200 must not tag error_type")
}

// TestSetErrorFromStatus_TagsErrorPath confirms a 5xx upstream tags
// the recorder; the inference log hook captures the resulting
// payload and exposes the label that the OTel histogram would also
// emit.
func TestSetErrorFromStatus_TagsErrorPath(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "p")
	SetErrorFromStatus(rec, 503)

	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	defer llm.SetInferenceLogHook(nil)
	rec.RecordCompletion(0, 0)
	assert.Equal(t, "upstream_server_error", captured.ErrorType)
}

// TestCaptureNonStreamingMetrics_TagsErrorBody confirms the wire-layer
// pre-pass: a non-2xx response tags the recorder before
// recordProxyMetrics fires RecordCompletion, so the metric carries
// error_type even when the body has no parseable usage fields.
func TestCaptureNonStreamingMetrics_TagsErrorBody(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "p")
	body := `{"error":{"message":"upstream is down","type":"server_error"}}`
	resp := &http.Response{
		StatusCode:    503,
		ContentLength: int64(len(body)),
		Body:          httpReadCloser(body),
		Header:        http.Header{},
	}

	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	defer llm.SetInferenceLogHook(nil)

	CaptureNonStreamingMetrics(resp, rec)
	assert.Equal(t, "upstream_server_error", captured.ErrorType,
		"5xx body without usage must still carry error_type label")
}

// TestCaptureNonStreamingMetrics_EmptyErrorBody covers the
// ContentLength=0 short-circuit. RecordCompletion still has to fire so
// the metric emits at all — silent drop on empty error bodies was the
// pre-D2.4 bug class.
func TestCaptureNonStreamingMetrics_EmptyErrorBody(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "p")
	resp := &http.Response{
		StatusCode:    502,
		ContentLength: 0,
		Body:          http.NoBody,
		Header:        http.Header{},
	}

	var called bool
	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) {
		called = true
		captured = d
	}))
	defer llm.SetInferenceLogHook(nil)

	CaptureNonStreamingMetrics(resp, rec)
	assert.True(t, called, "empty 5xx must still fire RecordCompletion")
	assert.Equal(t, "upstream_server_error", captured.ErrorType)
}

// TestCaptureNonStreamingMetrics_PopulatesResponseModel pins the
// gen_ai.response.model integration end-to-end: the wire-layer
// usage parser extracts the response.model from the body, calls
// recorder.SetResponseModel, and the value flows through to the
// inference log payload (and via the recorder to the OTel span +
// gen_ai.client.* metrics).
//
// Closes the deferred half of the Phase 6 cold-review must-fix —
// before this commit, the recorder API existed but no production
// caller invoked it, so gen_ai.response.model landed empty on every
// span.
func TestCaptureNonStreamingMetrics_PopulatesResponseModel(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4-turbo", "openai")
	body := `{
		"id":"chatcmpl-1",
		"object":"chat.completion",
		"model":"gpt-4-turbo-2024-04-09",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5}
	}`
	resp := &http.Response{
		StatusCode:    200,
		ContentLength: int64(len(body)),
		Body:          httpReadCloser(body),
		Header:        http.Header{},
	}

	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	defer llm.SetInferenceLogHook(nil)

	CaptureNonStreamingMetrics(resp, rec)
	assert.Equal(t, "gpt-4-turbo-2024-04-09", captured.ResponseModel,
		"wire-layer recordProxyMetrics must call SetResponseModel from the response body")
	assert.Equal(t, "gpt-4-turbo", captured.Model,
		"request.model on the inference log payload should still echo the request-side model")
}

// TestCaptureNonStreamingMetrics_OllamaResponseModel pins the same
// contract for the Ollama wire shape, exercising the
// ParseOllamaUsageExtended path in recordProxyMetrics.
func TestCaptureNonStreamingMetrics_OllamaResponseModel(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "llama3", "ollama")
	body := `{"model":"llama3:latest","done":true,"prompt_eval_count":7,"eval_count":11}`
	resp := &http.Response{
		StatusCode:    200,
		ContentLength: int64(len(body)),
		Body:          httpReadCloser(body),
		Header:        http.Header{},
	}

	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	defer llm.SetInferenceLogHook(nil)

	CaptureNonStreamingMetrics(resp, rec)
	assert.Equal(t, "llama3:latest", captured.ResponseModel)
	assert.Equal(t, int64(7), captured.TokensIn)
	assert.Equal(t, int64(11), captured.TokensOut)
}

// hookFn adapts a bare func into the inference log hook interface.
type hookFn func(llm.InferenceLogData)

func (h hookFn) OnInferenceComplete(d llm.InferenceLogData) { h(d) }

// httpReadCloser wraps a string in an http.NoBody-shaped reader for
// fixture responses.
func httpReadCloser(s string) interface {
	Read([]byte) (int, error)
	Close() error
} {
	return readCloser{Reader: strings.NewReader(s)}
}

type readCloser struct{ *strings.Reader }

func (readCloser) Close() error { return nil }
