// Package schema carries the launcher-load-bearing parameter spine for each
// provider. The Go Registry is canonical for *shape* (type + range); long-tail
// flags and policy-only metadata live in per-provider schema.yaml files
// loaded at runtime. A conflict between the two on shape is a startup error —
// the Go claim wins because it encodes structural correctness (plan §4.4).
package schema

// ParamKind is the typed shape category a parameter resolves to. Launcher
// coercion (today string-only) will key off this when typed values land.
type ParamKind string

const (
	ParamInt    ParamKind = "int"
	ParamFloat  ParamKind = "float"
	ParamString ParamKind = "string"
	ParamBool   ParamKind = "bool"
	// ParamAsset holds the name of one of the provider's asset files
	// (pkg/config/assets); the node that launches resolves it to a path.
	ParamAsset ParamKind = "asset"
)

// AssetPass is how an asset parameter reaches the engine. Most engines
// take a file (llama-server --chat-template-file); some take the text
// itself (mlx_lm.server --chat-template sets the tokenizer's
// chat_template to its argument verbatim).
type AssetPass string

const (
	// AssetPassPath hands the engine the asset's absolute path on the
	// launching node. It is what an empty Pass means.
	AssetPassPath AssetPass = "path"
	// AssetPassContent hands the engine the asset's bytes as the flag's
	// value.
	AssetPassContent AssetPass = "content"
)

// ParamShape is the Go-side declaration for a single parameter. Min/Max
// are optional; when set they constrain Kind==int/float validation. An
// empty Kind means "not structurally gated by Go" — only YAML policy
// applies.
type ParamShape struct {
	Kind        ParamKind
	Min         *float64
	Max         *float64
	Description string
	Enum        []string
	// Pass applies to ParamAsset only; empty means AssetPassPath.
	Pass AssetPass
}

// Registry is the Go spine: launcher-load-bearing params per provider.
// Keep this list narrow — every entry here is structural. Flags whose
// absence is a missing-feature (not a correctness bug) belong in
// schema.yaml instead.
var Registry = map[string]map[string]ParamShape{
	"vllm": {
		"max-model-len":          {Kind: ParamInt, Min: fptr(1), Description: "Maximum model context length in tokens"},
		"gpu-memory-utilization": {Kind: ParamFloat, Min: fptr(0), Max: fptr(1), Description: "Fraction of GPU memory allocated to model weights and activations"},
		"tensor-parallel-size":   {Kind: ParamInt, Min: fptr(1), Description: "Number of GPUs for tensor parallelism"},
		"dtype":                  {Kind: ParamString, Description: "Model weight dtype (auto|bf16|fp16|fp32)"},
	},
	"llamacpp": {
		"ctx-size":     {Kind: ParamInt, Min: fptr(1), Description: "Context window size in tokens"},
		"n-gpu-layers": {Kind: ParamInt, Min: fptr(0), Description: "Number of layers to offload to GPU"},
	},
	"mlx": {
		"max-tokens": {Kind: ParamInt, Min: fptr(1), Description: "Maximum generation length"},
	},
	"ollama": {
		"num-ctx": {Kind: ParamInt, Min: fptr(1), Description: "Context window size in tokens"},
	},
}

// Lookup returns the Go-spine shape for (provider, param), or false.
func Lookup(provider, param string) (ParamShape, bool) {
	if m, ok := Registry[provider]; ok {
		s, ok := m[param]
		return s, ok
	}
	return ParamShape{}, false
}

// KnownProvider reports whether the Go spine knows the provider at all.
func KnownProvider(provider string) bool {
	_, ok := Registry[provider]
	return ok
}

func fptr(v float64) *float64 { return &v }
