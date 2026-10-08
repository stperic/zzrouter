// Package spend implements the LiteLLM-mirrored spend, budget, and
// token-consumption metrics under the dashboard-parity priority order:
// since OpenTelemetry GenAI semconv has no canonical instruments for
// these concepts, the metrics are namespaced under zz.* and use
// LiteLLM's exact label vocabulary so a `s/litellm/zz/` rename of an
// existing LiteLLM dashboard resolves the same dimensions zzrouter
// emits without operator intervention.
//
// Token counters (zz.input.tokens.metric, zz.output.tokens.metric)
// run alongside OTel's gen_ai.client.token.usage histogram from
// pkg/observability/llm — neither is redundant: the histogram answers
// "what's the p95 prompt size", the counters answer "tokens by key by
// day" without the histogram's bucketing tax.
package spend

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stperic/zzrouter/pkg/observability/genai"
)

// Metrics holds the LiteLLM-mirrored spend instruments.
type Metrics struct {
	inputTokens  metric.Int64Counter
	outputTokens metric.Int64Counter
	spendUSD     metric.Float64Counter
}

var (
	mu             sync.Mutex
	globalMetrics  *Metrics
	globalProvider metric.MeterProvider
)

// GetMetrics returns the package-level Metrics, lazy-initialising on
// the current global meter provider. Mirrors the rebuild-on-provider-
// change pattern used in pkg/observability/llm + pkg/observability/proxy
// so test isolation works the same way.
func GetMetrics() *Metrics {
	mu.Lock()
	defer mu.Unlock()
	current := otel.GetMeterProvider()
	if globalMetrics != nil && globalProvider == current {
		return globalMetrics
	}
	m, err := newMetrics()
	if err != nil {
		slog.Warn("zz spend metrics init failed",
			"error", err,
			"impact", "zz.input.tokens.metric and zz.output.tokens.metric will not emit",
		)
		return nil
	}
	globalMetrics = m
	globalProvider = current
	return globalMetrics
}

func newMetrics() (*Metrics, error) {
	meter := otel.Meter("zzrouter.spend")

	inputTokens, err := meter.Int64Counter(
		"zz.input.tokens.metric",
		metric.WithDescription("Total prompt tokens consumed, sliced by virtual key + model + provider"),
		metric.WithUnit("{token}"),
	)
	if err != nil {
		return nil, err
	}

	outputTokens, err := meter.Int64Counter(
		"zz.output.tokens.metric",
		metric.WithDescription("Total completion tokens produced, sliced by virtual key + model + provider"),
		metric.WithUnit("{token}"),
	)
	if err != nil {
		return nil, err
	}

	// Unit intentionally omitted on the spend counter: the metric name
	// already carries `usd`. With WithUnit("USD") the OTel Prometheus
	// exporter would append `_USD` and produce a doubly-suffixed
	// `zz_spend_metric_total_USD_total` series. Same pattern as
	// pkg/observability/quotametrics's spend counter.
	spendUSD, err := meter.Float64Counter(
		"zz.spend.metric.total",
		metric.WithDescription("Total resolved spend in USD, sliced by virtual key + team + model + provider"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		inputTokens:  inputTokens,
		outputTokens: outputTokens,
		spendUSD:     spendUSD,
	}, nil
}

// CallerLabels carries the per-request label tuple shared across every
// LiteLLM-mirrored spend instrument. Using a value type keeps the
// recorder API non-positional — drift between Inc and Dec sites for
// counters with a dozen labels is the kind of bug that only shows up
// in dashboards, not in tests, so we centralise the construction.
//
// Caller-controlled fields (RequestedModel, Model, ModelGroup) are
// filtered through genai.GateModelLabel by RecordTokens before landing
// on a series — adversarial clients can't spray arbitrary values to
// explode Prometheus storage.
type CallerLabels struct {
	APIKeyAlias    string // LiteLLM `api_key_alias`
	HashedAPIKey   string // LiteLLM `hashed_api_key`
	Team           string // LiteLLM `team` (team ID)
	TeamAlias      string // LiteLLM `team_alias`
	User           string // LiteLLM `user` (empty until users ship)
	EndUser        string // LiteLLM `end_user` (empty until end-users ship)
	Model          string // LiteLLM `model` (post-group-resolution deployment)
	ModelGroup     string // LiteLLM `model_group`
	RequestedModel string // LiteLLM `requested_model` (pre-group-resolution)
	APIProvider    string // LiteLLM `api_provider` (canonical OTel provider name)
}

// attributes builds the LiteLLM-vocabulary attribute set, with model
// labels gated to honor SetModelAllowlist. Empty labels (user, end_user,
// team for keyless requests) remain valid Prometheus values.
func (l CallerLabels) attributes() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("api_key_alias", l.APIKeyAlias),
		attribute.String("hashed_api_key", l.HashedAPIKey),
		attribute.String("team", l.Team),
		attribute.String("team_alias", l.TeamAlias),
		attribute.String("user", l.User),
		attribute.String("end_user", l.EndUser),
		attribute.String("model", genai.GateModelLabel(l.Model)),
		attribute.String("model_group", genai.GateModelLabel(l.ModelGroup)),
		attribute.String("requested_model", genai.GateModelLabel(l.RequestedModel)),
		attribute.String("api_provider", l.APIProvider),
	}
}

// RecordTokens increments zz.input.tokens.metric and zz.output.tokens.metric
// by the supplied counts. Either count <= 0 omits its instrument so
// embedding routes (no completion tokens) don't emit zero-valued output
// series and pollute distributions.
func (m *Metrics) RecordTokens(ctx context.Context, labels CallerLabels, tokensIn, tokensOut int64) {
	if m == nil {
		return
	}
	if tokensIn <= 0 && tokensOut <= 0 {
		return
	}
	attrs := metric.WithAttributes(labels.attributes()...)
	if tokensIn > 0 {
		m.inputTokens.Add(ctx, tokensIn, attrs)
	}
	if tokensOut > 0 {
		m.outputTokens.Add(ctx, tokensOut, attrs)
	}
}

// RecordTokens is the package-level shortcut around GetMetrics. Returns
// silently when initialisation has failed; the warn from GetMetrics
// surfaces the dark-metric condition once per provider rebind.
func RecordTokens(ctx context.Context, labels CallerLabels, tokensIn, tokensOut int64) {
	GetMetrics().RecordTokens(ctx, labels, tokensIn, tokensOut)
}

// RecordSpendUSD increments zz.spend.metric.total by costUSD. Zero-cost
// completions still emit the series with a zero increment so dashboards
// see "no data" only when the recorder + bridge weren't wired, not when
// pricing was simply unresolved. Negative values are treated as zero —
// upstream-reported credit refunds aren't mirrored on this counter.
func (m *Metrics) RecordSpendUSD(ctx context.Context, labels CallerLabels, costUSD float64) {
	if m == nil {
		return
	}
	if costUSD < 0 {
		costUSD = 0
	}
	m.spendUSD.Add(ctx, costUSD, metric.WithAttributes(labels.attributes()...))
}

// RecordSpendUSD is the package-level shortcut around GetMetrics.
func RecordSpendUSD(ctx context.Context, labels CallerLabels, costUSD float64) {
	GetMetrics().RecordSpendUSD(ctx, labels, costUSD)
}
