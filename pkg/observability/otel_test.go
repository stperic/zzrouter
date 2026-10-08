package observability

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
)

func TestNewProvider_Disabled(t *testing.T) {
	ctx := context.Background()

	// Test with nil config
	provider, err := NewProvider(ctx, nil, "1.0.0")
	require.NoError(t, err)
	assert.NotNil(t, provider)
	assert.False(t, provider.IsEnabled())
	assert.Nil(t, provider.TracerProvider())
	assert.Nil(t, provider.MeterProvider())
	assert.Nil(t, provider.PrometheusHandler())

	// Test with disabled config
	cfg := &Config{Enabled: false}
	provider, err = NewProvider(ctx, cfg, "1.0.0")
	require.NoError(t, err)
	assert.NotNil(t, provider)
	assert.False(t, provider.IsEnabled())
}

func TestNewProvider_TracingOnly(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Enabled:     true,
		ServiceName: "test-service",
		OTLP: OTLPConfig{
			Endpoint: "", // No endpoint = no OTLP export
			Insecure: true,
		},
		Tracing: TracingConfig{
			Enabled:    true,
			SampleRate: 1.0,
		},
		Metrics: MetricsConfig{
			Enabled: false,
		},
	}

	provider, err := NewProvider(ctx, cfg, "1.0.0")
	require.NoError(t, err)
	require.NotNil(t, provider)
	assert.True(t, provider.IsEnabled())

	// Tracing should be initialized
	assert.NotNil(t, provider.TracerProvider())

	// Metrics should not be initialized since it's disabled
	// Note: MeterProvider may still be nil when metrics disabled
	assert.Nil(t, provider.PrometheusHandler())

	// Cleanup
	err = provider.Stop(ctx)
	assert.NoError(t, err)
}

func TestNewProvider_MetricsWithPrometheus(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Enabled:     true,
		ServiceName: "test-service",
		OTLP: OTLPConfig{
			Endpoint: "", // No OTLP endpoint
			Insecure: true,
		},
		Tracing: TracingConfig{
			Enabled: false,
		},
		Metrics: MetricsConfig{
			Enabled:        true,
			PrometheusPort: 9090,
			ExportInterval: 1,
		},
	}

	provider, err := NewProvider(ctx, cfg, "1.0.0")
	require.NoError(t, err)
	require.NotNil(t, provider)
	assert.True(t, provider.IsEnabled())

	// Prometheus handler should be available
	assert.NotNil(t, provider.PrometheusHandler())
	assert.NotNil(t, provider.MeterProvider())

	// Cleanup
	err = provider.Stop(ctx)
	assert.NoError(t, err)
}

func TestNewProvider_FullConfig(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Enabled:     true,
		ServiceName: "test-service",
		OTLP: OTLPConfig{
			Endpoint: "", // Empty endpoint - won't try to connect
			Insecure: true,
			Headers:  map[string]string{"X-Custom": "value"},
		},
		Tracing: TracingConfig{
			Enabled:    true,
			SampleRate: 0.5,
		},
		Metrics: MetricsConfig{
			Enabled:        true,
			PrometheusPort: 9091,
			ExportInterval: 5,
		},
	}

	provider, err := NewProvider(ctx, cfg, "2.0.0")
	require.NoError(t, err)
	require.NotNil(t, provider)

	assert.True(t, provider.IsEnabled())
	assert.NotNil(t, provider.TracerProvider())
	assert.NotNil(t, provider.MeterProvider())
	assert.NotNil(t, provider.PrometheusHandler())

	// Cleanup
	err = provider.Stop(ctx)
	assert.NoError(t, err)
}

func TestProvider_Tracer(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Enabled:     true,
		ServiceName: "test-service",
		Tracing: TracingConfig{
			Enabled:    true,
			SampleRate: 1.0,
		},
		Metrics: MetricsConfig{
			Enabled: false,
		},
	}

	provider, err := NewProvider(ctx, cfg, "1.0.0")
	require.NoError(t, err)
	defer func() { _ = provider.Stop(ctx) }()

	// Get a tracer
	tracer := provider.Tracer("test-component")
	assert.NotNil(t, tracer)

	// Create a span
	_, span := tracer.Start(ctx, "test-span")
	assert.NotNil(t, span)
	assert.True(t, span.SpanContext().IsValid())
	span.End()
}

func TestProvider_SamplingRates(t *testing.T) {
	tests := []struct {
		name       string
		sampleRate float64
	}{
		{"always_sample", 1.0},
		{"never_sample", 0.0},
		{"half_sample", 0.5},
		{"ten_percent", 0.1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			cfg := &Config{
				Enabled:     true,
				ServiceName: "test-service",
				Tracing: TracingConfig{
					Enabled:    true,
					SampleRate: tt.sampleRate,
				},
				Metrics: MetricsConfig{
					Enabled: false,
				},
			}

			provider, err := NewProvider(ctx, cfg, "1.0.0")
			require.NoError(t, err)
			require.NotNil(t, provider)

			// Create a tracer and span
			tracer := provider.Tracer("test")
			_, span := tracer.Start(ctx, "test-span")
			span.End()

			// Cleanup
			err = provider.Stop(ctx)
			assert.NoError(t, err)
		})
	}
}

func TestProvider_Stop(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Enabled:     true,
		ServiceName: "test-service",
		Tracing: TracingConfig{
			Enabled:    true,
			SampleRate: 1.0,
		},
		Metrics: MetricsConfig{
			Enabled:        true,
			PrometheusPort: 9092,
		},
	}

	provider, err := NewProvider(ctx, cfg, "1.0.0")
	require.NoError(t, err)

	// Shutdown with timeout
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err = provider.Stop(shutdownCtx)
	assert.NoError(t, err)

	// Shutdown again should be safe (idempotent)
	_ = provider.Stop(shutdownCtx)
	// May or may not error depending on internal state, but shouldn't panic
}

func TestProvider_StopNilProvider(t *testing.T) {
	ctx := context.Background()

	// Create disabled provider
	provider, err := NewProvider(ctx, nil, "1.0.0")
	require.NoError(t, err)

	// Shutdown should work even with nil internal providers
	err = provider.Stop(ctx)
	assert.NoError(t, err)
}

func TestGlobalTracerProvider(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Enabled:     true,
		ServiceName: "global-test-service",
		Tracing: TracingConfig{
			Enabled:    true,
			SampleRate: 1.0,
		},
		Metrics: MetricsConfig{
			Enabled: false,
		},
	}

	provider, err := NewProvider(ctx, cfg, "1.0.0")
	require.NoError(t, err)
	defer func() { _ = provider.Stop(ctx) }()

	// Global tracer should work
	globalTracer := otel.Tracer("global-test")
	assert.NotNil(t, globalTracer)

	_, span := globalTracer.Start(ctx, "global-span")
	assert.NotNil(t, span)
	span.End()
}

func TestGlobalMeterProvider(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Enabled:     true,
		ServiceName: "global-meter-test",
		Tracing: TracingConfig{
			Enabled: false,
		},
		Metrics: MetricsConfig{
			Enabled:        true,
			PrometheusPort: 9093,
		},
	}

	provider, err := NewProvider(ctx, cfg, "1.0.0")
	require.NoError(t, err)
	defer func() { _ = provider.Stop(ctx) }()

	// Global meter should work
	globalMeter := otel.Meter("global-test")
	assert.NotNil(t, globalMeter)

	// Create a counter
	counter, err := globalMeter.Int64Counter("test_counter")
	require.NoError(t, err)
	counter.Add(ctx, 1)
}
