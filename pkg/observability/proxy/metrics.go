// Package proxy provides observability instruments for zzrouter's proxy
// surface — failure counters, deployment-state gauges, and other
// instruments that observe the request layer above the GenAI semconv
// metrics in pkg/observability/llm.
//
// Naming follows the dashboard-parity priority order: OpenTelemetry
// canonical when available, otherwise LiteLLM-mirrored under the zz.*
// prefix (s/litellm/zz/) for drop-in dashboard portability.
package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/stperic/zzrouter/pkg/observability/genai"
)

// requestContextKey is the typed key for proxy-metric context-stash
// entries written via context.WithValue. Bare http.Handlers (the
// `writeError` call sites) use this surface; gin handlers
// can use the ergonomic c.Set form on the same string keys below.
type requestContextKey string

// Context-stash keys handlers/middleware can use to surface label
// values to the failed-request metric. Empty string is treated as
// "unknown" by the recorder; the labels still emit on the series so
// dashboards can split populated vs. unpopulated.
//
// Gin handlers use c.Set on these string keys directly; bare
// http.Handlers should use the SetXxx helpers below to stash on
// req.Context() since gin's keymap is not visible there.
//
// CARDINALITY NOTE: api_key_alias / hashed_api_key / team / team_alias
// are bounded by the active virtual-key + team count. Admins can mint
// keys without a soft cap (POST /keys is admin-gated; cap is operator
// behavior, not adversarial input), so deployments expecting hundreds
// of keys should rate-limit POST /keys at the LB or apply Prometheus
// `metric_relabel_configs` to drop low-volume keys.
// requested_model and gen_ai.request.model are caller-controlled —
// untrusted clients can mint arbitrary values up to the OpenAI
// dialect's request-body length limit. Operators concerned about
// label explosion should run the metric through a Prometheus relabel
// rule that drops series whose model label is not on an allowlist.
// A built-in allowlist is filed as a follow-up.
// hashed_api_key on the auth-rejection path is gated to keys > 12
// characters (see access_control.go::stashRejectedKeyFingerprint) so
// short-junk floods can't explode the dimension.
const (
	// CtxKeyAPIKeyAlias carries the human-readable virtual-key name
	// the user assigned (e.g. "Production API key"). LiteLLM-vocabulary
	// label `api_key_alias`.
	CtxKeyAPIKeyAlias = "zzrouter.metrics.api_key_alias" //nolint:gosec // This constant names a Gin context slot for metrics, not an API credential.

	// CtxKeyHashedAPIKey carries a stable identifier for the virtual
	// key — zzrouter uses the safe-to-log first4..last4 fingerprint
	// rather than the raw secret. LiteLLM-vocabulary label
	// `hashed_api_key`.
	CtxKeyHashedAPIKey = "zzrouter.metrics.hashed_api_key" //nolint:gosec // This constant names a Gin context slot for metrics, not an API credential.

	// CtxKeyTeamAlias carries the team name the calling virtual key
	// belongs to. Empty for keys with no team affiliation.
	// LiteLLM-vocabulary label `team_alias`.
	CtxKeyTeamAlias = "zzrouter.metrics.team_alias"

	// CtxKeyTeamID carries the stable team identifier. LiteLLM-vocabulary
	// label `team`. Empty for keys with no team.
	CtxKeyTeamID = "zzrouter.metrics.team_id"

	// CtxKeyRequestModel carries the model name from the request body.
	// Distinct from gen_ai.response.model (set post-dispatch).
	CtxKeyRequestModel = "zzrouter.metrics.request_model"

	// CtxKeyErrorType lets a dialect responder override the
	// status-derived error.type with a more specific closed-enum
	// value (e.g. "model_not_found" rather than the generic
	// "invalid_request_error" 404 default).
	CtxKeyErrorType = "zzrouter.metrics.error_type"

	// CtxKeyErrorCode lets the responder surface the OpenAI-style
	// closed-enum sub-code (e.g. "rate_limit_exceeded",
	// "context_length_exceeded") on the metric.
	CtxKeyErrorCode = "zzrouter.metrics.error_code"
)

// reqCtxErrorTypeKey is the typed context key for the error.type
// override stash. Bare http.Handlers route through SetErrorTypeOnRequest
// so the value lands at req.Context().Value(reqCtxErrorTypeKey) and the
// middleware reads it the same way it reads c.Get values.
var reqCtxErrorTypeKey = requestContextKey(CtxKeyErrorType)

// UnknownModelLabel re-exports the sentinel from pkg/observability/genai
// so existing proxy callers don't have to switch imports. The canonical
// definition (and the SetModelAllowlist setter) lives in genai because
// the gate also needs to apply at every llm-package emission site.
const UnknownModelLabel = genai.UnknownModelLabel

// SetModelAllowlist forwards to genai.SetModelAllowlist. Kept on the
// proxy surface for callers that already imported this package; the
// canonical setter lives in genai.
func SetModelAllowlist(checker func(string) bool) {
	genai.SetModelAllowlist(checker)
}

// SetErrorTypeOnRequest returns a copy of req whose context carries the
// proxy-metric error.type override. Use from bare http.Handler call
// sites (e.g. writeError users) where the gin
// keymap isn't reachable. Gin handlers should call c.Set(CtxKeyErrorType,
// ...) directly — both surfaces resolve to the same series label.
func SetErrorTypeOnRequest(req *http.Request, errType string) *http.Request {
	if req == nil || errType == "" {
		return req
	}
	ctx := context.WithValue(req.Context(), reqCtxErrorTypeKey, errType)
	return req.WithContext(ctx)
}

// Metrics holds the proxy-surface instruments.
type Metrics struct {
	// failedRequests mirrors LiteLLM's litellm_proxy_failed_requests_metric
	// counter, increment on every 4xx/5xx response. Dual-namespace labels
	// (OTel-canonical + LiteLLM-mirrored) so the same series satisfies
	// both an OTel-aware dashboard and a `s/litellm/zz/`-translated
	// LiteLLM dashboard without operator intervention.
	failedRequests metric.Int64Counter
}

var (
	mu             sync.Mutex
	globalMetrics  *Metrics
	globalProvider metric.MeterProvider
)

// GetMetrics returns the package-level Metrics, lazy-initialising on
// the current global meter provider. Safe to call repeatedly; rebuilds
// when the provider has changed since the last call so test isolation
// works the same way pkg/observability/llm does.
func GetMetrics() *Metrics {
	mu.Lock()
	defer mu.Unlock()
	current := otel.GetMeterProvider()
	if globalMetrics != nil && globalProvider == current {
		return globalMetrics
	}
	m, err := newMetrics()
	if err != nil {
		// Initialisation failure is non-fatal for the request path —
		// callers fall back to the nil-receiver no-op surface — but
		// the operator deserves a visible signal that the failure
		// counter is dark. Logged once per provider rebind because
		// the call is gated on `globalProvider == current`.
		slog.Warn("zz.proxy.failed.requests.metric init failed",
			"error", err,
			"impact", "metric will not emit; dashboards filtering on this series will see no data",
		)
		return nil
	}
	globalMetrics = m
	globalProvider = current
	return globalMetrics
}

func newMetrics() (*Metrics, error) {
	meter := otel.Meter("zzrouter.proxy")

	failedRequests, err := meter.Int64Counter(
		"zz.proxy.failed.requests.metric",
		metric.WithDescription("Total number of failed proxy requests (4xx/5xx)"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{failedRequests: failedRequests}, nil
}

// RecordFailedRequest increments zz.proxy.failed.requests.metric with
// the dual-namespace labels resolved from the gin.Context plus the
// HTTP status. The dialect responder MAY have already stashed a more
// specific errType / errCode via CtxKeyErrorType / CtxKeyErrorCode;
// otherwise both are derived from the HTTP status.
func (m *Metrics) RecordFailedRequest(c *gin.Context, status int) {
	if m == nil || c == nil {
		return
	}

	statusStr := strconv.Itoa(status)
	apiKeyAlias := stringFromCtx(c, CtxKeyAPIKeyAlias)
	hashedAPIKey := stringFromCtx(c, CtxKeyHashedAPIKey)
	teamAlias := stringFromCtx(c, CtxKeyTeamAlias)
	teamID := stringFromCtx(c, CtxKeyTeamID)
	requestModel := genai.GateModelLabel(stringFromCtx(c, CtxKeyRequestModel))

	// Prefer dialect-supplied error.type, fall back to status-derived
	// closed-enum so legacy error helpers that bypass the responder
	// still produce a usable label. Bare http.Handler callsites stash
	// via SetErrorTypeOnRequest (req.Context value); gin handlers via
	// c.Set on the same string key. Both surfaces resolve here.
	errType := stringFromCtx(c, CtxKeyErrorType)
	if errType == "" {
		if v, ok := c.Request.Context().Value(reqCtxErrorTypeKey).(string); ok {
			errType = v
		}
	}
	if errType == "" {
		errType = errorTypeFromStatus(status)
	}
	errCode := stringFromCtx(c, CtxKeyErrorCode)

	operation := genai.OperationName(c.FullPath())
	if operation == "" {
		operation = genai.OperationName(c.Request.URL.Path)
	}

	// OTel-canonical labels — the canonical view for OTel-aware
	// dashboards (Honeycomb, Grafana Cloud, Datadog, Phoenix, ...).
	otelAttrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", string(operation)),
		semconv.ErrorTypeKey.String(errType),
		semconv.HTTPResponseStatusCodeKey.Int(status),
	}
	if requestModel != "" {
		otelAttrs = append(otelAttrs, semconv.GenAIRequestModelKey.String(requestModel))
	}

	// LiteLLM-mirrored labels — kept under the same vocabulary the
	// LiteLLM dashboards expect, so an `s/litellm/zz/` rename of an
	// existing dashboard JSON resolves the labels by name. Empty
	// labels for concepts zzrouter doesn't yet surface (user, end_user)
	// remain valid Prometheus values.
	//
	// DELIBERATE DUPLICATION (do not "fix"): {team, team_alias},
	// {status_code, exception_status}, and {error.type, exception_class}
	// each carry the same value. The duplication is the dashboard-parity
	// tax — operators get to switch between the OTel-canonical view and
	// the LiteLLM-vocabulary view without touching the producer side.
	// Worth roughly 3x storage for the duplicated dimensions; that
	// trade-off is what the parity-plan docs prescribe.
	litellmAttrs := []attribute.KeyValue{
		attribute.String("api_key_alias", apiKeyAlias),
		attribute.String("hashed_api_key", hashedAPIKey),
		attribute.String("team", teamID),
		attribute.String("team_alias", teamAlias),
		attribute.String("user", ""),
		attribute.String("end_user", ""),
		attribute.String("requested_model", requestModel),
		attribute.String("status_code", statusStr),
		attribute.String("exception_class", errType),
		attribute.String("exception_status", statusStr),
		attribute.String("error_code", errCode),
	}

	attrs := append(otelAttrs, litellmAttrs...)
	m.failedRequests.Add(c.Request.Context(), 1, metric.WithAttributes(attrs...))
}

// errorTypeFromStatus maps an HTTP status code to the OpenAI closed-enum
// error.type value the openai dialect uses for the same status. Keeping
// this mapping local to the metrics layer means the proxy middleware
// can label every 4xx/5xx response — including ones from legacy error
// helpers that bypass the dialect responder — without importing the
// protocol package.
//
// Stays in sync with pkg/protocol/openai/responder.go's status->ErrorType
// choices. Any divergence here corrupts dashboards that filter by
// error.type, so keep the mappings 1:1.
func errorTypeFromStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusBadGateway, status == http.StatusGatewayTimeout:
		return "api_error"
	case status >= 500:
		return "server_error"
	case status >= 400:
		return "invalid_request_error"
	}
	return ""
}

// Middleware returns a gin middleware that records
// zz.proxy.failed.requests.metric for every 4xx/5xx response. Run the
// middleware on every request surface that should contribute to the
// failure counter (typically /v1/*, /api/*, /zzrouter/v1/*).
//
// Pre-c.Next handlers may stash a richer error.type via CtxKeyErrorType;
// the middleware reads that with status-derived fallback so legacy
// error helpers still get a usable label.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if c.Writer.Status() >= 400 {
			GetMetrics().RecordFailedRequest(c, c.Writer.Status())
		}
	}
}

// stringFromCtx fetches a string-valued gin context entry, returning
// the zero value when the key is missing or the entry is non-string.
func stringFromCtx(c *gin.Context, key string) string {
	if c == nil {
		return ""
	}
	v, ok := c.Get(key)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
