package schema

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const diagnosticSchema = `provider: arbitrary-engine
parameters:
  device-budget: {type: float}
diagnostics:
  runtime: arbitrary-runtime
  runtimes:
    arbitrary-runtime:
      imports: [torch, package.server]
      packages: [torch, package-name]
      checks: [pip_check, imports, cuda_companions]
      kernels: prebuilt_or_jit
      toolkit_minimum: {"12": "12.9"}
      workaround: {SAMPLER_MODE: "0"}
  memory: {kind: fraction_total, parameter: device-budget}
`

func TestDiagnosticsRejectUnsafeDeclarations(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"empty imports", "imports: [torch, package.server]", "imports: []"},
		{"duplicate check", "cuda_companions", "imports"},
		{"import source", "package.server", "package;evil"},
		{"package URL", "package-name", "https://example.com/pkg"},
		{"executable check", "cuda_companions", "shell"},
		{"unknown field", "kernels: prebuilt_or_jit", "kernels: prebuilt_or_jit\n      command: arbitrary"},
		{"environment key", "SAMPLER_MODE", "invalid.key"},
		{"memory knob", "parameter: device-budget", "parameter: --foo;bar"},
		{"negative auto margin", "kind: fraction_total", "kind: fraction_total, auto_safety_margin_mib: -1"},
		{"unknown mapping", "kind: fraction_total", "kind: guess"},
		{"unknown runtime", "runtime: arbitrary-runtime", "runtime: missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declared, err := LoadYAMLSchema([]byte(strings.Replace(diagnosticSchema, tc.old, tc.replacement, 1)))
			if err == nil {
				_, errs := Merge("arbitrary-engine", declared)
				require.NotEmpty(t, errs)
			}
		})
	}
}

func TestDiagnosticsSurviveSchemaAndWireRoundTrips(t *testing.T) {
	declared, err := LoadYAMLSchema([]byte(diagnosticSchema))
	require.NoError(t, err)
	merged, errs := Merge("arbitrary-engine", declared)
	require.Empty(t, errs)
	require.NotNil(t, merged.Diagnostics)
	assert.Equal(t, "device-budget", merged.Diagnostics.Memory.Parameter)
	data, err := json.Marshal(merged.Diagnostics)
	require.NoError(t, err)
	var wire Diagnostics
	require.NoError(t, json.Unmarshal(data, &wire))
	assert.Equal(t, *merged.Diagnostics, wire)
	saved, err := yaml.Marshal(declared)
	require.NoError(t, err)
	reloaded, err := LoadYAMLSchema(saved)
	require.NoError(t, err)
	assert.Equal(t, declared.Diagnostics, reloaded.Diagnostics)
}

func TestProtocol5SchemaDecoderIgnoresDiagnosticMetadata(t *testing.T) {
	// This is the pre-diagnostics decoder shape and its unchanged non-strict call.
	var legacy struct {
		Provider   string                  `yaml:"provider"`
		Parameters map[string]YAMLParam    `yaml:"parameters,omitempty"`
		Endpoints  map[string]YAMLEndpoint `yaml:"endpoints,omitempty"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(diagnosticSchema), &legacy))
	assert.Equal(t, "arbitrary-engine", legacy.Provider)
	assert.Contains(t, legacy.Parameters, "device-budget")
}

func TestAutoMarginSchemaRoundTripAndDefaultCompatibility(t *testing.T) {
	for _, margin := range []int64{0, 8192} {
		declared, err := LoadYAMLSchema([]byte(diagnosticSchema))
		require.NoError(t, err)
		declared.Diagnostics.Memory.AutoSafetyMarginMiB = margin
		data, err := yaml.Marshal(declared)
		require.NoError(t, err)
		reloaded, err := LoadYAMLSchema(data)
		require.NoError(t, err)
		assert.Equal(t, margin, reloaded.Diagnostics.Memory.AutoSafetyMarginMiB)
		wire, err := json.Marshal(reloaded.Diagnostics)
		require.NoError(t, err)
		var decoded Diagnostics
		require.NoError(t, json.Unmarshal(wire, &decoded))
		assert.Equal(t, margin, decoded.Memory.AutoSafetyMarginMiB)
		if margin == 0 {
			assert.NotContains(t, string(data), "auto_safety_margin_mib")
			// The previous reader strictly decodes diagnostics and these memory fields.
			var legacy struct {
				Runtime  string                   `yaml:"runtime"`
				Runtimes map[string]RuntimeChecks `yaml:"runtimes"`
				Memory   struct {
					Kind                 string `yaml:"kind"`
					Parameter            string `yaml:"parameter,omitempty"`
					DeviceCountParameter string `yaml:"device_count_parameter,omitempty"`
				} `yaml:"memory"`
			}
			metadata, err := yaml.Marshal(declared.Diagnostics)
			require.NoError(t, err)
			reader := yaml.NewDecoder(bytes.NewReader(metadata))
			reader.KnownFields(true)
			require.NoError(t, reader.Decode(&legacy))
		}
	}
}
