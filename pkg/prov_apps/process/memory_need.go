package process

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/gguf"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// DefaultActivationOverheadMiB is the engine's in-budget memory beyond weights and KV (activations, CUDA graphs,
// encoder profiling). Lab, vLLM on localai: gemma-4-E2B used 2.9 GiB (3 video items profiled), Qwen2.5-0.5B under 2.
const DefaultActivationOverheadMiB int64 = 4096

// Need is the model's weights, one context sequence's KV cache and per-device runtime overhead.
type Need struct {
	WeightsMiB, KVMiB, OverheadMiB int64
	WeightsBytes                   int64 // Exact bytes prevent false refusals at fractional MiB boundaries.
	Incomplete                     bool
	ContextMode                    string
	ContextTokens                  int64
	geometry                       *attentionConfig
}

// Total returns the single-device memory need; zero denotes unknown.
func (n Need) Total() int64 {
	if n.Incomplete || n.WeightsMiB > math.MaxInt64-n.KVMiB || n.WeightsMiB+n.KVMiB > math.MaxInt64-n.OverheadMiB {
		return 0
	}
	return n.WeightsMiB + n.KVMiB + n.OverheadMiB
}

type attentionConfig struct {
	NativeContext  int64           `json:"max_position_embeddings"`
	RopeScaling    json.RawMessage `json:"rope_scaling"`
	KeyDim         json.RawMessage
	ValueDim       json.RawMessage
	CacheK, CacheV string
	Layers         int64           `json:"num_hidden_layers"`
	KVHeads        json.RawMessage `json:"num_key_value_heads"`
	Heads          int64           `json:"num_attention_heads"`
	HeadDim        json.RawMessage `json:"head_dim"`
	HiddenSize     int64           `json:"hidden_size"`
	TorchDtype     string          `json:"torch_dtype"`
	Dtype          string          `json:"dtype"`
	LayerTypes     []string        `json:"layer_types"`
	SlidingWindow  *int64          `json:"sliding_window"`
	UseSliding     *bool           `json:"use_sliding_window"`
	KVLoRARank     json.RawMessage `json:"kv_lora_rank"`
}

// ModelMemoryNeed retains weight evidence when context or geometry cannot be sized.
func ModelMemoryNeed(model ResolvedModel, svc config.ServiceConfig, params map[string]string, policy schema.AutoContext) (Need, bool) {
	weightBytes, header, ok := weightsBytes(model.Path, svc)
	partial := Need{WeightsMiB: bytesToMiB(weightBytes), WeightsBytes: weightBytes, Incomplete: true, ContextMode: "unknown"}
	value := memoryParameter(svc.GetContextParam(), params)
	context, parseErr := strconv.ParseInt(value, 10, 64)
	numeric := parseErr == nil && context > 0
	if !numeric {
		context = 0
	}
	if numeric {
		partial.ContextMode = "numeric"
	} else if config.IsAuto(value) || svc.GetContextParam() == "" || (value == "0" && policy.AutoContextSource == "native") {
		partial.ContextMode = "auto"
		context = policy.AutoMinContextTokens
	}
	cfg, geometry := readAttentionConfig(model.Directory())
	if header != "" && (!geometry || policy.AutoContextSource == "native") {
		cfg, geometry = readGGUFConfig(header)
	}
	if partial.ContextMode == "auto" && policy.AutoContextSource == "native" {
		context = cfg.NativeContext
	}
	partial.ContextTokens = context
	if !ok || !geometry || context <= 0 || (partial.ContextMode == "auto" && (cfg.NativeContext < context || memoryParameter("rope-scaling", params) != "" || memoryParameter("hf-overrides", params) != "" || (len(cfg.RopeScaling) > 0 && string(cfg.RopeScaling) != "null"))) {
		return partial, false
	}
	cfg.CacheK, cfg.CacheV = memoryParameter("cache-type-k", params), memoryParameter("cache-type-v", params)
	partial.geometry = &cfg
	sized, complete := partial.atContext(context)
	if !complete {
		partial.geometry = nil
		return partial, false
	}
	return sized, true
}

func (n Need) atContext(context int64) (Need, bool) {
	kv, ok := n.geometry.kvBytes(context)
	n.KVMiB, n.OverheadMiB, n.Incomplete = bytesToMiB(kv), DefaultActivationOverheadMiB, !ok
	return n, ok
}

func readGGUFConfig(path string) (attentionConfig, bool) {
	file, err := os.Open(path)
	if err != nil {
		return attentionConfig{}, false
	}
	defer file.Close()
	h, err := gguf.Read(file)
	switch h.Architecture {
	case "llama", "qwen2", "qwen3":
	default:
		return attentionConfig{}, false
	}
	v := func(key string) int64 { return h.Values[h.Architecture+"."+key] }
	raw := func(n int64) json.RawMessage {
		if n == 0 {
			return nil
		}
		return json.RawMessage(strconv.FormatInt(n, 10))
	}
	cfg := attentionConfig{Layers: v("block_count"), NativeContext: v("context_length"), Heads: v("attention.head_count"), KVHeads: raw(v("attention.head_count_kv")), HiddenSize: v("embedding_length"), KeyDim: raw(v("attention.key_length")), ValueDim: raw(v("attention.value_length")), Dtype: "float16"}
	return cfg, err == nil && cfg.Layers > 0 && cfg.Heads > 0 && v("attention.sliding_window") == 0
}

// memoryParameter reads either spelling accepted by command construction.
func memoryParameter(key string, params map[string]string) string {
	value := params[key]
	if value == "" {
		value = params[strings.ReplaceAll(key, "-", "_")]
	}
	return value
}

// readAttentionConfig reads the language model's config.json, nested under text_config in multimodal models.
func readAttentionConfig(dir string) (attentionConfig, bool) {
	if dir == "" {
		return attentionConfig{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return attentionConfig{}, false
	}
	var envelope struct {
		TextConfig json.RawMessage `json:"text_config"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return attentionConfig{}, false
	}
	if len(envelope.TextConfig) > 0 {
		data = envelope.TextConfig
	}
	var cfg attentionConfig
	if json.Unmarshal(data, &cfg) != nil || cfg.Layers <= 0 || cfg.Heads <= 0 || len(cfg.KVLoRARank) > 0 {
		return attentionConfig{}, false
	}
	return cfg, true
}

// kvBytes is the KV cache of one sequence of context tokens.
func (cfg attentionConfig) kvBytes(context int64) (int64, bool) {
	heads, ok := geometryValue(cfg.KVHeads, cfg.Heads)
	if !ok {
		return 0, false
	}
	dim := int64(0)
	if cfg.HiddenSize > 0 && cfg.HiddenSize%cfg.Heads == 0 {
		dim = cfg.HiddenSize / cfg.Heads
	}
	if dim, ok = geometryValue(cfg.HeadDim, dim); !ok {
		return 0, false
	}
	dtypeBytes, ok := cfg.dtypeBytes()
	if !ok {
		return 0, false
	}
	layerTokens, ok := cfg.layerTokens(context)
	if !ok {
		return 0, false
	}
	var total int64
	for _, cache := range []struct {
		dim   json.RawMessage
		dtype string
	}{{cfg.KeyDim, cfg.CacheK}, {cfg.ValueDim, cfg.CacheV}} {
		dimension, valid := geometryValue(cache.dim, dim)
		// GGML quantized blocks include their scale bytes; never treat q8 as one byte.
		block, size := int64(1), dtypeBytes
		switch cache.dtype {
		case "":
		case "f16":
			size = 2
		case "f32":
			size = 4
		case "q8_0":
			block, size = 32, 34
		case "q4_0":
			block, size = 32, 18
		default:
			return 0, false
		}
		if !valid || dimension%block != 0 {
			return 0, false
		}
		part, err := needProduct([]int64{layerTokens, heads, dimension / block, size})
		if err != nil || part > math.MaxInt64-total {
			return 0, false
		}
		total += part
	}
	return total, true
}

func (cfg attentionConfig) dtypeBytes() (int64, bool) {
	dtype := cfg.TorchDtype
	if dtype == "" {
		dtype = cfg.Dtype
	}
	switch dtype {
	case "bfloat16", "float16":
		return 2, true
	case "float32":
		return 4, true
	}
	return 0, false
}

// layerTokens sums the tokens each attention layer caches. A linear-attention (recurrent) layer holds one state per
// concurrent sequence, sized by architecture-specific keys config.json does not standardise, so it makes the need unknown.
func (cfg attentionConfig) layerTokens(context int64) (int64, bool) {
	if cfg.LayerTypes == nil {
		// use_sliding_window: false (Qwen2 family) declares a window it does not apply.
		if cfg.SlidingWindow != nil && (cfg.UseSliding == nil || *cfg.UseSliding) {
			return 0, false
		}
		total, err := needProduct([]int64{cfg.Layers, context})
		return total, err == nil
	}
	if int64(len(cfg.LayerTypes)) != cfg.Layers {
		return 0, false
	}
	var total int64
	for _, layer := range cfg.LayerTypes {
		var tokens int64
		switch layer {
		case "full_attention":
			tokens = context
		case "sliding_attention":
			if cfg.SlidingWindow == nil || *cfg.SlidingWindow <= 0 {
				return 0, false
			}
			tokens = min(*cfg.SlidingWindow, context)
		default:
			return 0, false
		}
		if total > math.MaxInt64-tokens {
			return 0, false
		}
		total += tokens
	}
	return total, true
}

func weightsBytes(modelPath string, svc config.ServiceConfig) (int64, string, bool) {
	if modelPath == "" {
		return 0, "", false
	}
	files, err := layout.Weights(modelPath, featureGlobs(&svc))
	if err != nil || len(files) == 0 {
		return 0, "", false
	}
	var total int64
	for _, file := range files {
		if file.Size <= 0 || total > math.MaxInt64-file.Size {
			return 0, "", false
		}
		total += file.Size
	}
	header := ""
	if layout.IsGGUF(modelPath) {
		header = filepath.Join(filepath.Dir(modelPath), files[0].Path)
	}
	return total, header, true
}

func bytesToMiB(n int64) int64 {
	const mib = 1024 * 1024
	return n/mib + min(n%mib, 1)
}

func needProduct(values []int64) (int64, error) {
	product := int64(1)
	for _, value := range values {
		if value < 0 || (value > 0 && product > math.MaxInt64/value) {
			return 0, strconv.ErrRange
		}
		product *= value
	}
	return product, nil
}

func geometryValue(raw json.RawMessage, fallback int64) (int64, bool) {
	if len(raw) == 0 {
		return fallback, fallback > 0
	}
	var value *int64
	if json.Unmarshal(raw, &value) != nil || value == nil || *value <= 0 {
		return 0, false
	}
	return *value, true
}
