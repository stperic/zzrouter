package harness

import (
	"strings"
	"testing"
)

// TestParseMetrics_Basic exercises the canonical Prometheus exposition
// shapes the OTel SDK emits: histogram with labels (count, sum,
// buckets), labelless counter, labels with otel_scope_* extras.
func TestParseMetrics_Basic(t *testing.T) {
	body := strings.Join([]string{
		`# HELP gen_ai_client_operation_duration_seconds GenAI operation duration.`,
		`# TYPE gen_ai_client_operation_duration_seconds histogram`,
		`gen_ai_client_operation_duration_seconds_bucket{gen_ai_operation_name="chat",gen_ai_provider_name="llama_cpp",gen_ai_request_model="Qwen/Qwen2.5-1.5B",otel_scope_name="zzrouter.llm",le="0.16"} 21`,
		`gen_ai_client_operation_duration_seconds_count{gen_ai_operation_name="chat",gen_ai_provider_name="llama_cpp",gen_ai_request_model="Qwen/Qwen2.5-1.5B",otel_scope_name="zzrouter.llm"} 26`,
		`gen_ai_client_operation_duration_seconds_sum{gen_ai_operation_name="chat",gen_ai_provider_name="llama_cpp",gen_ai_request_model="Qwen/Qwen2.5-1.5B",otel_scope_name="zzrouter.llm"} 3.205981`,
		`# HELP target_info Target metadata`,
		`# TYPE target_info gauge`,
		`target_info{service_name="zzrouter"} 1`,
	}, "\n")

	set, err := ParseMetrics(body)
	if err != nil {
		t.Fatalf("ParseMetrics: %v", err)
	}
	if len(set.Samples) != 4 {
		t.Fatalf("Samples=%d want 4", len(set.Samples))
	}

	required := map[string]string{
		"gen_ai_operation_name": "chat",
		"gen_ai_provider_name":  "llama_cpp",
		"gen_ai_request_model":  "Qwen/Qwen2.5-1.5B",
	}
	got, ok := set.HistogramCount("gen_ai_client_operation_duration_seconds", required)
	if !ok {
		t.Fatal("HistogramCount: no match for required labels")
	}
	if got != 26 {
		t.Errorf("HistogramCount=%v want 26", got)
	}

	sum, ok := set.HistogramSum("gen_ai_client_operation_duration_seconds", required)
	if !ok || sum != 3.205981 {
		t.Errorf("HistogramSum=%v ok=%v want 3.205981", sum, ok)
	}

	// target_info is the labelless-ish form (just service_name).
	if !set.AnyMatch("target_info", map[string]string{"service_name": "zzrouter"}) {
		t.Error("target_info{service_name=zzrouter} not matched")
	}
}

// TestParseMetrics_LabelMismatchReturnsFalse confirms required-label
// filtering is strict — wrong value, missing key, both miss the match.
func TestParseMetrics_LabelMismatchReturnsFalse(t *testing.T) {
	body := `gen_ai_client_token_usage_count{gen_ai_token_type="input",gen_ai_provider_name="ollama"} 42`
	set, err := ParseMetrics(body)
	if err != nil {
		t.Fatalf("ParseMetrics: %v", err)
	}

	// Right key, wrong value → no match.
	if _, ok := set.Match("gen_ai_client_token_usage_count",
		map[string]string{"gen_ai_token_type": "output"}); ok {
		t.Error("expected no match on token_type=output")
	}
	// Right value → match.
	if _, ok := set.Match("gen_ai_client_token_usage_count",
		map[string]string{"gen_ai_token_type": "input"}); !ok {
		t.Error("expected match on token_type=input")
	}
}

// TestParseMetrics_LabellessSeries covers the simpler form where a
// metric has no labels at all (process_start_time_seconds, etc.).
func TestParseMetrics_LabellessSeries(t *testing.T) {
	body := `process_start_time_seconds 1717480000.123`
	set, err := ParseMetrics(body)
	if err != nil {
		t.Fatalf("ParseMetrics: %v", err)
	}
	if len(set.Samples) != 1 {
		t.Fatalf("Samples=%d want 1", len(set.Samples))
	}
	if set.Samples[0].Value != 1717480000.123 {
		t.Errorf("Value=%v want 1717480000.123", set.Samples[0].Value)
	}
}

// TestParseMetrics_EmptyBody ensures an empty body decodes cleanly to
// an empty set rather than a parser error.
func TestParseMetrics_EmptyBody(t *testing.T) {
	set, err := ParseMetrics("")
	if err != nil {
		t.Fatalf("ParseMetrics(empty): %v", err)
	}
	if len(set.Samples) != 0 {
		t.Errorf("Samples=%d want 0", len(set.Samples))
	}
}
