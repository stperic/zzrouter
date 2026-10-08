package wire

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteModelInBody(t *testing.T) {
	original := []byte(`{"model":"old","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)
	rewritten := RewriteModelInBody(original, "new")

	var obj map[string]any
	if err := json.Unmarshal(rewritten, &obj); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}
	assert.Equal(t, "new", obj["model"])
	assert.Equal(t, 0.7, obj["temperature"])
	assert.NotNil(t, obj["messages"])
}

func TestRewriteModelInBody_InvalidJSONReturnsOriginal(t *testing.T) {
	original := []byte(`not json`)
	rewritten := RewriteModelInBody(original, "new")
	assert.Equal(t, original, rewritten)
}

// Token counts are what the inference log meters and what budget settlement
// spends against, so include_usage is forced on rather than defaulted. The
// previous contract — leave any caller-supplied stream_options alone — meant
// `{"include_usage": false}` logged 0 in / 0 out, i.e. a caller could opt out
// of being billed. callerAsked is the separate question of what they SEE.
func TestForceUsageReporting(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantInclude any
		wantAsked   bool
	}{
		{
			name:        "streaming request gets usage turned on",
			body:        `{"model":"m","stream":true}`,
			wantInclude: true,
			wantAsked:   false,
		},
		{
			name:        "an explicit opt-out is overridden for metering",
			body:        `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`,
			wantInclude: true,
			wantAsked:   false,
		},
		{
			// Already asking means the frame is theirs to see, so nothing
			// downstream should strip it.
			name:        "a caller who asked is left alone and marked asked",
			body:        `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
			wantInclude: true,
			wantAsked:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, asked := ForceUsageReporting([]byte(tt.body))

			var obj map[string]any
			require.NoError(t, json.Unmarshal(out, &obj))
			opts, ok := obj["stream_options"].(map[string]any)
			require.True(t, ok, "stream_options missing from %s", out)
			assert.Equal(t, tt.wantInclude, opts["include_usage"])
			assert.Equal(t, tt.wantAsked, asked)
		})
	}
}

// Usage is already in a non-streaming response body and there is no frame to
// strip, so the body is untouched and callerAsked is true — a caller of this
// function must not go stripping frames on a path that has none.
func TestForceUsageReporting_NonStreamingUntouched(t *testing.T) {
	original := []byte(`{"model":"m"}`)
	out, asked := ForceUsageReporting(original)

	assert.Equal(t, original, out)
	assert.True(t, asked)
}

func TestForceUsageReporting_UnparseableBodyUntouched(t *testing.T) {
	original := []byte(`{not json`)
	out, asked := ForceUsageReporting(original)

	assert.Equal(t, original, out)
	assert.False(t, asked, "nothing was forced, so nothing may be stripped either")
}

func TestInjectRoutingMetadata_AddsBlock(t *testing.T) {
	body := []byte(`{"id":"x","usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	out := InjectRoutingMetadata(body, RoutingMetadata{Provider: "openai", Deployment: "dep-a", Node: "node-1"})

	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	meta, ok := obj["zzrouter"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "openai", meta["provider"])
	assert.Equal(t, "dep-a", meta["deployment"])
	assert.Equal(t, "node-1", meta["node"])
}

func TestMaybeInjectRoutingMetadata_TolerantOfLeadingWhitespace(t *testing.T) {
	// Some upstream gateways pretty-print their JSON or pad with leading
	// whitespace. The gate must accept these as valid inference responses
	// (json.Unmarshal does, per RFC 8259).
	body := []byte("\n   \t \n" + `{"id":"x","usage":{"prompt_tokens":1}}`)
	out := MaybeInjectRoutingMetadata(body, RoutingMetadata{Provider: "openrouter"}, nil)
	assert.Contains(t, string(out), `"zzrouter"`,
		"leading whitespace must not block the inject path")
}

func TestMaybeInjectRoutingMetadata_NonInferenceFallsThrough(t *testing.T) {
	// Files / models / unrelated pass-through bodies don't carry "usage";
	// without a deployment, they must not get a zzrouter block.
	body := []byte(`{"object":"list","data":[{"id":"file-1"}]}`)
	out := MaybeInjectRoutingMetadata(body, RoutingMetadata{Provider: "openai"}, nil)
	assert.Equal(t, body, out, "non-inference response must pass through unchanged")
}

func TestInjectRoutingMetadata_NonStreamingDoesNotTouchUsage(t *testing.T) {
	// Non-streaming responses carry the root zzrouter block only; the
	// zz_* namespace is reserved for streaming SSE usage frames. Even
	// when InjectUsage is true (the streaming-side gate), the
	// non-streaming inject must leave the usage block byte-for-byte.
	body := []byte(`{"id":"x","usage":{"prompt_tokens":5}}`)
	out := InjectRoutingMetadata(body, RoutingMetadata{Provider: "openai", Deployment: "dep-a", Node: "node-1", InjectUsage: true})

	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	usage, ok := obj["usage"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, float64(5), usage["prompt_tokens"])
	assert.Len(t, usage, 1, "non-streaming must not inject zz_* into usage")
}

func TestInjectUsageIntoSSEChunk_RewritesUsageLines(t *testing.T) {
	chunk := []byte(
		`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
			`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)
	out := InjectUsageIntoSSEChunk(chunk, RoutingMetadata{Provider: "openai", Deployment: "dep-a", Node: "node-1"})
	s := string(out)

	// Non-usage chunk unchanged.
	assert.Contains(t, s, `"content":"hi"`)
	// [DONE] marker preserved.
	assert.True(t, strings.Contains(s, "data: [DONE]"), "DONE marker must be preserved")
	// zz_* fields injected into usage chunk.
	assert.Contains(t, s, `"zz_provider":"openai"`)
	assert.Contains(t, s, `"zz_model":"dep-a"`)
	assert.Contains(t, s, `"zz_node":"node-1"`)
}

func TestInjectUsageIntoSSEChunk_NoUsageIsNoOp(t *testing.T) {
	chunk := []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n")
	out := InjectUsageIntoSSEChunk(chunk, RoutingMetadata{Provider: "openai", Deployment: "dep-a", Node: "node-1"})
	assert.Equal(t, chunk, out)
}

func TestInjectRoutingMetadata_OmitsCostWhenSourceEmpty(t *testing.T) {
	body := []byte(`{"id":"x"}`)
	out := InjectRoutingMetadata(body, RoutingMetadata{
		Provider:   "openai",
		Deployment: "dep-a",
		// CostSource intentionally empty
		CostUSD: 0.123, // ignored
	})
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	meta := obj["zzrouter"].(map[string]any)
	_, hasCost := meta["cost_usd"]
	_, hasSrc := meta["cost_source"]
	assert.False(t, hasCost, "empty CostSource ⇒ cost_usd omitted")
	assert.False(t, hasSrc, "empty CostSource ⇒ cost_source omitted")
}

func TestInjectRoutingMetadata_EmitsZeroCostForLocalProvider(t *testing.T) {
	body := []byte(`{"id":"x"}`)
	out := InjectRoutingMetadata(body, RoutingMetadata{
		Provider:   "llamacpp",
		Deployment: "dep-l",
		CostSource: CostSourceZZRouter, // local providers signal "no marginal cost"
		CostUSD:    0,
	})
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	meta := obj["zzrouter"].(map[string]any)
	assert.Equal(t, float64(0), meta["cost_usd"],
		"local providers emit cost_usd=0; absence-of-cost is the signal")
	assert.Equal(t, "zzrouter", meta["cost_source"])
}

func TestInjectRoutingMetadata_EmitsTimingWhenNonZero(t *testing.T) {
	body := []byte(`{"id":"x"}`)
	out := InjectRoutingMetadata(body, RoutingMetadata{
		Provider:     "openrouter",
		Deployment:   "dep-r",
		CostSource:   CostSourceProvider,
		CostUSD:      0.000001989,
		LatencyMs:    326,
		TTFTMs:       142,
		TokensPerSec: 95.2,
	})
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	meta := obj["zzrouter"].(map[string]any)
	assert.InDelta(t, 0.000001989, meta["cost_usd"], 1e-12)
	assert.Equal(t, "provider", meta["cost_source"])
	assert.EqualValues(t, 326, meta["latency_ms"])
	assert.EqualValues(t, 142, meta["ttft_ms"])
	assert.InDelta(t, 95.2, meta["tokens_per_second"], 1e-9)
}

func TestInjectUsageIntoSSEChunk_CostOnlyOnTerminalUsage(t *testing.T) {
	// Two usage frames: a partial (completion_tokens=0) and a terminal
	// (completion_tokens=3). Cost must land only on the terminal one.
	chunk := []byte(
		`data: {"choices":[],"usage":{"prompt_tokens":19,"completion_tokens":0}}` + "\n\n" +
			`data: {"choices":[],"usage":{"prompt_tokens":19,"completion_tokens":3}}` + "\n\n",
	)
	out := InjectUsageIntoSSEChunk(chunk, RoutingMetadata{
		Provider:   "openrouter",
		Deployment: "dep-r",
		CostSource: CostSourceProvider,
		CostUSD:    0.000001989,
	})
	s := string(out)
	// zz_provider stamped on both frames (idempotent routing fields).
	assert.Equal(t, 2, strings.Count(s, `"zz_provider":"openrouter"`))
	// zz_cost_usd stamped only once (terminal frame).
	assert.Equal(t, 1, strings.Count(s, `"zz_cost_usd":`),
		"cost must stamp only on terminal usage chunk (completion_tokens > 0)")
	assert.Equal(t, 1, strings.Count(s, `"zz_cost_source":"provider"`))
}

func TestInjectRoutingMetadata_EmitsRouteFieldsWhenPopulated(t *testing.T) {
	body := []byte(`{"id":"x"}`)
	out := InjectRoutingMetadata(body, RoutingMetadata{
		Provider:          "openrouter",
		GroupName:         "fast-chat",
		StrategyUsed:      "priority",
		DecisionLatencyMs: 4,
		FallbackChain: []FallbackAttempt{
			{Replica: "r1", Status: 503, DurationMs: 12, Outcome: "error"},
			{Replica: "r2", Status: 200, DurationMs: 34, Outcome: "success"},
		},
		Skipped: []SkippedReplica{
			{Replica: "r3", Reason: "cooldown"},
		},
	})
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	meta := obj["zzrouter"].(map[string]any)
	assert.Equal(t, "fast-chat", meta["group_name"])
	assert.Equal(t, "priority", meta["strategy_used"])
	assert.EqualValues(t, 4, meta["decision_latency_ms"])
	chain := meta["fallback_chain"].([]any)
	require.Len(t, chain, 2)
	first := chain[0].(map[string]any)
	assert.Equal(t, "r1", first["replica"])
	assert.EqualValues(t, 503, first["status"])
	assert.Equal(t, "error", first["outcome"])
	skipped := meta["skipped"].([]any)
	require.Len(t, skipped, 1)
	assert.Equal(t, "cooldown", skipped[0].(map[string]any)["reason"])
}

func TestInjectRoutingMetadata_OmitsRouteFieldsWhenZero(t *testing.T) {
	body := []byte(`{"id":"x"}`)
	out := InjectRoutingMetadata(body, RoutingMetadata{
		Provider: "ollama",
	})
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	meta := obj["zzrouter"].(map[string]any)
	for _, k := range []string{
		"group_name", "strategy_used", "fallback_chain", "skipped",
		"decision_latency_ms", "steering_applied",
		"estimated_cost_micro", "estimated_cost_source",
	} {
		_, present := meta[k]
		assert.False(t, present, "%q must be omitted when zero/empty", k)
	}
}

func TestInjectRoutingMetadata_OmitsEstimatedCostWhenSourceEmpty(t *testing.T) {
	body := []byte(`{"id":"x"}`)
	out := InjectRoutingMetadata(body, RoutingMetadata{
		Provider:           "openai",
		EstimatedCostMicro: 12345, // populated but no source
	})
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	meta := obj["zzrouter"].(map[string]any)
	_, hasMicro := meta["estimated_cost_micro"]
	_, hasSrc := meta["estimated_cost_source"]
	assert.False(t, hasMicro, "estimated cost is paired with its source")
	assert.False(t, hasSrc)
}

func TestInjectRoutingMetadata_EmitsEstimatedCostWhenSourcePresent(t *testing.T) {
	body := []byte(`{"id":"x"}`)
	out := InjectRoutingMetadata(body, RoutingMetadata{
		Provider:            "openai",
		EstimatedCostMicro:  500,
		EstimatedCostSource: "preview",
	})
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	meta := obj["zzrouter"].(map[string]any)
	assert.EqualValues(t, 500, meta["estimated_cost_micro"])
	assert.Equal(t, "preview", meta["estimated_cost_source"])
}

func TestInjectRoutingMetadata_EmitsSteeringAppliedWhenPopulated(t *testing.T) {
	body := []byte(`{"id":"x"}`)
	out := InjectRoutingMetadata(body, RoutingMetadata{
		Provider:        "openai",
		SteeringApplied: []string{"latency_budget_ms", "exclude_replicas"},
	})
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	meta := obj["zzrouter"].(map[string]any)
	applied := meta["steering_applied"].([]any)
	require.Len(t, applied, 2)
	assert.Equal(t, "latency_budget_ms", applied[0])
}

func TestSuppressBodyFromVerbose(t *testing.T) {
	cases := map[string]bool{
		"0":    true,
		"1":    false,
		"":     false,
		"true": false, // strict closed-enum: only "0" suppresses
		"yes":  false,
		"00":   false,
	}
	for in, want := range cases {
		got := SuppressBodyFromVerbose(in)
		assert.Equal(t, want, got, "SuppressBodyFromVerbose(%q)", in)
	}
}

func TestInjectRoutingMetadata_SuppressBodyOmitsBlock(t *testing.T) {
	body := []byte(`{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	out := MaybeInjectRoutingMetadata(body, RoutingMetadata{
		Provider:             "openai",
		Deployment:           "dep-a",
		SuppressBodyMetadata: true,
	}, nil)
	assert.NotContains(t, string(out), `"zzrouter"`,
		"SuppressBodyMetadata=true must skip the in-body block")
}

func TestInjectRoutingMetadata_EmitsBlockWhenVerboseAbsent(t *testing.T) {
	body := []byte(`{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	out := MaybeInjectRoutingMetadata(body, RoutingMetadata{
		Provider:             "openai",
		Deployment:           "dep-a",
		SuppressBodyMetadata: false,
	}, nil)
	assert.Contains(t, string(out), `"zzrouter"`,
		"default (verbose=1) must keep the in-body block")
}
