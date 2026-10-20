package schema

import (
	"fmt"
	"maps"
	"math"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestMergePlacementLog(t *testing.T) {
	valid := map[string]string{
		"layers":        `offloaded (\d+)/(\d+) layers to GPU`,
		"cpu_overrides": `tensor overrides to CPU`,
		"weights":       `(CPU)?\S* model buffer size = +([\d.]+) MiB`,
	}
	for _, endpoint := range []bool{false, true} {
		for name, groups := range map[string]int{"layers": 2, "cpu_overrides": 0, "weights": 2} {
			for _, tc := range []struct {
				name, pattern string
				valid         bool
			}{
				{"valid", valid[name], true},
				{"noncapturing group", `(?:prefix)` + valid[name], true},
				{"invalid regex", `(`, false},
				{"one group", `(value)`, false},
				{"three groups", `(a)(b)(c)`, false},
				{"wrong zero or two groups", map[int]string{0: `(a)(b)`, 2: `value`}[groups], false},
			} {
				t.Run(fmt.Sprintf("endpoint=%t/%s/%s", endpoint, name, tc.name), func(t *testing.T) {
					patterns := maps.Clone(valid)
					patterns[name] = tc.pattern
					y := &YAMLSchema{Parameters: map[string]YAMLParam{"n-gpu-layers": {PlacementLog: patterns}}}
					if endpoint {
						y.Parameters["n-gpu-layers"] = YAMLParam{PlacementLog: maps.Clone(valid)}
						y.Endpoints = map[string]YAMLEndpoint{"embeddings": {Parameters: map[string]YAMLParam{"n-gpu-layers": {PlacementLog: patterns}}}}
					}
					data, err := yaml.Marshal(y)
					require.NoError(t, err)
					decoded, err := LoadYAMLSchema(data)
					require.NoError(t, err)
					merged, errs := Merge("llamacpp", decoded, config.ServiceConfig{})
					if !tc.valid {
						require.NotEmpty(t, errs)
						assert.Contains(t, errs[0].Error(), "placement_log."+name)
						assert.Contains(t, errs[0].Error(), fmt.Sprintf("exactly %d capture groups", groups))
						return
					}
					require.Empty(t, errs)
					param := merged.ForEndpoint(map[bool]string{false: "chat", true: "embeddings"}[endpoint])["n-gpu-layers"]
					require.NotNil(t, param.PlacementLog[name])
					assert.Equal(t, tc.pattern, param.PlacementLog[name].String())
					assert.Equal(t, groups, param.PlacementLog[name].NumSubexp())
					wantBase := tc.pattern
					if endpoint {
						wantBase = valid[name]
						assert.Equal(t, wantBase, merged.Parameters["n-gpu-layers"].PlacementLog[name].String())
					}
					assert.Equal(t, wantBase, decoded.Parameters["n-gpu-layers"].PlacementLog[name])
				})
			}
		}
	}
}

func TestMergePlacementLogUnknownPattern(t *testing.T) {
	_, errs := Merge("llamacpp", &YAMLSchema{Parameters: map[string]YAMLParam{
		"n-gpu-layers": {PlacementLog: map[string]string{"buffer": "model buffer size"}},
	}}, config.ServiceConfig{})
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "unknown placement_log.buffer")
}

func TestMergeAutoAndServed(t *testing.T) {
	declared, err := LoadYAMLSchema([]byte(`parameters:
  ctx-size: {auto: "0", served: meta.n_ctx}
endpoints:
  embeddings:
    parameters:
      ctx-size: {auto: "auto", served: max_model_len}
`))
	require.NoError(t, err)
	merged, errs := Merge("llamacpp", declared, config.ServiceConfig{})
	require.Empty(t, errs)
	shape := merged.ForEndpoint("chat")["ctx-size"]
	assert.Equal(t, ParamInt, shape.Kind)
	assert.Equal(t, float64(1), *shape.Min)
	assert.Equal(t, "0", shape.Auto)
	assert.Equal(t, "meta.n_ctx", shape.Served)
	assert.Equal(t, shape, merged.ForEndpoint("embeddings")["ctx-size"], "base policy stays authoritative like pass")
}

func TestMergeMemoryAutoComesFromValidatedBudget(t *testing.T) {
	for _, kind := range []string{MemoryFractionTotal, MemoryReserveMiB, "invalid"} {
		t.Run(kind, func(t *testing.T) {
			declared := &YAMLSchema{
				Parameters: map[string]YAMLParam{"budget": {Type: "float"}},
				Endpoints: map[string]YAMLEndpoint{"embeddings": {Parameters: map[string]YAMLParam{
					"budget": {Description: "Endpoint budget"},
				}}},
				Diagnostics: &Diagnostics{Runtime: "test", Runtimes: map[string]RuntimeChecks{"test": {Kernels: "unknown"}},
					Memory: &MemoryBudget{Kind: kind, Parameter: "budget"}},
			}
			merged, errs := Merge("generic", declared, config.ServiceConfig{})
			if kind == "invalid" {
				require.NotEmpty(t, errs)
				assert.Nil(t, merged.Diagnostics)
			} else {
				require.Empty(t, errs)
			}
			for _, endpoint := range []string{"chat", "embeddings"} {
				shape := merged.ForEndpoint(endpoint)["budget"]
				assert.Equal(t, ParamFloat, shape.Kind)
				if kind == MemoryFractionTotal {
					assert.Equal(t, "auto", shape.Auto)
				} else {
					assert.Empty(t, shape.Auto)
				}
			}
			assert.Empty(t, declared.Parameters["budget"].Auto, "merging must not rewrite the declaration")
		})
	}
}

func TestAutoContextMetadataValidationAndEndpointResolution(t *testing.T) {
	for _, tc := range []struct {
		declaration string
		valid       bool
	}{
		{`max-model-len: {auto: auto, auto_min_context_tokens: 4096}`, true},
		{`max-model-len: {auto: auto, auto_min_context_tokens: -1}`, false},
		{`max-model-len: {auto_min_context_tokens: 4096}`, false},
		{`other: {type: string, auto: auto, auto_min_context_tokens: 4096}`, false},
		{`max-model-len: {auto: auto, auto_context_source: native}`, true},
		{`max-model-len: {auto: auto, auto_context_source: native, auto_min_context_tokens: 4096}`, false},
		{`max-model-len: {auto: auto, auto_context_source: guess}`, false},
	} {
		y, err := LoadYAMLSchema([]byte("parameters:\n  " + tc.declaration + "\n"))
		require.NoError(t, err)
		_, errs := Merge("vllm", y, config.ServiceConfig{})
		assert.Equal(t, tc.valid, len(errs) == 0, tc.declaration)
	}
	y, err := LoadYAMLSchema([]byte(`auto_min_context_tokens: 4096
parameters:
  max-model-len: {auto: auto, auto_min_context_tokens: 4096}
endpoints:
  embeddings:
    parameters:
      max-model-len: {auto_min_context_tokens: 8192}
`))
	require.NoError(t, err)
	merged, errs := Merge("vllm", y, config.ServiceConfig{})
	require.Empty(t, errs)
	assert.Equal(t, int64(4096), merged.AutoMinContextTokens)
	assert.Equal(t, int64(4096), merged.ForEndpoint("chat")["max-model-len"].AutoMinContextTokens)
	assert.Equal(t, int64(8192), merged.ForEndpoint("embeddings")["max-model-len"].AutoMinContextTokens)
}

func TestMemoryUsableFractionValidation(t *testing.T) {
	for _, kind := range []string{MemoryUnified, MemoryLayers, MemoryFractionTotal, ""} {
		for _, fraction := range []float64{0, -0.1, 0.75, 1, 1.1, math.NaN(), math.Inf(1)} {
			y := &YAMLSchema{MemoryUsableFraction: &fraction}
			if kind != "" {
				y.Diagnostics = &Diagnostics{Runtime: "test", Runtimes: map[string]RuntimeChecks{"test": {Kernels: "unknown"}}, Memory: &MemoryBudget{Kind: kind, Parameter: "budget"}}
			}
			merged, errs := Merge("generic", y, config.ServiceConfig{})
			valid := kind == MemoryUnified && fraction > 0 && fraction <= 1
			assert.Equal(t, valid, len(errs) == 0, "kind=%s fraction=%g", kind, fraction)
			if valid {
				assert.Equal(t, fraction, merged.MemoryUsableFraction)
			}
		}
	}
	// Older workers decode the non-strict envelope and retain strict diagnostics.
	data := []byte("auto_min_context_tokens: 4096\nmemory_usable_fraction: 0.75\ndiagnostics:\n  runtime: test\n  runtimes: {test: {kernels: unknown}}\n  memory: {kind: unified}\n")
	var legacy struct {
		Diagnostics *Diagnostics `yaml:"diagnostics"`
	}
	require.NoError(t, yaml.Unmarshal(data, &legacy))
	require.NoError(t, legacy.Diagnostics.Validate())
	declared, err := LoadYAMLSchema(data)
	require.NoError(t, err)
	merged, errs := Merge("generic", declared, config.ServiceConfig{})
	require.Empty(t, errs)
	assert.Equal(t, 0.75, merged.MemoryUsableFraction)
}

func TestMemoryParameterRolesMerge(t *testing.T) {
	for _, role := range []string{"", "k", "v", "both", "invalid"} {
		data := fmt.Appendf(nil, "parameters:\n  cache: {type: string, kv_cache_dtype: %q}\n  override: {type: string, context_override: true}\nendpoints:\n  embeddings:\n    parameters:\n      cache: {kv_cache_dtype: v}\n", role)
		declared, err := LoadYAMLSchema(data)
		require.NoError(t, err)
		merged, errs := Merge("generic", declared, config.ServiceConfig{})
		if role == "invalid" {
			require.NotEmpty(t, errs)
			continue
		}
		require.Empty(t, errs)
		assert.Equal(t, role, merged.ForEndpoint("chat")["cache"].KVCacheDtype)
		assert.Equal(t, "v", merged.ForEndpoint("embeddings")["cache"].KVCacheDtype)
		assert.True(t, merged.ForEndpoint("embeddings")["override"].ContextOverride)
	}
}

func TestRecurrentRowsValidationAndEndpointResolution(t *testing.T) {
	for _, rows := range []int64{-1, 0, 1, 4, MaxRecurrentStateRows, MaxRecurrentStateRows + 1} {
		for _, parameter := range []bool{false, true} {
			y := &YAMLSchema{AutoContext: AutoContext{RecurrentStateRows: &rows}}
			if parameter {
				y = &YAMLSchema{Parameters: map[string]YAMLParam{"slots": {Type: "int", SequenceSlots: true, AutoContext: AutoContext{RecurrentStateRows: &rows}}}}
			}
			_, errs := Merge("generic", y, config.ServiceConfig{})
			assert.Equal(t, rows >= 1 && rows <= MaxRecurrentStateRows, len(errs) == 0)
		}
	}
	for _, declaration := range []string{
		`slots: {type: string, sequence_slots: true, recurrent_state_rows: 4}`,
		`slots: {type: int, sequence_slots: true}`,
		`slots: {type: int, recurrent_state_rows: 4}`,
	} {
		y, err := LoadYAMLSchema([]byte("parameters:\n  " + declaration + "\n"))
		require.NoError(t, err)
		_, errs := Merge("generic", y, config.ServiceConfig{})
		require.NotEmpty(t, errs, declaration)
	}
	y, err := LoadYAMLSchema([]byte(`recurrent_state_rows: 2
parameters:
  slots: {type: int, sequence_slots: true, recurrent_state_rows: 4}
endpoints:
  embeddings:
    parameters:
      slots: {recurrent_state_rows: 1}
`))
	require.NoError(t, err)
	merged, errs := Merge("generic", y, config.ServiceConfig{})
	require.Empty(t, errs)
	assert.Equal(t, int64(2), *merged.RecurrentStateRows)
	assert.Equal(t, int64(4), *merged.ForEndpoint("chat")["slots"].RecurrentStateRows)
	assert.Equal(t, int64(1), *merged.ForEndpoint("embeddings")["slots"].RecurrentStateRows)
	assert.True(t, merged.ForEndpoint("embeddings")["slots"].SequenceSlots)
}

func TestLegacySchemaIgnoresRecurrentMetadata(t *testing.T) {
	data := []byte(`recurrent_state_rows: 2
parameters:
  slots: {type: int, sequence_slots: true, recurrent_state_rows: 4}
diagnostics:
  runtime: test
  runtimes: {test: {kernels: unknown}}
  memory: {kind: layers}
`)
	var legacy struct {
		Parameters map[string]struct {
			Type string `yaml:"type"`
		} `yaml:"parameters"`
		Diagnostics *Diagnostics `yaml:"diagnostics"`
	}
	require.NoError(t, yaml.Unmarshal(data, &legacy))
	require.NoError(t, legacy.Diagnostics.Validate())
	assert.Equal(t, "int", legacy.Parameters["slots"].Type)
	y, err := LoadYAMLSchema(data)
	require.NoError(t, err)
	merged, errs := Merge("generic", y, config.ServiceConfig{})
	require.Empty(t, errs)
	assert.Equal(t, int64(2), *merged.RecurrentStateRows)
}
