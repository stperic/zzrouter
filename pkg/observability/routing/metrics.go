// Package routing implements the zz.routing.decisions.metric counter,
// the only OTel-internal observability instrument that captures
// zzrouter's per-request routing decisions. Multi-node operators rely
// on this to validate that traffic lands on the nodes they expect —
// without it, a misconfigured cluster routing setup is only visible in
// debug logs.
//
// Naming: invented under the zz.* prefix because neither OTel nor
// LiteLLM defines a routing-decisions concept (LiteLLM doesn't manage
// cluster topology). The label set borrows OTel's gen_ai.* keys for
// dimensions that overlap with the existing GenAI metrics so dashboards
// can JOIN by provider/operation against the GenAI client/server
// histograms.
//
// CLUSTER DOUBLE-EMIT: a single user-facing request that traverses
// coordinator → worker emits twice — once on the coord with
// (cluster_aware, remote) and once on the worker with
// (local_only, local). This is the intended invariant; "did traffic
// land where I expected?" requires both events. Dashboards summing
// rate(zz_routing_decisions_metric_total) across the cluster will
// show ~2× user-facing QPS — slice by `strategy` to see one half or
// the other.
//
// SCOPE: emits only on LLM-facing dispatch paths (chat/completions,
// embeddings, images, moderations, audio, /api/* compat, /v1/responses
// affinity, fallback proxy). Cluster control-plane traffic
// (/zzrouter/v1/internal/*) and management commands (/api/show,
// /api/copy, …) intentionally do not emit — those aren't routing
// decisions in the LLM sense.
//
// CARDINALITY: bounded. strategy ∈ 3 values × outcome ∈ 4 values × ~10
// operations × ~12 providers ≈ 1.4k series upper bound across the
// cluster. Safe.
package routing

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// maxFallbackIndexLabel caps the fallback_index label value so a
// pathological request that walked through 50 candidates doesn't
// expand the time-series cardinality unboundedly. Any actual index
// above this ceiling collapses to the same bucket on the wire — the
// label still carries the signal "this was a deep fallback walk".
const maxFallbackIndexLabel = 4

// Strategy is the closed-enum tag identifying which routing component
// made the decision. The set is bounded by zzrouter's three router
// implementations + the fallback proxy.
type Strategy string

const (
	StrategyLocalOnly    Strategy = "local_only"    // worker-mode local-only dispatch
	StrategyClusterAware Strategy = "cluster_aware" // coordinator dispatch
	StrategyFallback     Strategy = "fallback"      // multi-deployment failover via fallback proxy
)

// Outcome is the closed-enum tag identifying how the request was
// routed. NoBackend means the dispatcher couldn't satisfy the
// request (no model resolver match, no candidate, etc.) — it never
// reached a wire-level call.
//
// The fallback path emits per-deployment Local/Remote on commit
// (matching the actual node the chain settled on) instead of a
// generic "fallback" outcome — keeping the operator's view of "where
// did this land" identical to the non-fallback path. Strategy still
// distinguishes whether the chain ran.
type Outcome string

const (
	OutcomeLocal     Outcome = "local"      // dispatched to this node's local backend
	OutcomeRemote    Outcome = "remote"     // proxied to a remote cluster peer
	OutcomeNoBackend Outcome = "no_backend" // dispatcher couldn't reach any backend
)

// Metrics holds the routing decisions counter.
type Metrics struct {
	decisions metric.Int64Counter
}

var (
	mu             sync.Mutex
	globalMetrics  *Metrics
	globalProvider metric.MeterProvider
)

// GetMetrics returns the package-level Metrics, lazy-initialising on
// the current global meter provider. Mirrors the
// rebuild-on-provider-change pattern used elsewhere in
// pkg/observability/* so test isolation works.
func GetMetrics() *Metrics {
	mu.Lock()
	defer mu.Unlock()
	current := otel.GetMeterProvider()
	if globalMetrics != nil && globalProvider == current {
		return globalMetrics
	}
	m, err := newMetrics()
	if err != nil {
		slog.Warn("zz.routing.decisions.metric init failed",
			"error", err,
			"impact", "routing-decisions counter will not emit; cluster-routing dashboards will see no data",
		)
		return nil
	}
	globalMetrics = m
	globalProvider = current
	return globalMetrics
}

func newMetrics() (*Metrics, error) {
	meter := otel.Meter("zzrouter.routing")
	decisions, err := meter.Int64Counter(
		"zz.routing.decisions.metric",
		metric.WithDescription("Per-request routing decisions, sliced by strategy + outcome"),
		metric.WithUnit("{decision}"),
	)
	if err != nil {
		return nil, err
	}
	return &Metrics{decisions: decisions}, nil
}

// RecordDecision increments zz.routing.decisions.metric for one
// dispatch decision. provider and operation use the same OTel-canonical
// vocabulary as the GenAI metrics — empty values are valid Prometheus
// labels and required so dashboards filtering on {provider="vllm"}
// don't silently miss empty-provider series.
//
// fallback_index=0 — single-attempt dispatches (non-fallback paths)
// land here via this entry point. Fallback paths use the
// RecordFallbackDecision variant which carries the actual index.
func (m *Metrics) RecordDecision(ctx context.Context, strategy Strategy, outcome Outcome, provider, operation string) {
	m.recordDispatch(ctx, strategy, outcome, provider, operation, 0)
}

// RecordFallbackDecision is the fallback-path variant that records
// which step in the fallback chain produced this outcome. The label
// is capped at maxFallbackIndexLabel to keep cardinality bounded.
func (m *Metrics) RecordFallbackDecision(ctx context.Context, strategy Strategy, outcome Outcome, provider, operation string, fallbackIndex int) {
	if fallbackIndex > maxFallbackIndexLabel {
		fallbackIndex = maxFallbackIndexLabel
	}
	if fallbackIndex < 0 {
		fallbackIndex = 0
	}
	m.recordDispatch(ctx, strategy, outcome, provider, operation, fallbackIndex)
}

func (m *Metrics) recordDispatch(ctx context.Context, strategy Strategy, outcome Outcome, provider, operation string, fallbackIndex int) {
	if m == nil {
		return
	}
	m.decisions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("strategy", string(strategy)),
		attribute.String("outcome", string(outcome)),
		attribute.String("gen_ai.provider.name", provider),
		attribute.String("gen_ai.operation.name", operation),
		attribute.Int("fallback_index", fallbackIndex),
	))
}

// RecordDecision is the package-level shortcut around GetMetrics.
func RecordDecision(ctx context.Context, strategy Strategy, outcome Outcome, provider, operation string) {
	GetMetrics().RecordDecision(ctx, strategy, outcome, provider, operation)
}

// RecordFallbackDecision is the package-level shortcut for the
// fallback-aware variant.
func RecordFallbackDecision(ctx context.Context, strategy Strategy, outcome Outcome, provider, operation string, fallbackIndex int) {
	GetMetrics().RecordFallbackDecision(ctx, strategy, outcome, provider, operation, fallbackIndex)
}

// SetRouteSpanAttrs stamps route-level attributes on the active span
// in ctx. No-op when there's no active span. The five attributes
// correspond to the four response headers (Route / Replica / Strategy
// / Fallback-Count) plus the count of pre-filter skipped replicas.
func SetRouteSpanAttrs(ctx context.Context, groupName, replica, strategy string, fallbackCount, skippedCount int) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	attrs := make([]attribute.KeyValue, 0, 5)
	if groupName != "" {
		attrs = append(attrs, attribute.String("zz.route.name", groupName))
	}
	if replica != "" {
		attrs = append(attrs, attribute.String("zz.route.replica", replica))
	}
	if strategy != "" {
		attrs = append(attrs, attribute.String("zz.route.strategy", strategy))
	}
	attrs = append(attrs,
		attribute.Int("zz.route.fallback.count", fallbackCount),
		attribute.Int("zz.route.skipped.count", skippedCount),
	)
	span.SetAttributes(attrs...)
}

// Note for future readers: the package previously exported
// OutcomeFallback as a fourth closed-enum value. It was dead — no
// emit site used it, because the fallback proxy now reports
// per-deployment Local/Remote on commit. Removed to keep the
// closed-enum tight and match what the wire actually carries.
