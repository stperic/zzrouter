package process

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func needFixture(t *testing.T, cfg map[string]any) (string, config.ServiceConfig, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"} {
		file, err := os.Create(filepath.Join(dir, name))
		require.NoError(t, err)
		require.NoError(t, file.Truncate(512*1024*1024))
		require.NoError(t, file.Close())
	}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), data, 0600))
	svc := config.ServiceConfig{Defaults: &config.AppDefaultsConfig{Metadata: &config.AppMetadata{ContextParam: "max-model-len"}}}
	return dir, svc, map[string]string{"max-model-len": "2048", "max-num-seqs": "256"}
}

func denseAttentionConfig() map[string]any {
	return map[string]any{"num_hidden_layers": 32, "num_key_value_heads": 8, "num_attention_heads": 32, "hidden_size": 4096, "torch_dtype": "float16"}
}

func admitNeed(t *testing.T, need Need, total, free int64) string {
	t.Helper()
	params := map[string]string{"budget": "auto"}
	require.NoError(t, admitBudget(schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, need, params, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: total, FreeMiB: free}}, nil, nil))
	return params["budget"]
}

func TestMemoryNeedDenseGQA(t *testing.T) {
	dir, svc, params := needFixture(t, denseAttentionConfig())
	need, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
	require.True(t, ok)
	assert.Equal(t, Need{WeightsMiB: 1024, WeightsBytes: 1 << 30, KVMiB: 256, OverheadMiB: DefaultActivationOverheadMiB}, memoryAmounts(need))
	assert.Equal(t, "0.5914", admitNeed(t, need, 10000, 9500), "(1024+256+4096) x 1.1 of 10000 MiB")
}

func TestDeletionMetadataOriginContract(t *testing.T) {
	dir, svc, params := needFixture(t, denseAttentionConfig())
	outside := filepath.Join(t.TempDir(), "weights.gguf")
	require.NoError(t, os.WriteFile(outside, []byte("GGUF"), 0600))
	require.NoError(t, os.Truncate(outside, 1<<30))
	source := filepath.Join(dir, "weights.gguf")
	require.NoError(t, os.Symlink(outside, source))
	selected := ResolveModel(svc, source)
	origin, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, origin, selected.Directory(), "metadata belongs to the selected repository")
	need, ok := ModelMemoryNeed(selected, svc, params, schema.AutoContext{}, nil)
	require.True(t, ok, "frozen weights must retain original attention metadata")
	require.Equal(t, Need{WeightsMiB: 1024, WeightsBytes: 1 << 30, KVMiB: 256, OverheadMiB: DefaultActivationOverheadMiB}, memoryAmounts(need))
	require.NoError(t, os.Remove(source))
	require.NoError(t, os.Mkdir(source, 0700))
	require.Equal(t, origin, selected.Directory(), "a changed source leaf cannot redirect the frozen metadata repository")
}

func TestMemoryNeedHybridTextConfig(t *testing.T) {
	layers := make([]string, 64)
	for i := range layers {
		layers[i] = "linear_attention"
		if i%4 == 3 {
			layers[i] = "full_attention"
		}
	}
	cfg := map[string]any{"text_config": map[string]any{"num_hidden_layers": 64, "num_key_value_heads": 4, "num_attention_heads": 24, "head_dim": 256, "hidden_size": 5120, "dtype": "bfloat16", "sliding_window": nil, "full_attention_interval": 4, "layer_types": layers}, "model_type": "synthetic_hybrid", "num_hidden_layers": 999}
	dir, svc, params := needFixture(t, cfg)
	params["max-model-len"] = "8192"
	// vLLM needs one recurrent state per max-num-seqs sequence (lab, Qwen3.8-27B: 256 seqs, room for 105).
	_, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
	assert.False(t, ok, "linear-attention state cannot be sized generically")

	for i := range layers {
		layers[i] = "full_attention"
	}
	dir, svc, params = needFixture(t, cfg)
	params["max-model-len"] = "8192"
	need, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
	require.True(t, ok, "the same geometry without recurrent layers is sized")
	assert.Equal(t, int64(2048), need.KVMiB, "text_config nesting and the dtype key are read")
}

func TestMemoryNeedSlidingAttention(t *testing.T) {
	cfg := denseAttentionConfig()
	cfg["num_hidden_layers"] = 2
	cfg["layer_types"] = []string{"full_attention", "sliding_attention"}
	cfg["sliding_window"] = 512
	dir, svc, params := needFixture(t, cfg)
	for _, tc := range []struct {
		context string
		kv      int64
	}{{"2048", 10}, {"128", 1}} {
		t.Run(tc.context, func(t *testing.T) {
			params["max-model-len"] = tc.context
			need, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
			require.True(t, ok)
			assert.Equal(t, tc.kv, need.KVMiB)
		})
	}
}

func TestMemoryNeedUnknownFallsBack(t *testing.T) {
	for _, scenario := range []string{"MLA", "null MLA", "unknown layer", "missing config", "unparseable config", "missing context metadata", "missing context value", "unparseable context", "missing layers", "missing heads", "missing dimension", "missing dtype", "unknown dtype", "unassigned sliding window", "missing sliding window", "invalid layer count", "invalid KV heads", "null KV heads", "null head dimension", "overflow"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := denseAttentionConfig()
			switch scenario {
			case "MLA":
				cfg["kv_lora_rank"] = 512
			case "null MLA":
				cfg["kv_lora_rank"] = nil
			case "unknown layer":
				cfg["num_hidden_layers"] = 1
				cfg["layer_types"] = []string{"mystery"}
			case "missing layers":
				delete(cfg, "num_hidden_layers")
			case "missing heads":
				delete(cfg, "num_attention_heads")
			case "missing dimension":
				delete(cfg, "hidden_size")
			case "missing dtype":
				delete(cfg, "torch_dtype")
			case "unknown dtype":
				cfg["torch_dtype"] = "auto"
			case "unassigned sliding window":
				cfg["sliding_window"] = 512
			case "missing sliding window":
				cfg["num_hidden_layers"] = 1
				cfg["layer_types"] = []string{"sliding_attention"}
			case "invalid layer count":
				cfg["layer_types"] = []string{"full_attention"}
			case "null KV heads":
				cfg["num_key_value_heads"] = nil
			case "null head dimension":
				cfg["head_dim"] = nil
			case "invalid KV heads":
				cfg["num_key_value_heads"] = 0
			case "overflow":
				cfg["num_hidden_layers"] = int64(1 << 62)
			}
			dir, svc, params := needFixture(t, cfg)
			switch scenario {
			case "missing config":
				require.NoError(t, os.Remove(filepath.Join(dir, "config.json")))
			case "unparseable config":
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte("{"), 0600))
			case "missing context metadata":
				svc.Defaults.Metadata = nil
			case "missing context value":
				delete(params, "max-model-len")
			case "unparseable context":
				params["max-model-len"] = "auto"
			}
			need, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
			require.False(t, ok)
			require.Equal(t, Need{WeightsMiB: 1024, WeightsBytes: 1 << 30, Incomplete: true}, memoryAmounts(need))
			devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000}}
			room, err := autoFraction(schema.MemoryBudget{Parameter: "budget"}, devices, nil)
			require.NoError(t, err)
			assert.Equal(t, room, 0.3952)
			assert.Equal(t, "0.3952", admitNeed(t, need, 10000, 6000))
		})
	}
}

func TestMemoryNeedFallbackGeometryAndMoE(t *testing.T) {
	cfg := denseAttentionConfig()
	delete(cfg, "num_key_value_heads")
	cfg["num_local_experts"] = 128
	cfg["torch_dtype"] = "float32"
	dir, svc, params := needFixture(t, cfg)
	need, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
	require.True(t, ok)
	assert.Equal(t, int64(2048), need.KVMiB)
}

func TestMemoryNeedDecidesFit(t *testing.T) {
	// Lab, Qwen2.5-0.5B beside a running model: capping a known need at the room handed vLLM 0.0056 and it failed later.
	need := Need{WeightsMiB: 50000, KVMiB: 10000, OverheadMiB: 2048}
	params := map[string]string{"budget": "auto"}
	var fit *MemoryFitError
	require.ErrorAs(t, admitBudget(schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, need, params, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 100000, FreeMiB: 40000}}, nil, nil), &fit)
	assert.Equal(t, int64(68260+5000), fit.RequiredMiB, "the shortfall eviction must cover is the sized need plus the margin")
	assert.Equal(t, "auto", params["budget"], "a refused attempt leaves auto for the retry")

	// Lab: a need that fits free memory but not beside the margin OOMed vLLM outside its budget.
	params = map[string]string{"budget": "auto"}
	require.ErrorAs(t, admitBudget(schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, Need{WeightsMiB: 952, OverheadMiB: 2048}, params, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 4000}}, nil, nil), &fit)
	assert.Equal(t, "0.3300", admitNeed(t, Need{WeightsMiB: 952, OverheadMiB: 2048}, 10000, 5400))
}

func TestMemoryNeedRoundsFractionUp(t *testing.T) {
	assert.Equal(t, "0.2817", admitNeed(t, Need{WeightsMiB: 1024, OverheadMiB: 2048}, 12000, 12000))
}

func TestMemoryNeedTensorParallel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		secondTotal int64
		want        string
	}{{"equal", 10000, "0.3661"}, {"different totals", 20000, "0.3661"}} {
		t.Run(tc.name, func(t *testing.T) {
			need := Need{WeightsMiB: 2048, KVMiB: 512, OverheadMiB: 2048}
			params := map[string]string{"budget": "auto", "devices": "2"}
			devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 9500}, {UUID: "GPU-B", TotalMiB: tc.secondTotal, FreeMiB: tc.secondTotal}}
			require.NoError(t, admitBudget(schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget", DeviceCountParameter: "devices"}, need, params, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A,GPU-B"}, devices, nil, nil))
			assert.Equal(t, tc.want, params["budget"])
		})
	}
}

func TestMemoryNeedSelectedGGUFShardsAndFeatures(t *testing.T) {
	dir, svc, params := needFixture(t, denseAttentionConfig())
	for _, name := range []string{"part-00001-of-00002.gguf", "part-00002-of-00002.gguf", "unselected.gguf", "projector.gguf"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600))
	}
	require.NoError(t, os.Truncate(filepath.Join(dir, "part-00001-of-00002.gguf"), 256*1024*1024))
	require.NoError(t, os.Truncate(filepath.Join(dir, "part-00002-of-00002.gguf"), 256*1024*1024+1))
	svc.Features = map[string]config.Feature{"vision": {Files: []string{"projector.gguf"}, Flag: "mmproj"}}
	need, ok := ModelMemoryNeed(ResolvedModel{Path: filepath.Join(dir, "part-00001-of-00002.gguf")}, svc, params, schema.AutoContext{}, nil)
	require.True(t, ok)
	assert.Equal(t, int64(513), need.WeightsMiB)
	_, ok = ModelMemoryNeed(ResolvedModel{Path: filepath.Join(dir, "projector.gguf")}, svc, params, schema.AutoContext{}, nil)
	assert.False(t, ok)
	require.NoError(t, os.Remove(filepath.Join(dir, "part-00002-of-00002.gguf")))
	_, ok = ModelMemoryNeed(ResolvedModel{Path: filepath.Join(dir, "part-00001-of-00002.gguf")}, svc, params, schema.AutoContext{}, nil)
	assert.False(t, ok)
}

func TestMemoryNeedDisabledSlidingWindowIsDense(t *testing.T) {
	cfg := denseAttentionConfig()
	dir, svc, params := needFixture(t, cfg)
	dense, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
	require.True(t, ok)
	cfg["sliding_window"] = 32768
	cfg["use_sliding_window"] = false
	dir, svc, params = needFixture(t, cfg)
	need, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
	require.True(t, ok, "a declared but disabled window is a dense model")
	assert.Equal(t, memoryAmounts(dense), memoryAmounts(need))
	cfg["use_sliding_window"] = true
	dir, svc, params = needFixture(t, cfg)
	_, ok = ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
	assert.False(t, ok, "an enabled window without layer_types cannot be placed")
}

func memoryAmounts(n Need) Need { n.ContextMode, n.ContextTokens, n.geometry = "", 0, nil; return n }
