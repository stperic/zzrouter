package observability

import (
	"testing"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
	}{
		{
			name:    "disabled config is valid",
			config:  &Config{Enabled: false},
			wantErr: false,
		},
		{
			name: "enabled config with empty service name gets default",
			config: &Config{
				Enabled:     true,
				ServiceName: "",
				OTLP: OTLPConfig{
					Endpoint: "localhost:4317",
				},
			},
			wantErr: false,
		},
		{
			name: "sample rate clamped to 0",
			config: &Config{
				Enabled: true,
				Tracing: TracingConfig{
					SampleRate: -1.0,
				},
			},
			wantErr: false,
		},
		{
			name: "sample rate clamped to 1",
			config: &Config{
				Enabled: true,
				Tracing: TracingConfig{
					SampleRate: 2.0,
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Check that sample rate was clamped
			if tt.config.Enabled && tt.config.Tracing.SampleRate < 0 {
				t.Error("Sample rate should be clamped to >= 0")
			}
			if tt.config.Enabled && tt.config.Tracing.SampleRate > 1 {
				t.Error("Sample rate should be clamped to <= 1")
			}
		})
	}
}

func TestIsTracingEnabled(t *testing.T) {
	tests := []struct {
		name     string
		config   *Config
		expected bool
	}{
		{
			name:     "disabled observability",
			config:   &Config{Enabled: false, Tracing: TracingConfig{Enabled: true}},
			expected: false,
		},
		{
			name:     "enabled observability, disabled tracing",
			config:   &Config{Enabled: true, Tracing: TracingConfig{Enabled: false}},
			expected: false,
		},
		{
			name:     "enabled both",
			config:   &Config{Enabled: true, Tracing: TracingConfig{Enabled: true}},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.IsTracingEnabled(); got != tt.expected {
				t.Errorf("IsTracingEnabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestIsMetricsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		config   *Config
		expected bool
	}{
		{
			name:     "disabled observability",
			config:   &Config{Enabled: false, Metrics: MetricsConfig{Enabled: true}},
			expected: false,
		},
		{
			name:     "enabled observability, disabled metrics",
			config:   &Config{Enabled: true, Metrics: MetricsConfig{Enabled: false}},
			expected: false,
		},
		{
			name:     "enabled both",
			config:   &Config{Enabled: true, Metrics: MetricsConfig{Enabled: true}},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.IsMetricsEnabled(); got != tt.expected {
				t.Errorf("IsMetricsEnabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestIsPrometheusEnabled(t *testing.T) {
	tests := []struct {
		name     string
		config   *Config
		expected bool
	}{
		{
			name:     "disabled metrics",
			config:   &Config{Enabled: true, Metrics: MetricsConfig{Enabled: false, PrometheusPort: 9090}},
			expected: false,
		},
		{
			// PrometheusPort is no longer a gate — the in-process /metrics handler
			// is served on the main gin engine whenever metrics.enabled is true.
			// The port field is reserved for a future dedicated sidecar listener.
			name:     "enabled metrics, no prometheus port",
			config:   &Config{Enabled: true, Metrics: MetricsConfig{Enabled: true, PrometheusPort: 0}},
			expected: true,
		},
		{
			name:     "enabled with prometheus port",
			config:   &Config{Enabled: true, Metrics: MetricsConfig{Enabled: true, PrometheusPort: 9090}},
			expected: true,
		},
		{
			name:     "observability disabled",
			config:   &Config{Enabled: false, Metrics: MetricsConfig{Enabled: true, PrometheusPort: 9090}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.IsPrometheusEnabled(); got != tt.expected {
				t.Errorf("IsPrometheusEnabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}
