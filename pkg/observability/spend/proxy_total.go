package spend

import (
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/proxy"
)

// totalRequestsMetrics holds the proxy total-requests counter.
//
// zz.proxy.total.requests.metric is the LiteLLM-mirrored counterpart to
// `litellm_proxy_total_requests_metric` — fires on every LLM-route
// HTTP request regardless of outcome, with `status_code`
// discriminating success vs failure. Together with
// zz.proxy.failed.requests.metric (in pkg/observability/proxy) it
// gives operators the LiteLLM dashboard trio: total / failed /
// failure-rate. Failure-rate is computed in PromQL (failed / total).
//
// SCOPE-DIVERGENCE BETWEEN failed AND total: the failed counter
// (Phase 2.3) fires on every 4xx/5xx response across the whole
// engine — admin-route auth failures are valuable telemetry on their
// own. The total counter (this slice) only fires on LLM routes (where
// genai.OperationName resolves a non-empty value). Naively dividing
// `failed / total` can exceed 1.0 if admin-route errors are present.
// To compute LLM-only failure rate, operators must filter both
// counters to the same scope. Both carry `requested_model` (and the
// failed counter additionally carries `gen_ai.operation.name`), so
// either filter narrows the join cleanly:
//
//	sum(zz_proxy_failed_requests_metric_total{requested_model!=""})
//	  /
//	sum(zz_proxy_requests_metric_total{requested_model!=""})
//
// WIRE NAME NORMALIZATION: the OTel Prometheus exporter strips the
// redundant "total" word from the middle of counter names (since
// every Prometheus counter family already gets a `_total` suffix
// appended). Our internal name `zz.proxy.total.requests.metric` ships
// on the wire as `zz_proxy_requests_metric_total` — operators
// migrating LiteLLM dashboards via `s/litellm/zz/` need to also drop
// the leading `total_` segment from references to this single
// counter. We keep the OTel-internal name aligned with LiteLLM's
// vocabulary so future code changes stay grep-able by intent.
//
// EMPTY-LABEL JOIN GOTCHA: model, model_group, and api_provider are
// emitted as empty strings on this counter — the middleware-driven
// design has no access to the resolved model post-dispatch. The
// bridge-driven counters (zz.input.tokens.metric, zz.spend.metric.total)
// DO carry them. So a JOIN query like
// `zz_input_tokens_metric{model="gpt-4o"} / zz_proxy_requests_metric_total{model="gpt-4o"}`
// silently returns 0 because the right-hand side never has a
// populated `model` label. Operators must JOIN on `requested_model`
// instead, which is populated on both counters via stashInferenceContext.
//
// Lives in the spend package (not the proxy package) because the
// label set matches CallerLabels — the LiteLLM "by-key" rollup
// vocabulary — rather than the dual-namespace OTel + LiteLLM tuple
// the failure counter carries. Operators querying "requests by key
// by day" filter on the same label set as the spend / token
// counters; keeping the metrics under one CallerLabels-emitting
// package makes the JOIN obvious.
type totalRequestsMetrics struct {
	totalRequests metric.Int64Counter
}

var (
	totalReqMu       sync.Mutex
	totalReqMetrics  *totalRequestsMetrics
	totalReqProvider metric.MeterProvider
)

// getTotalRequestsMetrics returns the package-level total-requests
// metrics, lazy-initialising on the current global meter provider.
// Mirrors the rebuild-on-provider-change pattern used elsewhere in
// pkg/observability/* so test isolation works.
func getTotalRequestsMetrics() *totalRequestsMetrics {
	totalReqMu.Lock()
	defer totalReqMu.Unlock()
	current := otel.GetMeterProvider()
	if totalReqMetrics != nil && totalReqProvider == current {
		return totalReqMetrics
	}
	m, err := newTotalRequestsMetrics()
	if err != nil {
		// Same as the other metrics in this package — initialisation
		// failure is non-fatal for the request path; the caller falls
		// back to nil-receiver no-op.
		return nil
	}
	totalReqMetrics = m
	totalReqProvider = current
	return totalReqMetrics
}

func newTotalRequestsMetrics() (*totalRequestsMetrics, error) {
	meter := otel.Meter("zzrouter.spend")
	totalRequests, err := meter.Int64Counter(
		"zz.proxy.total.requests.metric",
		metric.WithDescription("Total proxy requests, sliced by virtual key + model + provider + status_code"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, err
	}
	return &totalRequestsMetrics{totalRequests: totalRequests}, nil
}

// recordTotalRequest increments zz.proxy.total.requests.metric. Called
// from the gin middleware below; standalone callers should prefer the
// middleware so labels stay sourced from the same gin keys the rest of
// the LiteLLM-mirrored emit sites use.
func (m *totalRequestsMetrics) recordTotalRequest(c *gin.Context, status int) {
	if m == nil || c == nil {
		return
	}
	labels := callerLabelsFromContext(c)
	attrs := append(labels.attributes(),
		attribute.String("status_code", strconv.Itoa(status)),
	)
	m.totalRequests.Add(c.Request.Context(), 1, metric.WithAttributes(attrs...))
}

// callerLabelsFromContext builds a CallerLabels by reading the same
// gin keys setAccessContext + stashInferenceContext populate for the
// failed-requests middleware. Empty values are valid Prometheus
// labels and required so {team_alias=~".+"} selectors still resolve
// against series produced by anonymous or static-key traffic.
func callerLabelsFromContext(c *gin.Context) CallerLabels {
	if c == nil {
		return CallerLabels{}
	}
	get := func(key string) string {
		v, ok := c.Get(key)
		if !ok {
			return ""
		}
		s, _ := v.(string)
		return s
	}
	return CallerLabels{
		APIKeyAlias:    get(proxy.CtxKeyAPIKeyAlias),
		HashedAPIKey:   get(proxy.CtxKeyHashedAPIKey),
		Team:           get(proxy.CtxKeyTeamID),
		TeamAlias:      get(proxy.CtxKeyTeamAlias),
		RequestedModel: get(proxy.CtxKeyRequestModel),
		// model + model_group + api_provider stay empty on this
		// middleware-driven counter — they're populated post-dispatch
		// from the resolver, which the gin middleware doesn't see.
		// The spend/token counters fired from the inference-log bridge
		// do carry them, so JOIN queries between
		// zz_proxy_total_requests_metric and zz_input_tokens_metric on
		// {api_key_alias, requested_model} resolve correctly even
		// without the resolved-model dimension here.
	}
}

// TotalRequestsMiddleware returns a gin middleware that increments
// zz.proxy.total.requests.metric on every request after c.Next
// completes. Run alongside pkg/observability/proxy.Middleware so the
// LiteLLM dashboard trio (total / failed / failure-rate) shares the
// same emission point.
func TotalRequestsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		// Skip the /metrics endpoint so the scrape itself doesn't
		// drive the counter. Same convention as observability/genai
		// keeps that route off the GenAI duration histograms.
		if c.FullPath() == "/metrics" {
			return
		}
		// Skip OPTIONS preflight (CORS) for the same reason — those
		// are infrastructure noise, not LLM proxy traffic.
		if c.Request != nil && c.Request.Method == "OPTIONS" {
			return
		}
		// Skip the genai-aware ungated routes (health/readiness): if
		// genai.OperationName resolves to "" AND the route isn't a
		// known LLM surface, the request is operational not LLM.
		// Keeping the gate strict here matters because the inflight
		// metric (proxy package) and total-requests metric (this
		// package) need to slice the same denominator for failure-
		// rate panels.
		if genai.OperationName(c.FullPath()) == "" && genai.OperationName(c.Request.URL.Path) == "" {
			return
		}
		getTotalRequestsMetrics().recordTotalRequest(c, c.Writer.Status())
	}
}
