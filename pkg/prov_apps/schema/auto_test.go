package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
