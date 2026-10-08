package spend

import (
	"context"
	"testing"

	"github.com/stperic/zzrouter/pkg/observability/genai"
)

// TestRecordTokens_NilSafety pins that the recorder no-ops on nil
// receivers and on zero token counts — legitimate paths during early
// startup or for embedding routes that produce no completion tokens.
func TestRecordTokens_NilSafety(t *testing.T) {
	var m *Metrics
	m.RecordTokens(context.Background(), CallerLabels{}, 100, 200)        // nil receiver
	GetMetrics().RecordTokens(context.Background(), CallerLabels{}, 0, 0) // zero counts
}

// TestRecordSpendUSD_NilSafety pins the no-op paths for the spend
// counter — nil receiver, zero/negative cost. The negative-cost clamp
// prevents upstream-reported credit refunds (some cloud providers
// occasionally return negative cost values for billing adjustments)
// from decrementing the cumulative counter, which Prometheus would
// reject anyway as a counter reset.
func TestRecordSpendUSD_NilSafety(t *testing.T) {
	var m *Metrics
	m.RecordSpendUSD(context.Background(), CallerLabels{}, 1.23) // nil receiver
	GetMetrics().RecordSpendUSD(context.Background(), CallerLabels{}, 0)
	GetMetrics().RecordSpendUSD(context.Background(), CallerLabels{}, -5.0) // clamped to zero
}

// TestCallerLabels_GatesModelDimensions pins that requested_model,
// model, and model_group all flow through genai.GateModelLabel before
// landing on a series — when an operator wires SetModelAllowlist to
// reject "evil", the label collapses to UnknownModelLabel rather than
// emitting "evil" verbatim. The gate covers the OTel client metrics
// already (pkg/observability/llm), and applying it to the LiteLLM-
// mirrored counters keeps a single chokepoint instead of opening a
// second cardinality avenue. Other labels (api_key_alias, team, etc.)
// are bounded by the active virtual-key + team count and don't pass
// through the gate.
func TestCallerLabels_GatesModelDimensions(t *testing.T) {
	defer genai.SwapModelAllowlist(func(name string) bool {
		return name == "ok"
	})()

	labels := CallerLabels{
		APIKeyAlias:    "alice-prod",
		HashedAPIKey:   "abcd-1234",
		Team:           "team-1",
		TeamAlias:      "Engineering",
		Model:          "evil",
		ModelGroup:     "ok",
		RequestedModel: "evil",
		APIProvider:    "openai",
	}
	attrs := labels.attributes()

	got := map[string]string{}
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value.AsString()
	}

	if got["model"] != genai.UnknownModelLabel {
		t.Errorf("model = %q, want %q (gated)", got["model"], genai.UnknownModelLabel)
	}
	if got["requested_model"] != genai.UnknownModelLabel {
		t.Errorf("requested_model = %q, want %q (gated)", got["requested_model"], genai.UnknownModelLabel)
	}
	if got["model_group"] != "ok" {
		t.Errorf("model_group = %q, want passthrough", got["model_group"])
	}
	// Non-model labels remain passthrough — the gate does not apply.
	if got["api_key_alias"] != "alice-prod" {
		t.Errorf("api_key_alias = %q, want %q", got["api_key_alias"], "alice-prod")
	}
	if got["hashed_api_key"] != "abcd-1234" {
		t.Errorf("hashed_api_key = %q, want %q", got["hashed_api_key"], "abcd-1234")
	}
	if got["team_alias"] != "Engineering" {
		t.Errorf("team_alias = %q, want %q", got["team_alias"], "Engineering")
	}
}

// TestCallerLabels_EmitsAllLiteLLMLabels pins the wire-vocabulary
// contract: every label LiteLLM dashboards filter on must appear on
// every series, even when empty. Missing labels would break
// `s/litellm/zz/`-translated dashboards that include selectors like
// {team_alias=~".+"} (matches non-empty values) — those queries
// silently match nothing if the label was never emitted at all,
// instead of returning empty-string series the operator can recognize.
func TestCallerLabels_EmitsAllLiteLLMLabels(t *testing.T) {
	defer genai.SwapModelAllowlist(nil)() // ensure passthrough

	attrs := CallerLabels{}.attributes()
	got := map[string]bool{}
	for _, kv := range attrs {
		got[string(kv.Key)] = true
	}

	for _, want := range []string{
		"api_key_alias", "hashed_api_key",
		"team", "team_alias",
		"user", "end_user",
		"model", "model_group", "requested_model",
		"api_provider",
	} {
		if !got[want] {
			t.Errorf("missing label %q on empty CallerLabels", want)
		}
	}
}
