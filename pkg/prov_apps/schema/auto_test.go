package schema

import (
	"fmt"
	"maps"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
					merged, errs := Merge("llamacpp", decoded)
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
	}})
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
	merged, errs := Merge("llamacpp", declared)
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
			merged, errs := Merge("generic", declared)
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
		_, errs := Merge("vllm", y)
		assert.Equal(t, tc.valid, len(errs) == 0, tc.declaration)
	}
	y, err := LoadYAMLSchema([]byte(`auto_min_context_tokens: 4096
memory_fraction_environment: MLX_MEMORY_FRACTION
parameters:
  max-model-len: {auto: auto, auto_min_context_tokens: 4096}
endpoints:
  embeddings:
    parameters:
      max-model-len: {auto_min_context_tokens: 8192}
`))
	require.NoError(t, err)
	merged, errs := Merge("vllm", y)
	require.Empty(t, errs)
	assert.Equal(t, int64(4096), merged.AutoMinContextTokens)
	assert.Equal(t, "MLX_MEMORY_FRACTION", merged.MemoryFractionEnvironment)
	assert.Equal(t, int64(4096), merged.ForEndpoint("chat")["max-model-len"].AutoMinContextTokens)
	assert.Equal(t, int64(8192), merged.ForEndpoint("embeddings")["max-model-len"].AutoMinContextTokens)
}
