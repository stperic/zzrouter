package process

import (
	"bytes"
	"encoding/binary"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func autoNeed(t *testing.T, cfg map[string]any) Need {
	t.Helper()
	cfg["max_position_embeddings"] = 131072
	dir, svc, params := needFixture(t, cfg)
	params["max-model-len"] = "auto"
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{AutoMinContextTokens: 4096}, nil)
	require.True(t, ok)
	return n
}
func TestAutoContextMinimumEstablishesFit(t *testing.T) {
	n := autoNeed(t, denseAttentionConfig())
	budget := schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}
	for _, value := range []string{"auto", "0.95"} {
		params := map[string]string{"budget": value}
		a, err := AdmitMemoryBudget(budget, n, params, nil, devices, 0, nil, nil, 0)
		require.NoError(t, err)
		assert.Equal(t, "fit", a.Status)
		assert.Equal(t, "auto", a.ContextMode)
		assert.Equal(t, int64(4096), a.ContextTokens)
		required := int64(6196)
		if value == "auto" {
			required += 2048
			assert.Equal(t, "0.7952", params["budget"])
		} else {
			assert.Equal(t, value, params["budget"])
		}
		assert.Equal(t, required, a.RequiredMiB)
		assert.GreaterOrEqual(t, a.AffordableContextTokens, int64(4096))
	}
}
func TestNumericFractionChecksSizedModelNeed(t *testing.T) {
	n := autoNeed(t, denseAttentionConfig())
	for _, mode := range []string{"auto", "numeric"} {
		n.ContextMode = mode
		params := map[string]string{"budget": "0.6"}
		before := maps.Clone(params)
		a, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}, n, params, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, 0, []RunRef{{ID: "holder", Commitments: map[string]int64{"GPU-A": 9999}}}, nil, 0)
		var fit *MemoryFitError
		require.ErrorAs(t, err, &fit)
		assert.True(t, fit.Sized)
		assert.Empty(t, fit.Holders)
		assert.Equal(t, "no_fit", a.Status)
		assert.Equal(t, before, params)
		assert.Contains(t, a.Reason, "context requirement")
	}
}
func TestAffordableContextUsesAdmissionArithmetic(t *testing.T) {
	for _, sliding := range []bool{false, true} {
		cfg := denseAttentionConfig()
		if sliding {
			cfg["layer_types"] = []string{"full_attention", "sliding_attention"}
			cfg["num_hidden_layers"] = 2
			cfg["sliding_window"] = 512
		}
		n := autoNeed(t, cfg)
		for _, value := range []string{"auto", "0.65"} {
			budget := schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}
			devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}
			a, _ := AdmitMemoryBudget(budget, n, map[string]string{"budget": value}, nil, devices, 0, nil, nil, 0)
			require.Positive(t, a.AffordableContextTokens)
			for _, tokens := range []int64{a.AffordableContextTokens, a.AffordableContextTokens + 1} {
				if tokens > n.geometry.NativeContext {
					continue
				}
				candidate, ok := n.atContext(tokens)
				require.True(t, ok)
				_, err := AdmitMemoryBudget(budget, candidate, map[string]string{"budget": value}, nil, devices, 0, nil, nil, 0)
				assert.Equal(t, tokens > a.AffordableContextTokens, err != nil, "tokens=%d boundary=%d sliding=%v", tokens, a.AffordableContextTokens, sliding)
			}
		}
	}
}
func TestAutoContextRequiresCompleteEvidence(t *testing.T) {
	for _, missing := range []string{"none", "minimum", "range", "geometry", "scaled range", "range override", "selected device"} {
		t.Run(missing, func(t *testing.T) {
			cfg := denseAttentionConfig()
			cfg["max_position_embeddings"] = 131072
			policy := schema.AutoContext{AutoMinContextTokens: 4096}
			switch missing {
			case "minimum":
				policy.AutoMinContextTokens = 0
			case "range":
				delete(cfg, "max_position_embeddings")
			case "geometry":
				delete(cfg, "num_hidden_layers")
			case "scaled range":
				cfg["rope_scaling"] = map[string]any{"factor": 2}
			}
			dir, svc, params := needFixture(t, cfg)
			params["max-model-len"] = "auto"
			params["budget"] = "0.95"
			if missing == "range override" {
				params["custom-override"] = `{ "factor": 2 }`
			}
			n, _ := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, policy, map[string]schema.ParamShape{"custom-override": {ContextOverride: true}})
			devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}
			if missing == "selected device" {
				devices = nil
			}
			a, _ := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}, n, params, nil, devices, 0, nil, nil, 0)
			want := "unknown"
			if missing == "none" {
				want = "fit"
			}
			assert.Equal(t, want, a.Status)
			assert.Equal(t, "auto", a.ContextMode)
		})
	}
}
func TestAutoContextSelectedDevicesAndMargins(t *testing.T) {
	n := autoNeed(t, denseAttentionConfig())
	budget := schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget", DeviceCountParameter: "tp", AutoSafetyMarginMiB: 1000}
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}, {UUID: "GPU-B", TotalMiB: 20000, FreeMiB: 20000}}
	params := map[string]string{"budget": "auto", "tp": "2"}
	env := map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A,GPU-B"}
	a, err := AdmitMemoryBudget(budget, n, maps.Clone(params), env, devices, 0, nil, nil, 0)
	require.NoError(t, err)
	assert.Equal(t, "fit", a.Status)
	devices[1].FreeMiB = 11702
	_, err = AdmitMemoryBudget(budget, n, maps.Clone(params), env, devices, 0, nil, nil, 0)
	require.NoError(t, err)
	devices[1].FreeMiB--
	_, err = AdmitMemoryBudget(budget, n, maps.Clone(params), env, devices, 0, nil, nil, 0)
	require.Error(t, err)
	devices[1].FreeMiB = 11702
	_, err = AdmitMemoryBudget(budget, n, maps.Clone(params), env, devices, 0, []RunRef{{ID: "reserved", Commitments: map[string]int64{"GPU-B": 20000}}}, nil, 0)
	require.Error(t, err)
}

func ggufNeed(t *testing.T, arch string, params map[string]string, extra map[string]int64) Need {
	t.Helper()
	dir, svc, _ := needFixture(t, denseAttentionConfig())
	require.NoError(t, os.Remove(filepath.Join(dir, "config.json")))
	values := map[string]int64{"block_count": 32, "context_length": 32768, "embedding_length": 4096, "attention.head_count": 32, "attention.head_count_kv": 8}
	maps.Copy(values, extra)
	var b bytes.Buffer
	write := func(v any) { require.NoError(t, binary.Write(&b, binary.LittleEndian, v)) }
	text := func(v string) { write(uint64(len(v))); b.WriteString(v) }
	write(uint32(0x46554747))
	write(uint32(3))
	write(uint64(0))
	write(uint64(len(values) + 1))
	text("general.architecture")
	write(uint32(8))
	text(arch)
	for key, value := range values {
		text(arch + "." + key)
		write(uint32(10))
		write(uint64(value))
	}
	first := filepath.Join(dir, "part-00001-of-00002.gguf")
	second := filepath.Join(dir, "part-00002-of-00002.gguf")
	require.NoError(t, os.WriteFile(first, b.Bytes(), 0600))
	// Small shards: Windows runners allocate every byte of a truncated file.
	require.NoError(t, os.Truncate(first, 8<<20))
	require.NoError(t, os.WriteFile(second, []byte("not a header"), 0600))
	require.NoError(t, os.Truncate(second, 8<<20))
	svc.Defaults.Metadata.ContextParam = "ctx-size"
	n, _ := ModelMemoryNeed(ResolvedModel{Path: second}, svc, params, schema.AutoContext{AutoContextSource: "native"}, map[string]schema.ParamShape{"cache-type-k": {KVCacheDtype: "k"}, "cache-type-v": {KVCacheDtype: "v"}})
	return n
}
func TestGGUFContextAndQuantizedKV(t *testing.T) {
	for _, tc := range []struct {
		k, v string
		kv   int64
	}{{"", "", 4096}, {"q8_0", "q8_0", 2176}, {"q4_0", "q8_0", 1664}, {"f16", "f32", 6144}, {"bf16", "bf16", 4096}, {"q4_1", "q4_1", 1280}, {"q5_0", "q5_0", 1408}, {"q5_1", "q5_1", 1536}, {"iq4_nl", "iq4_nl", 1152}} {
		n := ggufNeed(t, "llama", map[string]string{"ctx-size": "auto", "cache-type-k": tc.k, "cache-type-v": tc.v}, nil)
		require.False(t, n.Incomplete)
		assert.Equal(t, int64(32768), n.ContextTokens)
		assert.Equal(t, tc.kv, n.KVMiB)
	}
	n := ggufNeed(t, "qwen2", map[string]string{"ctx-size": "4096"}, map[string]int64{"attention.key_length": 64, "attention.value_length": 256})
	require.False(t, n.Incomplete)
	assert.Equal(t, int64(640), n.KVMiB)
	assert.Equal(t, "numeric", n.ContextMode)
	for _, tc := range []struct {
		arch, cache string
		extra       map[string]int64
	}{{"mamba", "", nil}, {"deepseek2", "", nil}, {"llama", "unknown", nil}, {"llama", "", map[string]int64{"attention.sliding_window": 512}}} {
		assert.True(t, ggufNeed(t, tc.arch, map[string]string{"ctx-size": "auto", "cache-type-k": tc.cache}, tc.extra).Incomplete)
	}
}

func TestSplitGGUFReadsFirstShardHeader(t *testing.T) {
	// The fixture selects shard two, whose header is invalid; geometry lives in shard one.
	n := ggufNeed(t, "qwen3", map[string]string{"ctx-size": "auto"}, nil)
	require.False(t, n.Incomplete)
	assert.Equal(t, int64(32768), n.ContextTokens)
	assert.Equal(t, int64(16), n.WeightsMiB, "all shards still count as weights")
	assert.Equal(t, int64(4096), n.KVMiB)
}

func TestKVCacheDtypeRoles(t *testing.T) {
	dir, svc, params := needFixture(t, denseAttentionConfig())
	params["max-model-len"] = "4096"
	shapes := map[string]schema.ParamShape{"cache-encoding": {KVCacheDtype: "both"}}
	params["cache-encoding"] = "auto"
	auto, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, shapes)
	require.True(t, ok)
	for _, dtype := range []string{"fp8", "fp8_e4m3", "fp8_e5m2", "future-dtype"} {
		t.Run(dtype, func(t *testing.T) {
			params["cache-encoding"] = dtype
			n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, shapes)
			if dtype == "future-dtype" {
				require.False(t, ok)
				assert.True(t, n.Incomplete)
			} else {
				require.True(t, ok)
				assert.Equal(t, auto.KVMiB/2, n.KVMiB)
			}
			assessment, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}, n, map[string]string{"budget": "0.6"}, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, 0, nil, nil, 0)
			require.NoError(t, err, "fp8 fits and unknown dtype must not cause a sized refusal")
			want := "fit"
			if dtype == "future-dtype" {
				want = "unknown"
			}
			assert.Equal(t, want, assessment.Status)
		})
	}
	_, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}, auto, map[string]string{"budget": "0.6"}, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, 0, nil, nil, 0)
	require.Error(t, err, "auto KV exceeds the same numeric budget")
}
func TestLlamaPlacementBoundaries(t *testing.T) {
	// (16 weights + 4096 KV + 4096 overhead) * 1.1, rounded up.
	required := int64(9029)
	for _, context := range []string{"auto", "32768"} {
		n := ggufNeed(t, "llama", map[string]string{"ctx-size": context}, nil)
		for _, tc := range []struct {
			gpu, ram          int64
			status, placement string
		}{{required, 1, "fit", "gpu"}, {required - 1, 1, "fit", "offload"}, {required - 2, 1, "no_fit", ""}, {required - 1, 0, "unknown", ""}, {8, 1, "no_fit", ""}} {
			a, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryLayers}, n, nil, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: tc.gpu, FreeMiB: tc.gpu}}, tc.ram, nil, nil, 0)
			want := tc.status
			if context == "auto" && tc.gpu == required-2 {
				want = "unknown"
				assert.Contains(t, a.Reason, "engine may adapt")
				require.NoError(t, err, "native sized shortfall must not refuse admission")
			}
			if want == "no_fit" {
				require.Error(t, err, "numeric and weight lower bound failures still refuse")
			}
			assert.Equal(t, want, a.Status)
			assert.Equal(t, tc.placement, a.Placement)
			assert.Equal(t, int64(32768), a.ContextTokens)
		}
	}
}
func TestMLXDeclaredFractionAndMinimum(t *testing.T) {
	n := autoNeed(t, denseAttentionConfig())
	for _, tc := range []struct {
		fraction float64
		status   string
	}{{0.75, "fit"}, {0.6, "no_fit"}, {0, "unknown"}} {
		a, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryUnified}, n, nil, nil, []gpu.MemoryDevice{{UUID: "unified", TotalMiB: 10000, FreeMiB: 10000, Unified: true}}, 10000, nil, nil, tc.fraction)
		assert.Equal(t, tc.status, a.Status)
		assert.Equal(t, "auto", a.ContextMode)
		assert.Equal(t, int64(4096), a.ContextTokens)
		if tc.status == "no_fit" {
			assert.Positive(t, a.AffordableContextTokens)
			assert.Less(t, a.AffordableContextTokens, int64(4096))
		}
		if tc.status == "fit" {
			require.NoError(t, err)
			assert.Positive(t, a.AffordableContextTokens)
			assert.Empty(t, a.Placement)
		}
	}
	// Generation limits never become context evidence.
	cfg := denseAttentionConfig()
	cfg["max_position_embeddings"] = 131072
	dir, svc, params := needFixture(t, cfg)
	svc.Defaults.Metadata.ContextParam = ""
	params["max-tokens"] = "99999"
	actual, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{AutoMinContextTokens: 4096}, nil)
	require.True(t, ok)
	assert.Equal(t, int64(4096), actual.ContextTokens)
	assert.Equal(t, "auto", actual.ContextMode)
}

func TestMemoryDecisionHasNoEngineNameBranches(t *testing.T) {
	for _, file := range []string{"memory.go", "memory_auto.go", "memory_need.go", "../manager_memory.go", "../launch_features.go"} {
		node, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		require.NoError(t, err)
		ast.Inspect(node, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok && file == "../launch_features.go" && fn.Name.Name != "modelLaunchParameters" {
				return false
			}
			if literal, ok := n.(*ast.BasicLit); ok && literal.Kind == token.STRING {
				value, err := strconv.Unquote(literal.Value)
				require.NoError(t, err)
				assert.NotContains(t, []string{"vllm", "llamacpp", "llama.cpp", "mlx"}, value, "policy in %s must be declared metadata", file)
			}
			return true
		})
	}
}

func TestAutoContextAliasesAndNullScaling(t *testing.T) {
	cfg := denseAttentionConfig()
	cfg["max_position_embeddings"] = 32768
	cfg["rope_scaling"] = nil
	dir, svc, params := needFixture(t, cfg)
	delete(params, "max-model-len")
	params["max_model_len"] = "auto"
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{AutoMinContextTokens: 4096}, nil)
	require.True(t, ok)
	assert.Equal(t, "auto", n.ContextMode)
	assert.Equal(t, int64(4096), n.ContextTokens)
	canonical := ggufNeed(t, "llama", map[string]string{"ctx-size": "auto", "cache-type-k": "q8_0", "cache-type-v": "q4_0"}, nil)
	aliases := ggufNeed(t, "llama", map[string]string{"ctx_size": "auto", "cache_type_k": "q8_0", "cache_type_v": "q4_0"}, nil)
	assert.False(t, aliases.Incomplete)
	assert.Equal(t, canonical.KVMiB, aliases.KVMiB)
	assert.Equal(t, canonical.ContextTokens, aliases.ContextTokens)
}

func TestAffordableContextRespectsFixedReservationAvailability(t *testing.T) {
	n := autoNeed(t, denseAttentionConfig())
	a, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}, n, map[string]string{"budget": "0.95"}, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 9000}}, 0, nil, nil, 0)
	require.Error(t, err)
	assert.Equal(t, "no_fit", a.Status)
	assert.Zero(t, a.AffordableContextTokens)
}
