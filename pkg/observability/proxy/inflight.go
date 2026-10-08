package proxy

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/stperic/zzrouter/pkg/observability/genai"
)

// InflightMetrics holds the in-flight request counter.
//
// `zz.proxy.inflight.requests.metric` answers the operator's first
// question on a latency spike — "is the backend wedged, are requests
// piling up?" — before SLO alerts trip on the duration histogram.
// Saturation alerts (inflight > N for > M minutes) are the cheapest
// way to catch a wedged backend; LiteLLM doesn't ship this metric, so
// zzrouter invents it but uses OTel-canonical labels (gen_ai.* +
// server.*) to slice the same way the GenAI duration histograms do.
type InflightMetrics struct {
	inflight metric.Int64UpDownCounter
}

var (
	inflightMu       sync.Mutex
	inflightMetrics  *InflightMetrics
	inflightProvider metric.MeterProvider
)

// GetInflightMetrics returns the package-level inflight metrics, lazy-
// initialising on the current global meter provider. Safe to call
// repeatedly; rebuilds when the provider has changed since the last
// call so test isolation works the same way pkg/observability/llm does.
func GetInflightMetrics() *InflightMetrics {
	inflightMu.Lock()
	defer inflightMu.Unlock()
	current := otel.GetMeterProvider()
	if inflightMetrics != nil && inflightProvider == current {
		return inflightMetrics
	}
	m, err := newInflightMetrics()
	if err != nil {
		slog.Warn("zz.proxy.inflight.requests.metric init failed",
			"error", err,
			"impact", "metric will not emit; saturation dashboards will see no data",
		)
		return nil
	}
	inflightMetrics = m
	inflightProvider = current
	return inflightMetrics
}

func newInflightMetrics() (*InflightMetrics, error) {
	meter := otel.Meter("zzrouter.proxy")

	inflight, err := meter.Int64UpDownCounter(
		"zz.proxy.inflight.requests.metric",
		metric.WithDescription("Currently in-flight proxy requests, sliced by backend"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, err
	}
	return &InflightMetrics{inflight: inflight}, nil
}

// InflightToken is the symmetric label-bearer for an Inc/Dec pair.
// IncInflight captures the labels at request entry; DecInflight reuses
// them so the UpDownCounter records the +1/-1 pair under the same
// attribute set. Mismatched labels would leave a ghost +1 series and
// a separate ghost -1 series rather than netting to zero.
type InflightToken struct {
	attrs []attribute.KeyValue
}

// IncInflight increments the in-flight counter with the supplied
// labels and returns a token the caller MUST pass to DecInflight on
// the matching defer. Optional labels (requestModel, serverAddress)
// are OMITTED when empty — emitting empty-string dimensions would
// inflate Prometheus storage with no operator-visible value.
// gen_ai.operation.name and gen_ai.provider.name always emit (even
// when empty) so the metric carries at least one stable identifier
// for dashboards to filter on.
//
// REALTIME CARVE-OUT: WebSocket paths (routes_realtime.go) are
// long-lived bidirectional sessions, not request/response. They
// don't drive this counter; realtime saturation will live in
// `zz.realtime.sessions.active.metric` once that instrument lands.
//
// CARDINALITY: operation is bounded by the closed enum from
// genai.OperationName. provider is bounded by the registered
// providers list. requestModel is caller-controlled (same risk as
// the failure counter); operators concerned about explosion should
// apply Prometheus relabel rules. serverAddress is bounded by the
// cluster peer count + cloud endpoint count.
func (m *InflightMetrics) IncInflight(ctx context.Context, operation, provider, requestModel, serverAddress string) InflightToken {
	if m == nil {
		return InflightToken{}
	}
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", operation),
		attribute.String("gen_ai.provider.name", provider),
	}
	if gated := genai.GateModelLabel(requestModel); gated != "" {
		attrs = append(attrs, semconv.GenAIRequestModelKey.String(gated))
	}
	if serverAddress != "" {
		attrs = append(attrs, semconv.ServerAddressKey.String(serverAddress))
	}
	m.inflight.Add(ctx, 1, metric.WithAttributes(attrs...))
	return InflightToken{attrs: attrs}
}

// DecInflight decrements the in-flight counter with the labels the
// matching IncInflight captured. Safe no-op for the zero-value token
// (returned when m was nil at Inc time).
func (m *InflightMetrics) DecInflight(ctx context.Context, t InflightToken) {
	if m == nil || len(t.attrs) == 0 {
		return
	}
	m.inflight.Add(ctx, -1, metric.WithAttributes(t.attrs...))
}

// IncInflight is the package-level shortcut around GetInflightMetrics.
// Returns the zero-value token when metrics aren't initialised so the
// matching defer call is a no-op.
func IncInflight(ctx context.Context, operation, provider, requestModel, serverAddress string) InflightToken {
	return GetInflightMetrics().IncInflight(ctx, operation, provider, requestModel, serverAddress)
}

// DecInflight is the package-level shortcut around GetInflightMetrics.
func DecInflight(ctx context.Context, t InflightToken) {
	GetInflightMetrics().DecInflight(ctx, t)
}
