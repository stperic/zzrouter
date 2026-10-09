package process

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/layout"
)

// DefaultActivationOverheadMiB reserves 2 GiB per device for activations and CUDA graphs.
const DefaultActivationOverheadMiB int64 = 2048

// Need is the model's weights, one context sequence's KV cache and per-device runtime overhead.
type Need struct {
	WeightsMiB, KVMiB, OverheadMiB int64
}

// Total returns the single-device memory need; zero denotes unknown.
func (n Need) Total() int64 { return n.WeightsMiB + n.KVMiB + n.OverheadMiB }

type attentionConfig struct {
	Layers        int64           `json:"num_hidden_layers"`
	KVHeads       json.RawMessage `json:"num_key_value_heads"`
	Heads         int64           `json:"num_attention_heads"`
	HeadDim       json.RawMessage `json:"head_dim"`
	HiddenSize    int64           `json:"hidden_size"`
	TorchDtype    string          `json:"torch_dtype"`
	Dtype         string          `json:"dtype"`
	LayerTypes    []string        `json:"layer_types"`
	SlidingWindow *int64          `json:"sliding_window"`
	UseSliding    *bool           `json:"use_sliding_window"`
	KVLoRARank    json.RawMessage `json:"kv_lora_rank"`
}

// ModelMemoryNeed returns unknown when local weights, context or attention geometry cannot be sized.
func ModelMemoryNeed(modelPath string, svc config.ServiceConfig, params map[string]string) (Need, bool) {
	context, ok := contextLength(svc, params)
	if !ok || modelPath == "" {
		return Need{}, false
	}
	cfg, ok := readAttentionConfig((ResolvedModel{Path: modelPath}).Directory())
	if !ok {
		return Need{}, false
	}
	kvBytes, ok := cfg.kvBytes(context)
	if !ok {
		return Need{}, false
	}
	weightBytes, ok := weightsBytes(modelPath, svc)
	if !ok {
		return Need{}, false
	}
	return Need{WeightsMiB: bytesToMiB(weightBytes), KVMiB: bytesToMiB(kvBytes), OverheadMiB: DefaultActivationOverheadMiB}, true
}

// contextLength reads the provider's context parameter, under either spelling a resolved key may carry.
func contextLength(svc config.ServiceConfig, params map[string]string) (int64, bool) {
	key := svc.GetContextParam()
	if key == "" {
		return 0, false
	}
	value := params[key]
	if value == "" {
		value = params[strings.ReplaceAll(key, "-", "_")]
	}
	context, err := strconv.ParseInt(value, 10, 64)
	return context, err == nil && context > 0
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
	kv, err := needProduct([]int64{2, layerTokens, heads, dim, dtypeBytes})
	return kv, err == nil
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

func weightsBytes(modelPath string, svc config.ServiceConfig) (int64, bool) {
	files, err := layout.Weights(modelPath, featureGlobs(&svc))
	if err != nil || len(files) == 0 {
		return 0, false
	}
	var total int64
	for _, file := range files {
		if file.Size <= 0 || total > math.MaxInt64-file.Size {
			return 0, false
		}
		total += file.Size
	}
	return total, true
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
