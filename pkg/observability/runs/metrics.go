// Package runs implements the LiteLLM-mirrored deployment lifecycle
// counters: zz.deployment.{starts,stops,failures}.metric. Together
// they answer "why is vLLM restarting every five minutes?" — currently
// a log-grep exercise. LiteLLM doesn't ship these because LiteLLM
// doesn't manage backend lifecycles; zzrouter does.
//
// Naming: invented under zz.* with LiteLLM's litellm_deployment_*
// pattern but OTel-canonical labels (gen_ai.* + server.*) so the
// existing GenAI dashboards can JOIN by provider/model/address.
package runs

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

// StopReason classifies why an instance went from running to stopped.
// The closed-enum is bounded by the emit sites that actually fire it;
// values without an emit path are intentionally absent so dashboards
// querying `{reason="X"}` either resolve series or fail loudly,
// rather than silently match a closed-enum value the producer side
// never emits.
type StopReason string

const (
	StopReasonUser  StopReason = "user"  // explicit StopInstance call
	StopReasonCrash StopReason = "crash" // process exited unexpectedly
)

// FailureCause classifies why a launch or running instance failed.
// Bounded enum so cardinality stays predictable; raw error text is
// available in the audit log for forensic detail. Same emit-driven
// pruning as StopReason: only values that have a wire emit site
// today are exported.
type FailureCause string

const (
	FailureReadiness FailureCause = "readiness_fail" // readiness probe failed
	FailureLaunchErr FailureCause = "launch_err"     // LaunchInstance returned error
	FailureOther     FailureCause = "other"          // catch-all for emit sites without a typed Cause
)

// Metrics holds the three deployment-lifecycle counters.
type Metrics struct {
	starts   metric.Int64Counter
	stops    metric.Int64Counter
	failures metric.Int64Counter
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
		slog.Warn("zz.deployment.* lifecycle metrics init failed",
			"error", err,
			"impact", "deployment-lifecycle counters will not emit; instance-restart dashboards will see no data",
		)
		return nil
	}
	globalMetrics = m
	globalProvider = current
	return globalMetrics
}

func newMetrics() (*Metrics, error) {
	meter := otel.Meter("zzrouter.runs")

	starts, err := meter.Int64Counter(
		"zz.deployment.starts.metric",
		metric.WithDescription("Successful deployment starts (instance entered running state)"),
		metric.WithUnit("{deployment}"),
	)
	if err != nil {
		return nil, err
	}
	stops, err := meter.Int64Counter(
		"zz.deployment.stops.metric",
		metric.WithDescription("Deployment stops, sliced by reason"),
		metric.WithUnit("{deployment}"),
	)
	if err != nil {
		return nil, err
	}
	failures, err := meter.Int64Counter(
		"zz.deployment.failures.metric",
		metric.WithDescription("Deployment failures (launch error, crash, readiness fail), sliced by cause"),
		metric.WithUnit("{deployment}"),
	)
	if err != nil {
		return nil, err
	}
	return &Metrics{starts: starts, stops: stops, failures: failures}, nil
}

func baseAttrs(provider, model, serverAddress string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.provider.name", string(genai.ProviderName(provider))),
	}
	if model != "" {
		attrs = append(attrs, semconv.GenAIRequestModelKey.String(model))
	}
	if serverAddress != "" {
		attrs = append(attrs, semconv.ServerAddressKey.String(serverAddress))
	}
	return attrs
}

// RecordStart increments zz.deployment.starts.metric for one successful
// instance launch. Called when an instance enters the running state.
func (m *Metrics) RecordStart(ctx context.Context, provider, model, serverAddress string) {
	if m == nil {
		return
	}
	m.starts.Add(ctx, 1, metric.WithAttributes(baseAttrs(provider, model, serverAddress)...))
}

// RecordStop increments zz.deployment.stops.metric tagged with reason.
// Reason MUST be one of the StopReason closed-enum values; arbitrary
// strings would explode the dimension and defeat dashboard slicing.
func (m *Metrics) RecordStop(ctx context.Context, provider, model, serverAddress string, reason StopReason) {
	if m == nil {
		return
	}
	attrs := append(baseAttrs(provider, model, serverAddress),
		attribute.String("reason", string(reason)),
	)
	m.stops.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordFailure increments zz.deployment.failures.metric tagged with
// cause. The cause closed-enum keeps cardinality bounded; raw error
// text belongs in the audit log, not the metric label set.
func (m *Metrics) RecordFailure(ctx context.Context, provider, model, serverAddress string, cause FailureCause) {
	if m == nil {
		return
	}
	attrs := append(baseAttrs(provider, model, serverAddress),
		attribute.String("cause", string(cause)),
	)
	m.failures.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// Package-level shortcuts mirror the proxy / spend / routing packages.
func RecordStart(ctx context.Context, provider, model, serverAddress string) {
	GetMetrics().RecordStart(ctx, provider, model, serverAddress)
}

func RecordStop(ctx context.Context, provider, model, serverAddress string, reason StopReason) {
	GetMetrics().RecordStop(ctx, provider, model, serverAddress, reason)
}

func RecordFailure(ctx context.Context, provider, model, serverAddress string, cause FailureCause) {
	GetMetrics().RecordFailure(ctx, provider, model, serverAddress, cause)
}
