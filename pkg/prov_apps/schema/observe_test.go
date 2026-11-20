package schema

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestObserveValidationFailsSchemaMerge(t *testing.T) {
	for _, tc := range []struct{ name, block, reason string }{
		{"unknown role", "{path: /metrics, format: prometheus, queued: q}", "queued"},
		{"unknown key", "{path: /queue, format: json, method: POST}", "method"},
		{"format", "{path: /queue, format: yaml, running: x}", "format"},
		{"missing format", "{path: /queue}", "format"},
		{"busy requires JSON", "{path: /queue, format: prometheus, busy: [q]}", "busy"},
		{"duplicate busy field", "{path: /queue, format: json, busy: [q, q]}", "duplicate"},
		{"empty busy field", "{path: /queue, format: json, busy: ['']}", "empty"},
		{"queue without busy", "{path: /queue, format: json}", "observation role"},
		{"empty busy list", "{path: /queue, format: json, busy: []}", "observation role"},
		{"prometheus without roles", "{path: /metrics, format: prometheus}", "observation role"},
		{"missing path", "{format: json, busy: [q]}", "path"},
		{"absolute URL", "{path: 'http://remote/queue', format: json, busy: [q]}", "path"},
		{"network path", "{path: //remote/queue, format: json, busy: [q]}", "path"},
		{"query", "{path: '/queue?x=1', format: json, busy: [q]}", "path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			y, err := LoadYAMLSchema([]byte("observe: " + tc.block))
			require.NoError(t, err)
			merged, failures := Merge("studio", y, config.ServiceConfig{})
			require.Len(t, failures, 1)
			require.ErrorAs(t, failures[0], new(HardError))
			require.ErrorContains(t, failures[0], "studio.observe")
			require.ErrorContains(t, failures[0], tc.reason)
			require.Nil(t, merged.Observe)
		})
	}
	merged, failures := Merge("undeclared", nil, config.ServiceConfig{})
	require.Empty(t, failures)
	require.Nil(t, merged.Observe)
}

func TestCalibrationOutputMustBeNonEmptyJSON(t *testing.T) {
	for _, block := range []string{"{}", "{output: {}}", "{output: {bad: .nan}}", "{output: {nested: {invalid: .inf}}}"} {
		y, err := LoadYAMLSchema([]byte("calibration: " + block))
		require.NoError(t, err)
		merged, failures := Merge("engine", y, config.ServiceConfig{})
		require.Len(t, failures, 1, block)
		require.ErrorAs(t, failures[0], new(HardError))
		require.ErrorContains(t, failures[0], "engine.calibration.output")
		require.Nil(t, merged.Calibration)
	}
	y, err := LoadYAMLSchema([]byte("calibration: {output: {ignore_eos: true, params: {tokens: [1, 2]}}}"))
	require.NoError(t, err)
	merged, failures := Merge("engine", y, config.ServiceConfig{})
	require.Empty(t, failures)
	require.Equal(t, map[string]any{"ignore_eos": true, "params": map[string]any{"tokens": []any{1, 2}}}, merged.Calibration.Output)
}

func TestObserveRuntimeSelectorsUseConfiguredFeatureRuntimes(t *testing.T) {
	configured := config.ServiceConfig{Features: map[string]config.Feature{"vision": {Runtime: "mlx-vlm"}}}
	for _, tc := range []struct{ selectors, problem string }{
		{"[base, mlx-vlm]", ""},
		{"[unknown-runtime]", "unknown-runtime"},
		{"[mlx-vlm, mlx-vlm]", "duplicate"},
		{"[]", "explicit runtime"},
		{"[vision]", "vision"},
	} {
		t.Run(tc.selectors, func(t *testing.T) {
			declared, err := LoadYAMLSchema([]byte("observe: {path: /metrics, format: json, running: count, runtimes: " + tc.selectors + "}"))
			require.NoError(t, err)
			merged, failures := Merge("mlx", declared, configured)
			if tc.problem == "" {
				require.Empty(t, failures)
				require.Equal(t, []string{"base", "mlx-vlm"}, merged.Observe.Runtimes)
				return
			}
			require.Len(t, failures, 1)
			require.ErrorAs(t, failures[0], new(HardError))
			require.ErrorContains(t, failures[0], tc.problem)
			require.Nil(t, merged.Observe)
		})
	}
}
