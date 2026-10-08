// Package observability provides OpenTelemetry-based observability for zzRouter.
// It includes distributed tracing, metrics, and log correlation.
package observability

// Config holds the complete observability configuration.
type Config struct {
	// Enabled controls whether observability is active
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`

	// ServiceName identifies this service in traces and metrics
	ServiceName string `yaml:"service_name" mapstructure:"service_name"`

	// OTLP configuration for trace/metric export
	OTLP OTLPConfig `yaml:"otlp,omitempty" mapstructure:"otlp"`

	// Tracing configuration
	Tracing TracingConfig `yaml:"tracing,omitempty" mapstructure:"tracing"`

	// Metrics configuration
	Metrics MetricsConfig `yaml:"metrics,omitempty" mapstructure:"metrics"`
}

// OTLPConfig configures the OTLP exporter endpoint.
type OTLPConfig struct {
	// Endpoint is the OTLP collector endpoint (e.g., "localhost:4317")
	Endpoint string `yaml:"endpoint" mapstructure:"endpoint"`

	// Insecure disables TLS for the OTLP connection
	Insecure bool `yaml:"insecure" mapstructure:"insecure"`

	// Headers are additional headers to send with OTLP requests
	Headers map[string]string `yaml:"headers,omitempty" mapstructure:"headers"`
}

// TracingConfig configures distributed tracing.
type TracingConfig struct {
	// Enabled controls whether tracing is active
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`

	// SampleRate is the fraction of traces to sample (0.0-1.0)
	// 1.0 = sample all, 0.1 = sample 10%
	SampleRate float64 `yaml:"sample_rate" mapstructure:"sample_rate"`
}

// MetricsConfig configures metrics collection.
type MetricsConfig struct {
	// Enabled controls whether metrics are active
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`

	// PrometheusPort is the port for the Prometheus /metrics endpoint
	// 0 = disabled (metrics exported via OTLP only)
	PrometheusPort int `yaml:"prometheus_port" mapstructure:"prometheus_port"`

	// ExportInterval is how often to export metrics (in seconds)
	// Default: 15 seconds
	ExportInterval int `yaml:"export_interval" mapstructure:"export_interval"`
}

// Validate validates the configuration and returns an error if invalid.
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil // No validation needed if disabled
	}

	if c.ServiceName == "" {
		c.ServiceName = "zzrouter"
	}

	if c.Tracing.SampleRate < 0 {
		c.Tracing.SampleRate = 0
	}
	if c.Tracing.SampleRate > 1 {
		c.Tracing.SampleRate = 1
	}

	if c.Metrics.ExportInterval <= 0 {
		c.Metrics.ExportInterval = 15
	}

	return nil
}

// IsTracingEnabled returns true if tracing is enabled.
func (c *Config) IsTracingEnabled() bool {
	return c.Enabled && c.Tracing.Enabled
}

// IsMetricsEnabled returns true if metrics are enabled.
func (c *Config) IsMetricsEnabled() bool {
	return c.Enabled && c.Metrics.Enabled
}

// IsPrometheusEnabled returns true if the Prometheus /metrics handler should
// be built. This now tracks metrics.enabled directly — the handler is served
// on the main gin engine, so PrometheusPort is not required. The port field
// is reserved for a future dedicated sidecar listener.
func (c *Config) IsPrometheusEnabled() bool {
	return c.IsMetricsEnabled()
}
