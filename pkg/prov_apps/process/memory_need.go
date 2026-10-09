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

// DefaultActivationOverheadMiB reserves 2 GiB per device for activations, CUDA graphs and linear-attention state.
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
	KVLoRARank    json.RawMessage `json:"kv_lora_rank"`
}

// ModelMemoryNeed returns unknown when local weights, context or attention geometry cannot be sized.
func ModelMemoryNeed(modelPath string, svc config.ServiceConfig, params map[string]string) (Need, bool) {
	contextKey := svc.GetContextParam()
	value := params[contextKey]
	if value == "" {
		value = params[strings.ReplaceAll(contextKey, "-", "_")]
	}
	context, err := strconv.ParseInt(value, 10, 64)
	if contextKey == "" || err != nil || context <= 0 || modelPath == "" {
		return Need{}, false
	}
	dir := (ResolvedModel{Path: modelPath}).Directory()
	if dir == "" {
		return Need{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return Need{}, false
	}
	var envelope struct {
		TextConfig json.RawMessage `json:"text_config"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return Need{}, false
	}
	if len(envelope.TextConfig) > 0 {
		data = envelope.TextConfig
	}
	var cfg attentionConfig
	if json.Unmarshal(data, &cfg) != nil || cfg.Layers <= 0 || cfg.Heads <= 0 || len(cfg.KVLoRARank) > 0 {
		return Need{}, false
	}
	heads, ok := geometryValue(cfg.KVHeads, cfg.Heads)
	if !ok {
		return Need{}, false
	}
	dim := int64(0)
	if cfg.HiddenSize > 0 && cfg.HiddenSize%cfg.Heads == 0 {
		dim = cfg.HiddenSize / cfg.Heads
	}
	dim, ok = geometryValue(cfg.HeadDim, dim)
	if !ok {
		return Need{}, false
	}
	dtype := cfg.TorchDtype
	if dtype == "" {
		dtype = cfg.Dtype
	}
	var dtypeBytes int64
	switch dtype {
	case "bfloat16", "float16":
		dtypeBytes = 2
	case "float32":
		dtypeBytes = 4
	default:
		return Need{}, false
	}
	var layerTokens int64
	if cfg.LayerTypes == nil {
		if cfg.SlidingWindow != nil {
			return Need{}, false
		}
		layerTokens, err = needProduct([]int64{cfg.Layers, context})
	} else {
		if int64(len(cfg.LayerTypes)) != cfg.Layers {
			return Need{}, false
		}
		for _, layer := range cfg.LayerTypes {
			var tokens int64
			switch layer {
			case "full_attention":
				tokens = context
			case "sliding_attention":
				if cfg.SlidingWindow == nil || *cfg.SlidingWindow <= 0 {
					return Need{}, false
				}
				tokens = min(*cfg.SlidingWindow, context)
			case "linear_attention":
			default:
				return Need{}, false
			}
			if layerTokens > math.MaxInt64-tokens {
				return Need{}, false
			}
			layerTokens += tokens
		}
	}
	if err != nil || heads <= 0 || dim <= 0 {
		return Need{}, false
	}
	kvBytes, err := needProduct([]int64{2, layerTokens, heads, dim, dtypeBytes})
	if err != nil {
		return Need{}, false
	}
	files, err := layout.Weights(modelPath, featureGlobs(&svc))
	if err != nil || len(files) == 0 {
		return Need{}, false
	}
	var weightBytes int64
	for _, file := range files {
		if file.Size <= 0 || weightBytes > math.MaxInt64-file.Size {
			return Need{}, false
		}
		weightBytes += file.Size
	}
	return Need{WeightsMiB: bytesToMiB(weightBytes), KVMiB: bytesToMiB(kvBytes), OverheadMiB: DefaultActivationOverheadMiB}, true
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
