package instance

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

// TestAcquire_RecordsAdmissionOutcomes drives Acquire through every outcome and checks the
// admission wait histogram gets one data point per call, labelled with the outcome.
func TestAcquire_RecordsAdmissionOutcomes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prior := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prior)
		_ = provider.Shutdown(context.Background())
	})

	// One slot, no queue: the second caller is refused at the door.
	inst := NewInstance("id", "vllm", "llama2:7b", 0, 0, 0)
	inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 0, QueueTimeout: time.Second})

	// admitted
	first, err := inst.Acquire(context.Background(), nil)
	require.NoError(t, err)
	defer first.Release()

	// queue_full: the slot is held and no waiter is allowed.
	_, err = inst.Acquire(context.Background(), nil)
	require.ErrorIs(t, err, ErrQueueFull)

	// wait_expired: a waiter is allowed but the slot never frees within the timeout.
	waiting := NewInstance("id2", "vllm", "llama2:7b", 0, 0, 0)
	waiting.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 1, QueueTimeout: 20 * time.Millisecond})
	held, err := waiting.Acquire(context.Background(), nil)
	require.NoError(t, err)
	defer held.Release()
	_, err = waiting.Acquire(context.Background(), nil)
	require.ErrorIs(t, err, ErrQueueWaitExpired)

	// cancelled: the caller's context is already gone.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = waiting.Acquire(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)

	outcomes := admissionOutcomesFrom(t, reader)
	assert.Equal(t, map[string]uint64{
		"admitted":     2,
		"queue_full":   1,
		"wait_expired": 1,
		"cancelled":    1,
	}, outcomes)
}

// admissionOutcomesFrom returns the sample count per outcome and checks the provider and model labels.
func admissionOutcomesFrom(t *testing.T, reader *sdkmetric.ManualReader) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	counts := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "zz.instance.admission.wait.duration.metric" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			for _, dp := range hist.DataPoints {
				outcome, _ := dp.Attributes.Value(attribute.Key("outcome"))
				provider, _ := dp.Attributes.Value(attribute.Key("gen_ai.provider.name"))
				model, _ := dp.Attributes.Value(attribute.Key("gen_ai.request.model"))
				assert.Equal(t, "vllm", provider.AsString())
				assert.Equal(t, "llama2:7b", model.AsString())
				counts[outcome.AsString()] += dp.Count
			}
		}
	}
	return counts
}
