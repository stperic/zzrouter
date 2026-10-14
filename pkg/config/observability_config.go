package config

import "github.com/stperic/zzrouter/pkg/observability"

// ObservabilityConfig holds OpenTelemetry observability configuration.
type ObservabilityConfig struct {
	// Enabled controls whether observability is active
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// ServiceName identifies this service in traces and metrics
	ServiceName string `mapstructure:"service_name" yaml:"service_name,omitempty"`

	// OTLP configuration for trace/metric export
	OTLP OTLPConfig `mapstructure:"otlp" yaml:"otlp,omitempty"`

	// Tracing configuration
	Tracing TracingConfig `mapstructure:"tracing" yaml:"tracing,omitempty"`

	// Metrics configuration
	Metrics MetricsConfig `mapstructure:"metrics" yaml:"metrics,omitempty"`
}

// OTLPConfig configures the OTLP exporter endpoint.
type OTLPConfig struct {
	// Endpoint is a gRPC host:port or an absolute HTTP base URL.
	Endpoint string `mapstructure:"endpoint" yaml:"endpoint,omitempty"`

	// Protocol selects grpc (the default) or http/protobuf.
	Protocol string `yaml:"protocol,omitempty" mapstructure:"protocol"`

	// Insecure disables TLS for the OTLP connection
	Insecure bool `mapstructure:"insecure" yaml:"insecure,omitempty"`

	// Headers are additional headers to send with OTLP requests
	Headers map[string]string `mapstructure:"headers" yaml:"headers,omitempty"`
}

// TracingConfig configures distributed tracing.
type TracingConfig struct {
	// Enabled controls whether tracing is active
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// SampleRate is the fraction of traces to sample (0.0-1.0)
	// 1.0 = sample all, 0.1 = sample 10%
	SampleRate float64 `mapstructure:"sample_rate" yaml:"sample_rate,omitempty"`
}

// MetricsConfig configures metrics collection.
type MetricsConfig struct {
	// Enabled controls whether metrics are active
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// OTLPExport controls remote metrics push; omitted means enabled.
	OTLPExport *bool `yaml:"otlp_export,omitempty" mapstructure:"otlp_export"`

	// PrometheusPort is the port for the Prometheus /metrics endpoint
	// 0 = disabled (metrics exported via OTLP only)
	PrometheusPort int `mapstructure:"prometheus_port" yaml:"prometheus_port,omitempty"`

	// ExportInterval is how often to export metrics (in seconds)
	// Default: 15 seconds
	ExportInterval int `mapstructure:"export_interval" yaml:"export_interval,omitempty"`
}

// ToObservabilityConfig converts node settings into the provider configuration.
func (c *ObservabilityConfig) ToObservabilityConfig() *observability.Config {
	return &observability.Config{
		Enabled: c.Enabled, ServiceName: c.ServiceName,
		OTLP:    observability.OTLPConfig{Endpoint: c.OTLP.Endpoint, Protocol: c.OTLP.Protocol, Insecure: c.OTLP.Insecure, Headers: c.OTLP.Headers},
		Tracing: observability.TracingConfig{Enabled: c.Tracing.Enabled, SampleRate: c.Tracing.SampleRate},
		Metrics: observability.MetricsConfig{Enabled: c.Metrics.Enabled, OTLPExport: c.Metrics.OTLPExport, PrometheusPort: c.Metrics.PrometheusPort, ExportInterval: c.Metrics.ExportInterval},
	}
}

// IsEnabled returns true if observability is enabled.
func (c *ObservabilityConfig) IsEnabled() bool {
	return c.Enabled
}

// IsTracingEnabled returns true if tracing is enabled.
func (c *ObservabilityConfig) IsTracingEnabled() bool {
	return c.Enabled && c.Tracing.Enabled
}

// IsMetricsEnabled returns true if metrics are enabled.
func (c *ObservabilityConfig) IsMetricsEnabled() bool {
	return c.Enabled && c.Metrics.Enabled
}

// IsPrometheusEnabled returns true if the Prometheus /metrics handler should
// be built. Tracks metrics.enabled directly — the handler is served on the
// main gin engine, so PrometheusPort is not required. The port field remains
// reserved for a future dedicated sidecar listener.
func (c *ObservabilityConfig) IsPrometheusEnabled() bool {
	return c.IsMetricsEnabled()
}
