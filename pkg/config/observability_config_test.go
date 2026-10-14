package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestObservabilityConfigExportRoundTrip(t *testing.T) {
	for _, protocol := range []string{"grpc", "http/protobuf"} {
		for _, toggle := range []*bool{nil, ptrBool(false), ptrBool(true)} {
			t.Run(protocol+toggleName(toggle), func(t *testing.T) {
				original := ObservabilityConfig{Enabled: true, ServiceName: "router-test", OTLP: OTLPConfig{Endpoint: "http://collector/prefix", Protocol: protocol, Headers: map[string]string{"test": "synthetic"}}, Tracing: TracingConfig{Enabled: true, SampleRate: 0.25}, Metrics: MetricsConfig{Enabled: true, OTLPExport: toggle, ExportInterval: 7}}
				body, err := yaml.Marshal(original)
				require.NoError(t, err)
				var decoded ObservabilityConfig
				require.NoError(t, yaml.Unmarshal(body, &decoded))
				assert.Equal(t, original, decoded)
				converted := decoded.ToObservabilityConfig()
				assert.Equal(t, protocol, converted.OTLP.Protocol)
				assert.Equal(t, toggle, converted.Metrics.OTLPExport)
				assert.Equal(t, original.OTLP.Headers, converted.OTLP.Headers)
				assert.Equal(t, 0.25, converted.Tracing.SampleRate)
				assert.Equal(t, toggle == nil || *toggle, converted.Metrics.IsOTLPEnabled())
			})
		}
	}
}

func ptrBool(value bool) *bool { return &value }
func toggleName(value *bool) string {
	if value == nil {
		return " default"
	}
	if *value {
		return " on"
	}
	return " off"
}
