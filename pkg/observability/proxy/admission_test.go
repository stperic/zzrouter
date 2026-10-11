package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// withManualMeter routes the global meter provider to a ManualReader for one test.
func withManualMeter(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prior := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prior)
		_ = provider.Shutdown(context.Background())
	})
	return reader
}

// admissionPoints collects the admission wait histogram's data points, keyed by outcome.
func admissionPoints(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	points := map[string]metricdata.HistogramDataPoint[float64]{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "zz.instance.admission.wait.duration.metric" {
				continue
			}
			assert.Equal(t, "s", m.Unit)
			assert.Equal(t, "Time a request waited for an instance slot", m.Description)
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "admission wait must be a float64 histogram")
			for _, dp := range hist.DataPoints {
				outcome, _ := dp.Attributes.Value(attribute.Key("outcome"))
				points[outcome.AsString()] = dp
			}
		}
	}
	return points
}

func TestAdmissionWait_RecordsEachOutcome(t *testing.T) {
	reader := withManualMeter(t)

	RecordAdmissionWait(context.Background(), "vllm", "llama2:7b", AdmissionAdmitted, 10*time.Millisecond)
	RecordAdmissionWait(context.Background(), "vllm", "llama2:7b", AdmissionQueueFull, 0)
	RecordAdmissionWait(context.Background(), "vllm", "llama2:7b", AdmissionWaitExpired, 2*time.Second)
	RecordAdmissionWait(context.Background(), "vllm", "llama2:7b", AdmissionCancelled, 5*time.Millisecond)

	points := admissionPoints(t, reader)
	require.Len(t, points, 4)
	for _, outcome := range []string{AdmissionAdmitted, AdmissionQueueFull, AdmissionWaitExpired, AdmissionCancelled} {
		dp, ok := points[outcome]
		require.True(t, ok, "missing outcome %q", outcome)
		assert.Equal(t, uint64(1), dp.Count)
		provider, _ := dp.Attributes.Value(attribute.Key("gen_ai.provider.name"))
		assert.Equal(t, "vllm", provider.AsString())
		model, _ := dp.Attributes.Value(attribute.Key("gen_ai.request.model"))
		assert.Equal(t, "llama2:7b", model.AsString())
	}
	assert.InDelta(t, 2.0, points[AdmissionWaitExpired].Sum, 1e-9, "wait is recorded in seconds")
}

func TestAdmissionWait_OmitsEmptyModel(t *testing.T) {
	reader := withManualMeter(t)

	RecordAdmissionWait(context.Background(), "vllm", "", AdmissionAdmitted, time.Millisecond)

	dp := admissionPoints(t, reader)[AdmissionAdmitted]
	_, has := dp.Attributes.Value(attribute.Key("gen_ai.request.model"))
	assert.False(t, has, "empty model must not emit a dimension")
}

func TestAdmissionWait_NilSafe(t *testing.T) {
	var m *AdmissionMetrics
	m.RecordAdmissionWait(context.Background(), "vllm", "m", AdmissionAdmitted, time.Millisecond)
}
