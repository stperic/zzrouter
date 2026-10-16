package process

import (
	"encoding/json"
	"maps"
	"math"
	"strconv"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hybridConfig() map[string]any {
	layers := make([]string, 64)
	for i := range layers {
		layers[i] = "linear_attention"
		if (i+1)%4 == 0 {
			layers[i] = "full_attention"
		}
	}
	return map[string]any{"num_hidden_layers": 64, "num_key_value_heads": 4, "num_attention_heads": 24,
		"hidden_size": 5120, "head_dim": 256, "dtype": "bfloat16", "max_position_embeddings": 262144,
		"layer_types": layers, "full_attention_interval": 4, "linear_conv_kernel_dim": 4,
		"linear_key_head_dim": 128, "linear_value_head_dim": 128, "linear_num_key_heads": 16,
		"linear_num_value_heads": 48, "mamba_ssm_dtype": "float32", "mtp_num_hidden_layers": 1}
}

func rowPolicy(rows int64) schema.AutoContext {
	return schema.AutoContext{AutoMinContextTokens: 4096, RecurrentStateRows: &rows}
}

func TestHybridNeedUsesOnlyFullAttentionKV(t *testing.T) {
	for _, rows := range []int64{1, 2, 4} {
		t.Run(strconv.FormatInt(rows, 10), func(t *testing.T) {
			dir, svc, params := needFixture(t, map[string]any{"text_config": hybridConfig()})
			params["max-model-len"] = "auto"
			n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, rowPolicy(rows), nil)
			require.True(t, ok)
			cache, ok := n.geometry.kvBytes(4096)
			require.True(t, ok)
			assert.Equal(t, int64(268435456)+156893184*rows, cache)
			assert.Equal(t, int64(1<<30), n.WeightsBytes, "all weight shards count")
			params["max-num-seqs"] = "1"
			one, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, rowPolicy(rows), nil)
			require.True(t, ok)
			assert.Equal(t, n, one, "minimum viability is one request, independent of the batching limit")
		})
	}
}

func TestHybridKVOverrideDoesNotQuantizeRecurrentState(t *testing.T) {
	dir, svc, params := needFixture(t, hybridConfig())
	params["max-model-len"], params["cache"] = "4096", "fp8"
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, rowPolicy(2), map[string]schema.ParamShape{"cache": {KVCacheDtype: "both"}})
	require.True(t, ok)
	cache, ok := n.geometry.kvBytes(4096)
	require.True(t, ok)
	assert.Equal(t, int64(134217728+2*156893184), cache)
}

func TestHybridMissingOrContradictoryGeometryIsUnknown(t *testing.T) {
	for _, key := range []string{"linear_conv_kernel_dim", "linear_key_head_dim", "linear_value_head_dim", "linear_num_key_heads", "linear_num_value_heads"} {
		t.Run("missing "+key, func(t *testing.T) {
			cfg := hybridConfig()
			delete(cfg, key)
			assertHybridUnknown(t, cfg, rowPolicy(1))
		})
	}
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"linear_conv_kernel_dim", 0}, {"linear_num_key_heads", -1}, {"linear_num_value_heads", 47},
		{"linear_key_head_dim", math.MaxInt64}, {"linear_value_head_dim", math.MaxInt64},
		{"mamba_ssm_dtype", "float64"}, {"mamba_ssm_dtype", "int32"}, {"mamba_ssm_dtype", nil},
		{"full_attention_interval", 3}, {"full_attention_interval", -1}, {"full_attention_interval", 0},
		{"num_key_value_heads", 5}, {"num_key_value_heads", 48}, {"linear_conv_kernel_dim", math.MaxInt64},
		{"layer_types", []string{"linear_attention"}}, {"layer_types", nil},
	} {
		t.Run(tc.key+"="+fmtValue(tc.value), func(t *testing.T) {
			cfg := hybridConfig()
			cfg[tc.key] = tc.value
			assertHybridUnknown(t, cfg, rowPolicy(1))
		})
	}
	assertHybridUnknown(t, hybridConfig(), schema.AutoContext{})
	for _, dtype := range []string{"float16", "bfloat16", "float32", ""} {
		cfg := hybridConfig()
		cfg["mamba_ssm_dtype"] = dtype
		dir, svc, params := needFixture(t, cfg)
		_, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, rowPolicy(1), nil)
		require.True(t, ok, dtype)
	}
}

func fmtValue(v any) string { data, _ := json.Marshal(v); return string(data) }

func assertHybridUnknown(t *testing.T, cfg map[string]any, policy schema.AutoContext) {
	t.Helper()
	dir, svc, params := needFixture(t, cfg)
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, policy, nil)
	require.False(t, ok)
	assert.Equal(t, int64(1<<30), n.WeightsBytes)
	assert.Zero(t, n.Total())
	params["budget"] = ".9"
	a, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}, n, params, nil,
		[]gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, 0, nil, nil, 0)
	require.NoError(t, err, "incomplete evidence cannot produce a sized refusal")
	assert.Equal(t, "unknown", a.Status)
}

func TestHybridAffordableContextUsesSameAllocation(t *testing.T) {
	dir, svc, params := needFixture(t, hybridConfig())
	params["max-model-len"] = "auto"
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, rowPolicy(1), nil)
	require.True(t, ok)
	budget := schema.MemoryBudget{Kind: schema.MemoryUnified}
	devices := []gpu.MemoryDevice{{UUID: "unified", TotalMiB: 12000, FreeMiB: 12000, Unified: true}}
	a, err := AdmitMemoryBudget(budget, n, params, nil, devices, 12000, nil, nil, .75)
	require.NoError(t, err)
	require.Equal(t, "fit", a.Status)
	require.Greater(t, a.AffordableContextTokens, int64(4096))
	for _, delta := range []int64{0, 1} {
		candidate, ok := n.atContext(a.AffordableContextTokens + delta)
		require.True(t, ok)
		_, _, _, err := sizedMemoryFit(budget, candidate, devices, 12000, .75, false, nil)
		assert.Equal(t, delta == 0, err == nil)
	}
	devices = append(devices, gpu.MemoryDevice{UUID: "GPU-B", TotalMiB: 12000, FreeMiB: 12000})
	budget = schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget", DeviceCountParameter: "devices"}
	params["budget"], params["devices"] = ".9", "2"
	a, err = AdmitMemoryBudget(budget, n, params, map[string]string{"CUDA_VISIBLE_DEVICES": "unified,GPU-B"}, devices, 0, nil, nil, 0)
	require.NoError(t, err)
	assert.Equal(t, "unknown", a.Status)
	assert.Zero(t, a.AffordableContextTokens)
}

func TestHybridMTPExcludedFromNormalCache(t *testing.T) {
	// Supplied HF text configs count 64 decoder layers, with mtp_num_hidden_layers=1 separate.
	// GGUF includes that MTP block in block_count, so 65-1 must give the same cache.
	cfg := hybridConfig()
	dir, svc, params := needFixture(t, cfg)
	params["max-model-len"] = "4096"
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, rowPolicy(1), nil)
	require.True(t, ok)
	assert.Equal(t, int64(64), n.geometry.Layers)
	assert.Equal(t, bytesToMiB(425328640), n.KVMiB)

	extra := map[string]int64{"block_count": 65, "nextn_predict_layers": 1, "full_attention_interval": 4,
		"embedding_length": 5120, "attention.head_count": 24, "attention.head_count_kv": 4,
		"attention.key_length": 256, "attention.value_length": 256, "ssm.conv_kernel": 4,
		"ssm.group_count": 16, "ssm.time_step_rank": 48, "ssm.state_size": 128, "ssm.inner_size": 6144}
	shapes := map[string]schema.ParamShape{"slots": {Kind: schema.ParamInt, SequenceSlots: true, AutoContext: schema.AutoContext{RecurrentStateRows: ptrRows(4)}}}
	for _, slots := range []string{"", "1", "4"} {
		params := map[string]string{"ctx-size": "4096", "slots": slots}
		n := ggufNeed(t, "qwen35", params, extra, shapes)
		require.False(t, n.Incomplete)
		rows := int64(4)
		if slots == "1" {
			rows = 1
		}
		cache, ok := n.geometry.kvBytes(4096)
		require.True(t, ok)
		assert.Equal(t, int64(268435456)+rows*156893184, cache, "KV is not multiplied by slots")
	}
	for key, value := range map[string]int64{"nextn_predict_layers": 65, "ssm.inner_size": 6145, "full_attention_interval": 0, "ssm.group_count": 0} {
		bad := maps.Clone(extra)
		bad[key] = value
		assert.True(t, ggufNeed(t, "qwen35", map[string]string{"ctx-size": "4096"}, bad, shapes).Incomplete)
	}
	for _, value := range []string{"0", "-1", "1025", "bad"} {
		assert.True(t, ggufNeed(t, "qwen35", map[string]string{"ctx-size": "4096", "slots": value}, extra, shapes).Incomplete)
		assert.False(t, ggufNeed(t, "qwen3", map[string]string{"ctx-size": "4096", "slots": value}, nil, shapes).Incomplete, "slot count matters only to recurrent layers")
	}
	moe := ggufNeed(t, "qwen3moe", map[string]string{"ctx-size": "4096"}, map[string]int64{"block_count": 48, "attention.head_count_kv": 4, "expert_count": 128, "expert_used_count": 8})
	require.False(t, moe.Incomplete)
	assert.Equal(t, int64(402653184), moe.KVMiB*1024*1024)
	assert.Equal(t, int64(16<<20), moe.WeightsBytes)
}

func TestNativeHybridOverflowRemainsUnknown(t *testing.T) {
	extra := map[string]int64{"block_count": 65, "nextn_predict_layers": 1, "full_attention_interval": 4,
		"context_length": 262144, "embedding_length": 5120, "attention.head_count": 24, "attention.head_count_kv": 4,
		"attention.key_length": 256, "attention.value_length": 256, "ssm.conv_kernel": 4,
		"ssm.group_count": 16, "ssm.time_step_rank": 48, "ssm.state_size": 128, "ssm.inner_size": 6144}
	shapes := map[string]schema.ParamShape{"slots": {SequenceSlots: true, AutoContext: schema.AutoContext{RecurrentStateRows: ptrRows(4)}}}
	for _, context := range []string{"auto", "262144"} {
		n := ggufNeed(t, "qwen35", map[string]string{"ctx-size": context}, extra, shapes)
		require.False(t, n.Incomplete)
		devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}
		budget := schema.MemoryBudget{Kind: schema.MemoryLayers}
		a, err := AdmitMemoryBudget(budget, n, nil, nil, devices, 1000, nil, nil, 0)
		if context == "auto" {
			require.NoError(t, err)
			assert.Equal(t, "unknown", a.Status)
		} else {
			var fit *MemoryFitError
			require.ErrorAs(t, err, &fit)
			assert.Equal(t, "no_fit", a.Status)
			assert.True(t, fit.Sized)
		}
		devices[0].TotalMiB, devices[0].FreeMiB = 8, 8
		a, err = AdmitMemoryBudget(budget, n, nil, nil, devices, 1, nil, nil, 0)
		var fit *MemoryFitError
		require.ErrorAs(t, err, &fit)
		assert.Equal(t, "no_fit", a.Status)
		assert.False(t, fit.Sized, "native auto does not bypass the exact weight lower bound")
	}
}

func ptrRows(n int64) *int64 { return &n }
