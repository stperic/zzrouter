package routing

import (
	"context"
	"testing"
)

// TestRecordDecision_NilSafety pins the no-op path on nil receivers,
// legitimate during early-init when the meter provider hasn't bound.
func TestRecordDecision_NilSafety(t *testing.T) {
	var m *Metrics
	m.RecordDecision(context.Background(), StrategyClusterAware, OutcomeLocal, "vllm", "chat")
	GetMetrics().RecordDecision(context.Background(), StrategyLocalOnly, OutcomeRemote, "", "")
}

// TestStrategyAndOutcome_ClosedEnums pins the canonical values. A drift
// here would silently break dashboard panels filtering on these
// labels, since Prometheus treats them as opaque strings.
func TestStrategyAndOutcome_ClosedEnums(t *testing.T) {
	wantStrategies := map[Strategy]string{
		StrategyLocalOnly:    "local_only",
		StrategyClusterAware: "cluster_aware",
		StrategyFallback:     "fallback",
	}
	for s, want := range wantStrategies {
		if string(s) != want {
			t.Errorf("Strategy(%v) = %q, want %q", s, string(s), want)
		}
	}
	wantOutcomes := map[Outcome]string{
		OutcomeLocal:     "local",
		OutcomeRemote:    "remote",
		OutcomeNoBackend: "no_backend",
	}
	for o, want := range wantOutcomes {
		if string(o) != want {
			t.Errorf("Outcome(%v) = %q, want %q", o, string(o), want)
		}
	}
}

func TestRecordFallbackDecision_NilSafety(t *testing.T) {
	var m *Metrics
	m.RecordFallbackDecision(context.Background(), StrategyFallback, OutcomeLocal, "ollama", "chat", 2)
	GetMetrics().RecordFallbackDecision(context.Background(), StrategyFallback, OutcomeRemote, "vllm", "chat", 99)
}

func TestSetRouteSpanAttrs_NoActiveSpanIsNoop(t *testing.T) {
	// Default span returned by SpanFromContext on a vanilla context is
	// non-recording; the helper must short-circuit without panicking.
	SetRouteSpanAttrs(context.Background(), "fast-chat", "r1", "priority", 1, 0)
}
