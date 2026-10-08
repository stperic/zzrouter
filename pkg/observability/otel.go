package observability

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Provider holds the OpenTelemetry providers and shutdown functions.
type Provider struct {
	config          *Config
	tracerProvider  *sdktrace.TracerProvider
	meterProvider   *metric.MeterProvider
	promRegistry    *promclient.Registry
	promHTTPHandler http.Handler
}

// NewProvider initializes OpenTelemetry with the given configuration.
// It returns a Provider that must be shut down when the application exits.
func NewProvider(ctx context.Context, cfg *Config, version string) (*Provider, error) {
	if cfg == nil || !cfg.Enabled {
		return &Provider{config: cfg}, nil
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid observability config: %w", err)
	}

	p := &Provider{config: cfg}

	// Get hostname for service instance ID
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}

	// Schemaless: these keys are stable, and a pinned schema URL refuses to
	// merge with the SDK's default as soon as the SDK moves to a newer one.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(hostname),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Initialize tracing
	if cfg.IsTracingEnabled() {
		if err := p.initTracing(ctx, cfg, res); err != nil {
			return nil, fmt.Errorf("failed to initialize tracing: %w", err)
		}
	}

	// Initialize metrics
	if cfg.IsMetricsEnabled() {
		if err := p.initMetrics(ctx, cfg, res); err != nil {
			// Clean up tracing if metrics init fails
			if p.tracerProvider != nil {
				_ = p.tracerProvider.Shutdown(ctx)
			}
			return nil, fmt.Errorf("failed to initialize metrics: %w", err)
		}
	}

	// Set up W3C Trace Context propagation
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return p, nil
}

// initTracing sets up the tracing provider with OTLP exporter.
func (p *Provider) initTracing(ctx context.Context, cfg *Config, res *resource.Resource) error {
	// Configure sampler based on sample rate
	var sampler sdktrace.Sampler
	switch cfg.Tracing.SampleRate {
	case 1.0:
		sampler = sdktrace.AlwaysSample()
	case 0.0:
		sampler = sdktrace.NeverSample()
	default:
		sampler = sdktrace.TraceIDRatioBased(cfg.Tracing.SampleRate)
	}

	// Build tracer provider options
	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}

	// Only create OTLP exporter if endpoint is configured
	// Without an endpoint, we still have a functional TracerProvider for local tracing
	if cfg.OTLP.Endpoint != "" {
		var opts []otlptracegrpc.Option
		opts = append(opts, otlptracegrpc.WithEndpoint(cfg.OTLP.Endpoint))

		if cfg.OTLP.Insecure {
			opts = append(opts, otlptracegrpc.WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
			opts = append(opts, otlptracegrpc.WithInsecure())
		}

		// Add custom headers if configured
		if len(cfg.OTLP.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(cfg.OTLP.Headers))
		}

		exporter, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return fmt.Errorf("failed to create OTLP trace exporter: %w", err)
		}

		tpOpts = append(tpOpts, sdktrace.WithBatcher(exporter))
	}

	// Create tracer provider
	p.tracerProvider = sdktrace.NewTracerProvider(tpOpts...)

	// Set global tracer provider
	otel.SetTracerProvider(p.tracerProvider)

	return nil
}

// initMetrics sets up the metrics provider with OTLP and optional Prometheus exporters.
func (p *Provider) initMetrics(ctx context.Context, cfg *Config, res *resource.Resource) error {
	var readers []metric.Reader

	// OTLP metric exporter (if endpoint configured)
	if cfg.OTLP.Endpoint != "" {
		var opts []otlpmetricgrpc.Option
		opts = append(opts, otlpmetricgrpc.WithEndpoint(cfg.OTLP.Endpoint))

		if cfg.OTLP.Insecure {
			opts = append(opts, otlpmetricgrpc.WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}

		if len(cfg.OTLP.Headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.OTLP.Headers))
		}

		exporter, err := otlpmetricgrpc.New(ctx, opts...)
		if err != nil {
			return fmt.Errorf("failed to create OTLP metric exporter: %w", err)
		}

		interval := time.Duration(cfg.Metrics.ExportInterval) * time.Second
		readers = append(readers, metric.NewPeriodicReader(exporter, metric.WithInterval(interval)))
	}

	// Prometheus exporter — always build it when metrics are enabled so the
	// main gin server can serve /metrics. The PrometheusPort config field is
	// reserved for a future dedicated sidecar listener; the zero-value no
	// longer gates the in-process handler.
	if cfg.IsMetricsEnabled() {
		// Create a new Prometheus registry for OTel metrics
		p.promRegistry = promclient.NewRegistry()

		promExporter, err := prometheus.New(
			prometheus.WithRegisterer(p.promRegistry),
		)
		if err != nil {
			return fmt.Errorf("failed to create Prometheus exporter: %w", err)
		}
		readers = append(readers, promExporter)

		// Create HTTP handler for the /metrics endpoint
		p.promHTTPHandler = promhttp.HandlerFor(p.promRegistry, promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		})
	}

	// Create meter provider with all readers
	opts := []metric.Option{metric.WithResource(res)}
	for _, reader := range readers {
		opts = append(opts, metric.WithReader(reader))
	}

	p.meterProvider = metric.NewMeterProvider(opts...)

	// Set global meter provider
	otel.SetMeterProvider(p.meterProvider)

	return nil
}

// Start is a no-op — OTel providers are fully initialized in NewProvider.
// Present for Start/Stop symmetry with the server lifecycle orchestrator.
func (p *Provider) Start(_ context.Context) {}

// Stop gracefully shuts down all providers and nils them out so
// subsequent calls are idempotent (playbook §5.3: stop-safe).
//
// Returns aggregated errors rather than logging-and-swallowing
// (playbook §5.2 exception): OTel shutdown failure means buffered
// spans/metrics are lost — a real programmatic failure the caller
// should know about, not just a missed graceful-drain deadline.
func (p *Provider) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error

	if p.tracerProvider != nil {
		if err := p.tracerProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("tracer provider shutdown: %w", err))
		}
		p.tracerProvider = nil
	}

	if p.meterProvider != nil {
		if err := p.meterProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("meter provider shutdown: %w", err))
		}
		p.meterProvider = nil
	}

	return errors.Join(errs...)
}

// Tracer returns a named tracer for the given component.
func (p *Provider) Tracer(name string) trace.Tracer {
	if p.tracerProvider == nil {
		return otel.Tracer(name)
	}
	return p.tracerProvider.Tracer(name)
}

// Meter returns a named meter for the given component.
func (p *Provider) Meter(name string) otelmetric.Meter {
	if p.meterProvider == nil {
		return otel.Meter(name)
	}
	return p.meterProvider.Meter(name)
}

// PrometheusHandler returns the Prometheus HTTP handler for /metrics endpoint.
// Returns nil if Prometheus is not configured.
func (p *Provider) PrometheusHandler() http.Handler {
	return p.promHTTPHandler
}

// IsEnabled returns true if observability is enabled.
func (p *Provider) IsEnabled() bool {
	return p.config != nil && p.config.Enabled
}

// TracerProvider returns the underlying tracer provider.
func (p *Provider) TracerProvider() *sdktrace.TracerProvider {
	return p.tracerProvider
}

// MeterProvider returns the underlying meter provider.
func (p *Provider) MeterProvider() *metric.MeterProvider {
	return p.meterProvider
}
