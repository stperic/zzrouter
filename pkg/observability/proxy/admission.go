package proxy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/stperic/zzrouter/pkg/observability/genai"
)

// Admission outcomes: how an instance slot request ended. Closed set, so the histogram's
// outcome dimension stays bounded.
const (
	AdmissionAdmitted    = "admitted"
	AdmissionQueueFull   = "queue_full"
	AdmissionWaitExpired = "wait_expired"
	AdmissionCancelled   = "cancelled"
)

// AdmissionMetrics holds the instance admission wait histogram.
//
// `zz.instance.admission.wait.duration.metric` shows how long requests queue for an instance
// slot before they are admitted or refused. A rising tail with outcome=admitted means the
// instance is saturated but still serving; queue_full and wait_expired mean callers were turned
// away, which the duration histogram alone cannot show.
type AdmissionMetrics struct {
	wait metric.Float64Histogram
}

var (
	admissionMu       sync.Mutex
	admissionMetrics  *AdmissionMetrics
	admissionProvider metric.MeterProvider
)

// GetAdmissionMetrics returns the package-level admission metrics, lazy-initialising on the
// current global meter provider. Rebuilds when the provider has changed since the last call.
func GetAdmissionMetrics() *AdmissionMetrics {
	admissionMu.Lock()
	defer admissionMu.Unlock()
	current := otel.GetMeterProvider()
	if admissionMetrics != nil && admissionProvider == current {
		return admissionMetrics
	}
	m, err := newAdmissionMetrics()
	if err != nil {
		slog.Warn("zz.instance.admission.wait.duration.metric init failed",
			"error", err,
			"impact", "metric will not emit; admission wait dashboards will see no data",
		)
		return nil
	}
	admissionMetrics = m
	admissionProvider = current
	return admissionMetrics
}

func newAdmissionMetrics() (*AdmissionMetrics, error) {
	meter := otel.Meter("zzrouter.proxy")

	wait, err := meter.Float64Histogram(
		"zz.instance.admission.wait.duration.metric",
		metric.WithDescription("Time a request waited for an instance slot"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	return &AdmissionMetrics{wait: wait}, nil
}

// RecordAdmissionWait records one slot request's wait and how it ended. Optional labels (model)
// are omitted when empty, matching the inflight counter.
func (m *AdmissionMetrics) RecordAdmissionWait(ctx context.Context, provider, model, outcome string, wait time.Duration) {
	if m == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.provider.name", provider),
		attribute.String("outcome", outcome),
	}
	if gated := genai.GateModelLabel(model); gated != "" {
		attrs = append(attrs, semconv.GenAIRequestModelKey.String(gated))
	}
	m.wait.Record(ctx, wait.Seconds(), metric.WithAttributes(attrs...))
}

// RecordAdmissionWait is the package-level shortcut around GetAdmissionMetrics.
func RecordAdmissionWait(ctx context.Context, provider, model, outcome string, wait time.Duration) {
	GetAdmissionMetrics().RecordAdmissionWait(ctx, provider, model, outcome, wait)
}
