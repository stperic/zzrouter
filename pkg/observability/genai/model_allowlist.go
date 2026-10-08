package genai

import "sync/atomic"

// UnknownModelLabel is the sentinel emitted on requested_model /
// gen_ai.request.model when a checker registered via
// SetModelAllowlist rejects the caller-supplied name.
// Bounded-cardinality replacement for caller-controlled values that
// aren't on the operator's allowlist.
//
// Lives in this package so every metric / span emission site that
// touches the request-side model name routes through one gate.
const UnknownModelLabel = "_unknown_"

// modelAllowlist holds the optional checker that gates caller-supplied
// model names against an operator-defined allowlist (typically the
// model registry). nil means "no gating" — the raw caller value passes
// through unchanged. Operators concerned about adversarial cardinality
// spraying call SetModelAllowlist on startup.
var modelAllowlist atomic.Pointer[func(string) bool]

// SetModelAllowlist registers an optional checker for the
// caller-controlled model labels emitted by the proxy + llm metrics
// and the inference span. When the checker returns false for a model
// name, the label collapses to UnknownModelLabel rather than emitting
// the raw value. This is the operator-facing knob to bound cardinality
// on the request_model / gen_ai.request.model dimensions.
//
// Pass nil to clear a previous registration. Concurrency-safe; the
// active checker is loaded atomically on each metric emission.
//
// Wiring example (from server startup):
//
//	known := loadKnownModels(registry)
//	genai.SetModelAllowlist(func(name string) bool { _, ok := known[name]; return ok })
//
// Callers that want a periodic snapshot refresh should swap the closure
// out via a fresh SetModelAllowlist call after each refresh.
//
// COVERAGE: this gate runs at every site that emits a caller-controlled
// model on a metric or span — the proxy failure counter
// (zz.proxy.failed.requests.metric), the inflight gauge
// (zz.proxy.inflight.requests.metric), the OTel GenAI client metrics
// (gen_ai.client.operation.duration / gen_ai.client.token.usage /
// gen_ai.server.{time_to_first_token, request.duration,
// time_per_output_token}), and the inference span attributes
// (gen_ai.request.model on llm.inference / llm.model.load spans).
func SetModelAllowlist(checker func(string) bool) {
	if checker == nil {
		modelAllowlist.Store(nil)
		return
	}
	modelAllowlist.Store(&checker)
}

// SwapModelAllowlist sets a fresh checker and returns a restore
// closure that puts the previous value back. Use from tests:
//
//	defer genai.SwapModelAllowlist(myChecker)()
//
// Avoids the cleanup-resets-to-nil hazard where t.Cleanup
// SetModelAllowlist(nil) would clobber a production wiring set by an
// earlier startup hook. In unit-test contexts the previous value is
// usually nil; in integration contexts where startup may have set a
// real allowlist, the prior value is preserved.
func SwapModelAllowlist(checker func(string) bool) func() {
	prev := modelAllowlist.Load()
	SetModelAllowlist(checker)
	return func() {
		if prev == nil {
			modelAllowlist.Store(nil)
			return
		}
		modelAllowlist.Store(prev)
	}
}

// GateModelLabel applies the registered allowlist (if any) to a
// caller-supplied model name. Returns the input verbatim when no
// checker is set or when the checker accepts; returns UnknownModelLabel
// when the checker rejects. Empty input always returns empty (the gate
// preserves the existing "absent label" semantic at call sites that
// gate on `!= ""`).
//
// Hot-path safe: a single atomic load + at most one indirect call.
func GateModelLabel(name string) string {
	if name == "" {
		return ""
	}
	checker := modelAllowlist.Load()
	if checker == nil {
		return name
	}
	if (*checker)(name) {
		return name
	}
	return UnknownModelLabel
}
