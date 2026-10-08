package config

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/pkg/security"
)

// Launch placeholders. CommandBuilder expands exactly these tokens in
// execution.args; anything else would reach the engine's argv verbatim.
const (
	// PlaceholderPort expands to the port allocated for the instance.
	PlaceholderPort = "${PORT}"

	// PlaceholderModelPath expands to the absolute path of the model's
	// local weights. This is the only way to point an engine at weights —
	// passing a bare name lets the engine resolve it against its own
	// registry cache and download a second copy.
	PlaceholderModelPath = "${MODEL_PATH}"

	// PlaceholderModel expands to the canonical model name. Naming only
	// (--served-model-name, --alias); never weights.
	PlaceholderModel = "${MODEL}"
)

// launchPlaceholders is the closed vocabulary CommandBuilder expands.
var launchPlaceholders = map[string]bool{
	PlaceholderPort:      true,
	PlaceholderModelPath: true,
	PlaceholderModel:     true,
}

// WireModel names what an inference engine keys on — and echoes back — in
// the "model" field of OpenAI-wire payloads.
type WireModel string

const (
	// WireModelName: the engine honors the client-facing model name,
	// either because it takes a naming flag (vLLM --served-model-name,
	// llama.cpp --alias) or because it ignores the field entirely.
	WireModelName WireModel = "name"

	// WireModelPath: the engine loads weights by whatever token the
	// request carries, so it must receive the same path it was launched
	// with (mlx_lm maps only the literal "default_model" to its --model
	// argument; any other value is loaded as a fresh path or repo id).
	// zzRouter sends the path inbound and restores the client-facing name
	// on the way out.
	WireModelPath WireModel = "path"
)

// Resolved returns the wire model with the default applied. Most engines
// honor the client's name, so that is the zero value's meaning.
func (w WireModel) Resolved() WireModel {
	if w == "" {
		return WireModelName
	}
	return w
}

// validateExecutionPlaceholders enforces the launch-token contract on an
// execution config:
//
//  1. Every ${TOKEN} is either a launch placeholder or an allow-listed
//     environment variable. An unknown token is a typo that would
//     otherwise be passed to the engine literally.
//  2. An engine that is told about the model at all must be pointed at
//     local weights via ${MODEL_PATH}. ${MODEL} alone means the engine
//     resolves the name itself, which bypasses zzRouter's model store.
//  3. wire_model is one of the known values.
func validateExecutionPlaceholders(providerName string, exec ExecutionConfig) error {
	usesModelPath := false
	usesModel := false

	for i, arg := range exec.Args {
		for _, token := range execArgTemplateRegex.FindAllString(arg, -1) {
			switch {
			case token == PlaceholderModelPath:
				usesModelPath = true
			case token == PlaceholderModel:
				usesModel = true
			case launchPlaceholders[token]:
			case security.AllowedEnvVars[strings.TrimSuffix(strings.TrimPrefix(token, "${"), "}")]:
			default:
				return fmt.Errorf("provider %q: execution.args[%d] uses unknown placeholder %s", providerName, i, token)
			}
		}
	}

	if usesModel && !usesModelPath {
		return fmt.Errorf("provider %q: execution.args names the model with %s but never points the engine at local weights with %s",
			providerName, PlaceholderModel, PlaceholderModelPath)
	}

	switch exec.WireModel.Resolved() {
	case WireModelName, WireModelPath:
	default:
		return fmt.Errorf("provider %q: execution.wire_model %q must be %q or %q",
			providerName, exec.WireModel, WireModelName, WireModelPath)
	}

	return nil
}
