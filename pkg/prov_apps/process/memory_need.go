package process

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/gguf"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// DefaultActivationOverheadMiB is the engine's in-budget memory beyond weights and KV (activations, CUDA graphs,
// encoder profiling). Lab, vLLM on localai: gemma-4-E2B used 2.9 GiB (3 video items profiled), Qwen2.5-0.5B under 2.
const DefaultActivationOverheadMiB int64 = 4096

// Need is the model's weights, cache allocation and per-device runtime overhead.
// KVMiB includes attention KV and recurrent states, rounded together.
type Need struct {
	WeightsMiB, KVMiB, OverheadMiB int64
	WeightsBytes                   int64 // Exact bytes prevent false refusals at fractional MiB boundaries.
	UnknownCause                   UnknownCause
	ContextMode                    string
	ContextTokens                  int64
	nativeContext                  bool
	geometry                       *attentionConfig
}

// Total returns the single-device memory need; zero denotes unknown.
func (n Need) Total() int64 {
	if n.UnknownCause != "" || n.WeightsMiB > math.MaxInt64-n.KVMiB || n.WeightsMiB+n.KVMiB > math.MaxInt64-n.OverheadMiB {
		return 0
	}
	return n.WeightsMiB + n.KVMiB + n.OverheadMiB
}

type attentionConfig struct {
	LinearKernel   int64           `json:"linear_conv_kernel_dim"`
	LinearKHeads   int64           `json:"linear_num_key_heads"`
	LinearVHeads   int64           `json:"linear_num_value_heads"`
	LinearKDim     int64           `json:"linear_key_head_dim"`
	LinearVDim     int64           `json:"linear_value_head_dim"`
	StateDtype     json.RawMessage `json:"mamba_ssm_dtype"`
	FullInterval   *int64          `json:"full_attention_interval"`
	stateRows      int64
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
func ModelMemoryNeed(model ResolvedModel, svc config.ServiceConfig, params map[string]string, policy schema.AutoContext, shapes map[string]schema.ParamShape) (Need, bool) {
	weightBytes, header, ok := weightsBytes(model.Path, svc)
	partial := Need{WeightsMiB: bytesToMiB(weightBytes), WeightsBytes: weightBytes, UnknownCause: WeightsUnavailable, ContextMode: "unknown"}
	value := params[svc.GetContextParam()]
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
	cfg, cause := readAttentionConfig(model.Directory())
	if header != "" && (cause != "" || policy.AutoContextSource == "native") {
		cfg, cause = readGGUFConfig(header)
	}
	if partial.ContextMode == "auto" && policy.AutoContextSource == "native" {
		partial.nativeContext = true
		context = cfg.NativeContext
	}
	partial.ContextTokens = context
	if !ok {
		return partial, false
	}
	partial.UnknownCause = cause
	if cause != "" {
		return partial, false
	}
	partial.UnknownCause = ContextUnresolved
	if context <= 0 || (partial.ContextMode == "auto" && len(cfg.RopeScaling) > 0 && string(cfg.RopeScaling) != "null") {
		return partial, false
	}
	if partial.ContextMode == "auto" && cfg.NativeContext < context {
		if cfg.NativeContext > 0 {
			partial.UnknownCause = ContextBelowMinimum
		}
		return partial, false
	}
	if policy.RecurrentStateRows != nil {
		cfg.stateRows = *policy.RecurrentStateRows
	}
	if partial.UnknownCause = cfg.applyCacheParameters(params, shapes, partial.ContextMode == "auto"); partial.UnknownCause != "" {
		return partial, false
	}

	partial.geometry = &cfg
	sized, complete := partial.atContext(context)
	if !complete {
		partial.UnknownCause = sized.UnknownCause
		partial.geometry = nil
		return partial, false
	}
	return sized, true
}

// applyCacheParameters interprets sizing roles after ordinary parameter resolution.
func (cfg *attentionConfig) applyCacheParameters(params map[string]string, shapes map[string]schema.ParamShape, auto bool) UnknownCause {
	for name, shape := range shapes {
		value := params[name]
		if shape.SequenceSlots && shape.RecurrentStateRows != nil {
			cfg.stateRows = *shape.RecurrentStateRows
			// auto leaves the slot count to the engine, which the declared default rows describe.
			if value != "" && !config.IsAuto(value) {
				// An unparsable count leaves zero rows, which only a recurrent layer refuses to size.
				cfg.stateRows, _ = strconv.ParseInt(value, 10, 64)
			}
		}
		if shape.ContextOverride && value != "" && auto {
			return ContextUnresolved
		}
		if shape.KVCacheDtype == "k" || shape.KVCacheDtype == "both" {
			cfg.CacheK = value
		}
		if shape.KVCacheDtype == "v" || shape.KVCacheDtype == "both" {
			cfg.CacheV = value
		}
	}
	return ""
}

func (n Need) atContext(context int64) (Need, bool) {
	kv, cause := n.geometry.kvBytes(context)
	n.KVMiB, n.OverheadMiB, n.UnknownCause = bytesToMiB(kv), DefaultActivationOverheadMiB, cause
	if cause == "" && n.Total() == 0 {
		n.UnknownCause = ModelUnmodelled
	}
	return n, n.UnknownCause == ""
}

func readGGUFConfig(path string) (attentionConfig, UnknownCause) {
	file, err := os.Open(path)
	if err != nil {
		return attentionConfig{}, MetadataInvalid
	}
	defer file.Close()
	h, err := gguf.Read(file)
	if err != nil {
		if errors.Is(err, gguf.ErrUnsupported) {
			return attentionConfig{}, ModelUnmodelled
		}
		return attentionConfig{}, MetadataInvalid
	}
	v := func(key string) int64 { return h.Values[h.Architecture+"."+key] }
	raw := func(n int64) json.RawMessage {
		if n == 0 {
			return nil
		}
		return json.RawMessage(strconv.FormatInt(n, 10))
	}
	cfg := attentionConfig{Layers: v("block_count"), NativeContext: v("context_length"), Heads: v("attention.head_count"), KVHeads: raw(v("attention.head_count_kv")), HiddenSize: v("embedding_length"), KeyDim: raw(v("attention.key_length")), ValueDim: raw(v("attention.value_length")), Dtype: "float16"}
	if h.Architecture == "qwen35" {
		cfg.Layers -= v("nextn_predict_layers")
		interval := v("full_attention_interval")
		cfg.FullInterval, cfg.LinearKernel = &interval, v("ssm.conv_kernel")
		cfg.LinearKHeads, cfg.LinearVHeads, cfg.LinearKDim = v("ssm.group_count"), v("ssm.time_step_rank"), v("ssm.state_size")
		inner := v("ssm.inner_size")
		if *cfg.FullInterval <= 0 || cfg.LinearVHeads <= 0 || inner <= 0 || inner%cfg.LinearVHeads != 0 {
			return cfg, MetadataInvalid
		}
		cfg.LinearVDim = inner / cfg.LinearVHeads
	}
	if cfg.Layers <= 0 || cfg.Heads <= 0 {
		return cfg, MetadataInvalid
	}
	if v("attention.sliding_window") != 0 {
		return cfg, ModelUnmodelled
	}
	return cfg, ""
}

// readAttentionConfig reads the language model's config.json, nested under text_config in multimodal models.
func readAttentionConfig(dir string) (attentionConfig, UnknownCause) {
	if dir == "" {
		return attentionConfig{}, MetadataInvalid
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return attentionConfig{}, MetadataInvalid
	}
	var envelope struct {
		TextConfig json.RawMessage `json:"text_config"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return attentionConfig{}, MetadataInvalid
	}
	if len(envelope.TextConfig) > 0 {
		data = envelope.TextConfig
	}
	var cfg attentionConfig
	if json.Unmarshal(data, &cfg) != nil || cfg.Layers <= 0 || cfg.Heads <= 0 {
		return attentionConfig{}, MetadataInvalid
	}
	if len(cfg.KVLoRARank) > 0 || (cfg.FullInterval != nil && cfg.LayerTypes == nil) {
		return cfg, ModelUnmodelled
	}
	return cfg, ""
}

// kvBytes sizes attention KV plus fixed float32 recurrent states at the declared row count.
func (cfg attentionConfig) kvBytes(context int64) (int64, UnknownCause) {
	heads, ok := geometryValue(cfg.KVHeads, cfg.Heads)
	if !ok || cfg.Heads <= 0 || cfg.Heads%heads != 0 {
		return 0, MetadataInvalid
	}
	dim := int64(0)
	if cfg.HiddenSize > 0 && cfg.HiddenSize%cfg.Heads == 0 {
		dim = cfg.HiddenSize / cfg.Heads
	}
	if len(cfg.KeyDim) == 0 || len(cfg.ValueDim) == 0 {
		if dim, ok = geometryValue(cfg.HeadDim, dim); !ok {
			return 0, MetadataInvalid
		}
	}
	dtypeBytes, ok := cfg.dtypeBytes()
	if !ok {
		return 0, ModelUnmodelled
	}
	layerTokens, recurrent, cause := cfg.layerTokens(context)
	if cause != "" {
		return 0, cause
	}
	total, cause := cfg.recurrentBytes(recurrent)
	if cause != "" {
		return 0, cause
	}
	for _, cache := range []struct {
		dim   json.RawMessage
		dtype string
	}{{cfg.KeyDim, cfg.CacheK}, {cfg.ValueDim, cfg.CacheV}} {
		dimension, valid := geometryValue(cache.dim, dim)
		// GGML quantized blocks include their scale bytes; never treat q8 as one byte.
		block, size := int64(1), dtypeBytes
		switch cache.dtype {
		case "", "auto":
		case "f16", "bf16", "float16", "bfloat16":
			size = 2
		case "f32":
			size = 4
		case "fp8", "fp8_e4m3", "fp8_e5m2":
			size = 1
		case "q8_0":
			block, size = 32, 34
		case "q4_0", "iq4_nl":
			block, size = 32, 18
		case "q4_1":
			block, size = 32, 20
		case "q5_0":
			block, size = 32, 22
		case "q5_1":
			block, size = 32, 24
		default:
			return 0, KVDtypeUnsupported
		}
		if !valid {
			return 0, MetadataInvalid
		}
		if dimension%block != 0 {
			return 0, KVDtypeUnsupported
		}
		part, err := needProduct([]int64{layerTokens, heads, dimension / block, size})
		if err != nil || part > math.MaxInt64-total {
			return 0, ModelUnmodelled
		}
		total += part
	}
	return total, ""
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

// layerTokens sums attention tokens separately from recurrent layers.
func (cfg attentionConfig) layerTokens(context int64) (int64, int64, UnknownCause) {
	if cfg.FullInterval != nil && *cfg.FullInterval <= 0 {
		return 0, 0, MetadataInvalid
	}
	if cfg.LayerTypes == nil {
		// use_sliding_window: false (Qwen2 family) declares a window it does not apply.
		if cfg.SlidingWindow != nil && (cfg.UseSliding == nil || *cfg.UseSliding) {
			return 0, 0, ModelUnmodelled
		}
		layers := cfg.Layers
		if cfg.FullInterval != nil {
			layers /= *cfg.FullInterval
		}
		total, err := needProduct([]int64{layers, context})
		if err != nil {
			return 0, 0, ModelUnmodelled
		}
		return total, cfg.Layers - layers, ""
	}
	if int64(len(cfg.LayerTypes)) != cfg.Layers {
		return 0, 0, MetadataInvalid
	}
	var total, recurrent int64
	for i, layer := range cfg.LayerTypes {
		if cfg.FullInterval != nil && (((int64(i)+1)%*cfg.FullInterval == 0) != (layer == "full_attention") || (layer != "full_attention" && layer != "linear_attention")) {
			return 0, 0, MetadataInvalid
		}
		var tokens int64
		switch layer {
		case "linear_attention":
			recurrent++
		case "full_attention":
			tokens = context
		case "sliding_attention":
			if cfg.SlidingWindow == nil || *cfg.SlidingWindow <= 0 {
				return 0, 0, MetadataInvalid
			}
			tokens = min(*cfg.SlidingWindow, context)
		default:
			return 0, 0, ModelUnmodelled
		}
		if total > math.MaxInt64-tokens {
			return 0, 0, ModelUnmodelled
		}
		total += tokens
	}
	return total, recurrent, ""
}

func (cfg attentionConfig) recurrentBytes(layers int64) (int64, UnknownCause) {
	if layers == 0 {
		return 0, ""
	}
	var dtype string
	if len(cfg.StateDtype) > 0 && (string(cfg.StateDtype) == "null" || json.Unmarshal(cfg.StateDtype, &dtype) != nil) {
		return 0, ModelUnmodelled
	}
	switch dtype {
	case "", "float16", "bfloat16", "float32":
	default:
		return 0, ModelUnmodelled
	}
	if cfg.LinearKernel < 1 || cfg.LinearKHeads < 1 || cfg.LinearVHeads < 1 || cfg.LinearVHeads%cfg.LinearKHeads != 0 || cfg.LinearKDim < 1 || cfg.LinearVDim < 1 {
		return 0, MetadataInvalid
	}
	if cfg.stateRows < 1 || cfg.stateRows > schema.MaxRecurrentStateRows {
		return 0, ModelUnmodelled
	}
	k, ek := needProduct([]int64{2, cfg.LinearKHeads, cfg.LinearKDim})
	v, ev := needProduct([]int64{cfg.LinearVHeads, cfg.LinearVDim})
	if ek != nil || ev != nil || k > math.MaxInt64-v {
		return 0, ModelUnmodelled
	}
	conv, ec := needProduct([]int64{cfg.LinearKernel - 1, k + v, 4})
	ssm, es := needProduct([]int64{v, cfg.LinearKDim, 4})
	if ec != nil || es != nil || conv > math.MaxInt64-ssm {
		return 0, ModelUnmodelled
	}
	total, err := needProduct([]int64{conv + ssm, layers, cfg.stateRows})
	if err != nil {
		return 0, ModelUnmodelled
	}
	return total, ""
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
