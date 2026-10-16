package process

import (
	"maps"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSizingUnknownCausesCoverExistingFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]string)
		policy schema.AutoContext
		cause  UnknownCause
	}{
		{"missing native context", func(c map[string]any, p map[string]string) { p["max-model-len"] = "auto" }, rowPolicy(1), ContextUnresolved},
		{"below minimum", func(c map[string]any, p map[string]string) {
			c["max_position_embeddings"] = 2048
			p["max-model-len"] = "auto"
		}, rowPolicy(1), ContextBelowMinimum},
		{"rope scaling", func(c map[string]any, p map[string]string) {
			c["max_position_embeddings"] = 8192
			c["rope_scaling"] = map[string]any{"factor": 2}
			p["max-model-len"] = "auto"
		}, rowPolicy(1), ContextUnresolved},
		{"KV dtype", func(c map[string]any, p map[string]string) { p["cache"] = "mystery" }, rowPolicy(1), KVDtypeUnsupported},
		{"KV block", func(c map[string]any, p map[string]string) { c["head_dim"] = 127; p["cache"] = "q8_0" }, rowPolicy(1), KVDtypeUnsupported},
		{"context override", func(c map[string]any, p map[string]string) {
			c["max_position_embeddings"] = 8192
			p["max-model-len"] = "auto"
			p["override"] = "yes"
		}, rowPolicy(1), ContextUnresolved},
		{"invalid heads", func(c map[string]any, p map[string]string) { c["num_key_value_heads"] = 3 }, rowPolicy(1), MetadataInvalid},
		{"invalid dimension", func(c map[string]any, p map[string]string) { c["head_dim"] = nil }, rowPolicy(1), MetadataInvalid},
		{"model dtype", func(c map[string]any, p map[string]string) { c["torch_dtype"] = "float64" }, rowPolicy(1), ModelUnmodelled},
		{"MLA", func(c map[string]any, p map[string]string) { c["kv_lora_rank"] = 512 }, rowPolicy(1), ModelUnmodelled},
		{"unknown window", func(c map[string]any, p map[string]string) { c["sliding_window"] = 512 }, rowPolicy(1), ModelUnmodelled},
		{"unknown layer", func(c map[string]any, p map[string]string) {
			c["num_hidden_layers"] = 1
			c["layer_types"] = []string{"other"}
		}, rowPolicy(1), ModelUnmodelled},
		{"contradictory layers", func(c map[string]any, p map[string]string) { c["layer_types"] = []string{"full_attention"} }, rowPolicy(1), MetadataInvalid},
		{"overflow", func(c map[string]any, p map[string]string) { c["num_hidden_layers"] = math.MaxInt64 }, rowPolicy(1), ModelUnmodelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := denseAttentionConfig()
			dir, svc, params := needFixture(t, cfg)
			tc.mutate(cfg, params)
			data := fmtValue(cfg)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0600))
			n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, tc.policy, map[string]schema.ParamShape{"cache": {KVCacheDtype: "both"}, "override": {ContextOverride: true}})
			require.False(t, ok)
			assert.Equal(t, tc.cause, n.UnknownCause)
			assert.Equal(t, int64(1024), n.WeightsMiB)
		})
	}
	dir, svc, params := needFixture(t, denseAttentionConfig())
	for _, name := range []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"} {
		require.NoError(t, os.Remove(filepath.Join(dir, name)))
	}
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
	require.False(t, ok)
	assert.Equal(t, WeightsUnavailable, n.UnknownCause)
}

func TestUnknownCausePrecedenceIsDeterministic(t *testing.T) {
	dir, svc, params := needFixture(t, denseAttentionConfig())
	params["cache"] = "unsupported"
	shapes := map[string]schema.ParamShape{"cache": {KVCacheDtype: "both"}}
	for _, data := range []string{"{", `{"num_hidden_layers":1}`, ""} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0600))
		n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, shapes)
		require.False(t, ok)
		assert.Equal(t, MetadataInvalid, n.UnknownCause)
		assert.Equal(t, int64(1024), n.WeightsMiB)
	}
	require.NoError(t, os.Remove(filepath.Join(dir, "config.json")))
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, shapes)
	require.False(t, ok)
	assert.Equal(t, MetadataInvalid, n.UnknownCause)
	for _, name := range []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"} {
		require.NoError(t, os.Remove(filepath.Join(dir, name)))
	}
	n, ok = ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, shapes)
	require.False(t, ok)
	assert.Equal(t, WeightsUnavailable, n.UnknownCause)
}

func TestGGUFUnknownCauses(t *testing.T) {
	for _, tc := range []struct {
		arch  string
		extra map[string]int64
		cause UnknownCause
	}{
		{"unsupported", nil, ModelUnmodelled},
		{"llama", map[string]int64{"block_count": 0}, MetadataInvalid},
		{"llama", map[string]int64{"attention.sliding_window": 512}, ModelUnmodelled},
	} {
		t.Run(tc.arch+fmtValue(tc.extra), func(t *testing.T) {
			n := ggufNeed(t, tc.arch, map[string]string{"ctx-size": "4096"}, tc.extra)
			assert.Equal(t, tc.cause, n.UnknownCause)
			assert.Equal(t, int64(16), n.WeightsMiB)
			assert.Zero(t, n.Total())
		})
	}
}

func TestUnknownAssessmentRetainsIndependentEvidence(t *testing.T) {
	for _, tc := range []struct {
		kind, value string
		devices     []gpu.MemoryDevice
		cause       UnknownCause
	}{
		{schema.MemoryFractionTotal, "bad", nil, MemoryEvidenceUnavailable},
		{schema.MemoryFractionTotal, "auto", nil, MemoryEvidenceUnavailable},
		{schema.MemoryExternal, "", nil, BudgetNotSized},
		{schema.MemoryReserveMiB, "10", []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, BudgetNotSized},
	} {
		t.Run(tc.kind+tc.value, func(t *testing.T) {
			n := Need{WeightsMiB: 1024, ContextMode: "numeric", ContextTokens: 32768, UnknownCause: ModelUnmodelled}
			a, _ := AdmitMemoryBudget(schema.MemoryBudget{Kind: tc.kind, Parameter: "budget"}, n, map[string]string{"budget": tc.value}, nil, tc.devices, 0, nil, nil, 0)
			assert.Equal(t, "unknown", a.Status)
			assert.Equal(t, tc.cause, a.UnknownCause)
			assert.Equal(t, n.WeightsMiB, a.WeightsMiB)
			assert.Equal(t, n.ContextTokens, a.ContextTokens)
			assert.Zero(t, a.CapacityMiB)
		})
	}
}

func TestHybridAllocationUnknownUsesExistingBudgetFallback(t *testing.T) {
	dir, svc, params := needFixture(t, hybridConfig())
	params["max-model-len"] = "auto"
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{AutoMinContextTokens: 4096}, nil)
	require.False(t, ok)
	require.Equal(t, ModelUnmodelled, n.UnknownCause)
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 9800}}
	for _, value := range []string{"auto", "0.9"} {
		params["budget"] = value
		a, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}, n, params, nil, devices, 0, nil, nil, 0)
		require.NoError(t, err)
		assert.Equal(t, "unknown", a.Status)
		assert.Equal(t, ModelUnmodelled, a.UnknownCause)
		assert.Equal(t, int64(1024), a.WeightsMiB)
		assert.Equal(t, int64(9800), a.CapacityMiB)
		assert.Zero(t, a.RequiredMiB)
		want := "0.7752"
		if value != "auto" {
			want = value
		}
		assert.Equal(t, want, params["budget"])
	}
}

func TestUnknownCauseDoesNotChangeMemoryDecisions(t *testing.T) {
	// Existing pre-contract fixtures: requirements and capacities are pinned only for decisions.
	for _, tc := range []struct {
		name, fraction, status   string
		need                     Need
		free, required, capacity int64
	}{
		{"rounded boundary", "auto", "fit", Need{WeightsMiB: 2048, KVMiB: 512, OverheadMiB: 2048}, 7117, 7117, 7117},
		{"margin short", "auto", "no_fit", Need{WeightsMiB: 2048, KVMiB: 512, OverheadMiB: 2048}, 7116, 7117, 7116},
		{"fraction short", "0.5", "no_fit", Need{WeightsMiB: 2048, KVMiB: 512, OverheadMiB: 2048}, 10000, 5069, 5000},
		{"incomplete", "0.9", "unknown", Need{WeightsMiB: 1024, UnknownCause: ModelUnmodelled}, 10000, 0, 0},
		{"weight refusal", "0.5", "no_fit", Need{WeightsMiB: 5001, UnknownCause: ModelUnmodelled}, 10000, 5001, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}, tc.need, map[string]string{"budget": tc.fraction}, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: tc.free}}, 0, nil, nil, 0)
			assert.Equal(t, tc.status, a.Status)
			assert.Equal(t, tc.need.WeightsMiB, a.WeightsMiB)
			if a.Status != "unknown" {
				assert.Equal(t, tc.required, a.RequiredMiB)
				assert.Equal(t, tc.capacity, a.CapacityMiB)
				assert.Empty(t, a.UnknownCause)
			}
		})
	}
}

func TestUnknownCapacityUsesExistingFitEvidence(t *testing.T) {
	for _, kind := range []string{schema.MemoryFractionTotal, schema.MemoryLayers, schema.MemoryUnified} {
		for _, fraction := range []string{"0.9", "auto"} {
			t.Run(kind+fraction, func(t *testing.T) {
				budget := schema.MemoryBudget{Kind: kind, Parameter: "budget"}
				devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 9800, Unified: kind == schema.MemoryUnified}}
				params := map[string]string{"budget": fraction}
				n := Need{WeightsMiB: 1, KVMiB: 1, geometry: &attentionConfig{}}
				fit, err := AdmitMemoryBudget(budget, n, maps.Clone(params), nil, devices, 20000, nil, nil, .75)
				require.NoError(t, err)
				require.Equal(t, "fit", fit.Status)
				n.UnknownCause = ModelUnmodelled
				n.geometry = nil
				unknown, err := AdmitMemoryBudget(budget, n, maps.Clone(params), nil, devices, 20000, nil, nil, .75)
				require.NoError(t, err)
				assert.Equal(t, "unknown", unknown.Status)
				assert.Equal(t, fit.CapacityMiB, unknown.CapacityMiB)
			})
		}
	}
}

func TestUnknownOffloadCapacityUsesKnownWeights(t *testing.T) {
	for _, ram := range []int64{0, 20000} {
		t.Run(fmtValue(ram), func(t *testing.T) {
			n := Need{WeightsMiB: 11000, WeightsBytes: 11000 << 20, UnknownCause: ModelUnmodelled, ContextMode: "numeric", ContextTokens: 32768}
			a, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryLayers}, n, nil, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, ram, nil, nil, 0)
			require.NoError(t, err, "missing capacity must preserve admission")
			assert.Equal(t, "unknown", a.Status)
			assert.Equal(t, n.WeightsMiB, a.WeightsMiB)
			assert.Equal(t, n.ContextTokens, a.ContextTokens)
			assert.Zero(t, a.RequiredMiB)
			if ram == 0 {
				assert.Zero(t, a.CapacityMiB)
				assert.Equal(t, MemoryEvidenceUnavailable, a.UnknownCause)
			} else {
				assert.Equal(t, int64(30000), a.CapacityMiB)
				assert.Equal(t, ModelUnmodelled, a.UnknownCause)
			}
		})
	}
}

func TestLegacyExplainableAssessmentNormalization(t *testing.T) {
	for _, mode := range []string{"", "numeric"} {
		for _, status := range []string{"fit", "no_fit", "unknown"} {
			a := MemoryAssessment{Status: status, ContextMode: mode, ContextTokens: 4096, RequiredMiB: 17, CapacityMiB: 19}
			a.NormalizeLegacy()
			if mode == "" {
				assert.Equal(t, "unknown", a.Status)
				assert.Equal(t, NodeOutdated, a.UnknownCause)
				assert.Zero(t, a.RequiredMiB)
			} else {
				assert.Equal(t, status, a.Status)
				assert.Equal(t, int64(19), a.CapacityMiB)
				if status == "unknown" {
					assert.Equal(t, NodeOutdated, a.UnknownCause)
				} else {
					assert.Empty(t, a.UnknownCause)
				}
			}
		}
	}
}
