// Package quotametrics implements the pkg/access/quota.MetricsRecorder
// interface using OpenTelemetry instruments. Metrics land on the
// current global OTel meter provider; when the OTel Prometheus
// exporter is wired (the default zzRouter configuration), they are
// visible at GET /metrics.
//
// Cardinality contract: every instrument declared here carries at most
// two labels (`scope` ∈ {"key","team"}, plus `outcome`/`reason` on
// decisions). Never add per-entity labels here — tenant and key IDs
// are unbounded and belong in the audit log or the per-entity API.
package quotametrics

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stperic/zzrouter/pkg/access/quota"
)

// Metric names — stable on the wire, prefix under `zzrouter.quota.*`
// to match the rest of zzRouter's OTel instruments (`zzrouter.llm.*`
// etc.). Renaming any of these invalidates existing dashboards and
// alert rules, so they are pinned by a test in this package.
const (
	metricDecisionTotal     = "zzrouter.quota.decision.total"
	metricSpendSettledUSD   = "zzrouter.quota.spend.settled.usd"
	metricConcurrencyActive = "zzrouter.quota.concurrency.active"
	attrScope               = "scope"
	attrOutcome             = "outcome"
	attrReason              = "reason"
	meterName               = "zzrouter.quota"
)

// Recorder is a quota.MetricsRecorder backed by OTel instruments.
// Construct with New and wire into quota.Enforcer via
// SetMetricsRecorder. Safe for concurrent use.
type Recorder struct {
	decisionCounter  metric.Int64Counter
	spendCounter     metric.Float64Counter
	concurrencyGauge metric.Int64UpDownCounter
}

// New builds a Recorder against the current global OTel meter
// provider. Returns an error on instrument registration failure; the
// caller should fall back to quota.NullMetricsRecorder{} so missing
// observability does not break enforcement.
func New() (*Recorder, error) {
	meter := otel.Meter(meterName)

	decisionCounter, err := meter.Int64Counter(
		metricDecisionTotal,
		metric.WithDescription("Total quota decisions, by scope, outcome, and reason."),
		// `{decision}` is an OTel "annotation" unit: the Prometheus
		// exporter renders the family as `_total` without appending
		// a per-unit suffix. Do not replace with a bare word.
		metric.WithUnit("{decision}"),
	)
	if err != nil {
		return nil, err
	}

	// Unit intentionally omitted: the metric name already carries
	// `usd`, and the OTel Prometheus exporter would otherwise append
	// `_USD` producing `zzrouter_quota_spend_settled_usd_USD_total`.
	spendCounter, err := meter.Float64Counter(
		metricSpendSettledUSD,
		metric.WithDescription("Total settled spend in USD, by scope."),
	)
	if err != nil {
		return nil, err
	}

	concurrencyGauge, err := meter.Int64UpDownCounter(
		metricConcurrencyActive,
		metric.WithDescription("Active concurrency slots held, by scope."),
		metric.WithUnit("{slot}"),
	)
	if err != nil {
		return nil, err
	}

	return &Recorder{
		decisionCounter:  decisionCounter,
		spendCounter:     spendCounter,
		concurrencyGauge: concurrencyGauge,
	}, nil
}

// ObserveDecision records one quota decision.
func (r *Recorder) ObserveDecision(scope quota.ScopeKind, outcome, reason string) {
	if r == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String(attrScope, string(scope)),
		attribute.String(attrOutcome, outcome),
	}
	if reason != "" {
		attrs = append(attrs, attribute.String(attrReason, reason))
	}
	r.decisionCounter.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

// ObserveSpendSettlement records a settled reservation's actual USD.
func (r *Recorder) ObserveSpendSettlement(scope quota.ScopeKind, usd float64) {
	if r == nil {
		return
	}
	r.spendCounter.Add(context.Background(), usd, metric.WithAttributes(
		attribute.String(attrScope, string(scope)),
	))
}

// ObserveConcurrencyAcquire increments the active-slot gauge.
func (r *Recorder) ObserveConcurrencyAcquire(scope quota.ScopeKind) {
	if r == nil {
		return
	}
	r.concurrencyGauge.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String(attrScope, string(scope)),
	))
}

// ObserveConcurrencyRelease decrements the active-slot gauge.
func (r *Recorder) ObserveConcurrencyRelease(scope quota.ScopeKind) {
	if r == nil {
		return
	}
	r.concurrencyGauge.Add(context.Background(), -1, metric.WithAttributes(
		attribute.String(attrScope, string(scope)),
	))
}

// compile-time interface assertion
var _ quota.MetricsRecorder = (*Recorder)(nil)
