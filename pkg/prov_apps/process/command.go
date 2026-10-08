package process

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/security"
)

// ErrModelNotLocal is returned when a provider must be pointed at local
// weights but the model is not in zzRouter's model store. Engines that take
// a bare name would silently download their own copy; failing here keeps the
// model store the single place weights live.
var ErrModelNotLocal = errors.New("model weights not found locally")

// CommandBuilder builds shell commands from ServiceConfig and instance parameters.
type CommandBuilder struct {
	serviceConfig *config.ServiceConfig
}

// NewCommandBuilder creates a command builder for the given service config.
func NewCommandBuilder(serviceConfig *config.ServiceConfig) *CommandBuilder {
	return &CommandBuilder{serviceConfig: serviceConfig}
}

// Launch is a fully resolved provider launch: the argv to exec plus the
// model identity the rest of the system needs in order to talk to the
// engine over the wire.
type Launch struct {
	Command string
	Args    []string

	// ModelPath is the absolute path to the model's local weights, or
	// empty when the provider's args never reference them.
	ModelPath string

	// WireModel is the token this engine keys on in the "model" field of
	// wire payloads — the canonical name for most engines, the weights
	// path for config.WireModelPath engines.
	WireModel string
}

// Build resolves an admitted model into an argv, expanding the
// launch placeholders (see config.PlaceholderModelPath and friends) and
// appending runtime parameters as CLI flags.
//
// An engine that references ${MODEL_PATH} must be pointed at weights that
// exist: resolution failure is a launch failure here rather than an opaque
// "no such file" from the engine several seconds later.
func (cb *CommandBuilder) Build(model string, port int, params map[string]string) (Launch, error) {
	if cb.serviceConfig == nil || cb.serviceConfig.Runtime == nil {
		return Launch{}, fmt.Errorf("no runtime execution config")
	}

	exec := &cb.serviceConfig.Runtime.Execution
	if exec.Command == "" {
		return Launch{}, fmt.Errorf("no command configured")
	}

	// The auto_deploy chain appends "#variant" to carry the resolved file
	// through to the launcher; it addresses weights, never the name.
	modelClean := model
	if i := strings.Index(modelClean, "#"); i > 0 {
		modelClean = modelClean[:i]
	}

	// A variant runs its base's weights under its own name: ${MODEL} is
	// the name, ${MODEL_PATH} the weights.
	var modelPath string
	if argsReferenceModelPath(exec.Args) {
		weights := cb.serviceConfig.WeightsOf(model)
		resolved, ok := resolveModelToPath(weights, featureGlobs(cb.serviceConfig)...)
		if !ok {
			return Launch{}, fmt.Errorf("%w: %q is not in the local model store", ErrModelNotLocal, weights)
		}
		modelPath = resolved
	}

	args := make([]string, 0, len(exec.Args))
	for _, arg := range exec.Args {
		arg = strings.ReplaceAll(arg, config.PlaceholderPort, strconv.Itoa(port))
		arg = strings.ReplaceAll(arg, config.PlaceholderModelPath, modelPath)
		arg = strings.ReplaceAll(arg, config.PlaceholderModel, modelClean)
		arg = security.ExpandEnvWhitelisted(arg)
		args = append(args, arg)
	}

	if len(params) > 0 {
		args = append(args, buildParameterArgs(params, cb.excludedParams())...)
	}

	wireModel := modelClean
	if exec.WireModel.Resolved() == config.WireModelPath {
		wireModel = modelPath
	}

	return Launch{
		Command:   exec.Command,
		Args:      args,
		ModelPath: modelPath,
		WireModel: wireModel,
	}, nil
}

// ValidateVariantModel refuses local weights that occupy a variant name.
// This lookup is uncached: downloads can arrive before a catalog refresh.
// It also applies to engines that accept a bare name instead of MODEL_PATH.
func ValidateVariantModel(sc *config.ServiceConfig, model string) error {
	if sc == nil || len(sc.Variants()) == 0 {
		return nil
	}
	if name, _, ok := sc.Variant(model); ok {
		if _, found := resolveModelToPath(name, featureGlobs(sc)...); found {
			return &config.ModelNameConflictError{Model: name}
		}
		return nil
	}
	// A new repo/path alias may not be in the catalog yet. Check the
	// physical identity it resolves to instead of trusting the raw token.
	if path, found := resolveModelToPath(model, featureGlobs(sc)...); found {
		file := filepath.Base(path)
		stem := strings.TrimSuffix(file, filepath.Ext(file))
		logical := logicalWeightsName(path, featureGlobs(sc)...)
		for name := range sc.Variants() {
			if strings.EqualFold(name, file) || strings.EqualFold(name, stem) || strings.EqualFold(name, logical) {
				return &config.ModelNameConflictError{Model: name}
			}
		}
	}
	return nil
}

// logicalWeightsName uses the same complete GGUF identity as scans and launches.
// Feature files and incomplete shard sets do not reserve weights names.
func logicalWeightsName(path string, excluded ...string) string {
	if layout.Matches(filepath.Base(path), excluded) {
		return ""
	}
	if variants := layout.GGUFVariants([]layout.File{{Path: filepath.Base(path)}}); len(variants) == 1 {
		return variants[0].Name
	}
	files, err := layout.OnDisk(filepath.Dir(path), excluded...)
	if err != nil {
		return ""
	}
	for _, variant := range layout.GGUFVariants(files) {
		for _, file := range variant.Files {
			if filepath.Join(filepath.Dir(path), filepath.FromSlash(file.Path)) == path {
				return variant.Name
			}
		}
	}
	return ""
}

// argsReferenceModelPath reports whether any arg needs the resolved weights.
func argsReferenceModelPath(args []string) bool {
	for _, a := range args {
		if strings.Contains(a, config.PlaceholderModelPath) {
			return true
		}
	}
	return false
}

// resolveModelToPath resolves a model name to the absolute path of its
// local weights, reporting whether the weights are actually on disk.
//
// Deliberately uncached: downloads land while the server runs, so a cached
// "not there yet" (or a directory cached before its files arrived) would
// outlive the fact it described. The filesystem walk only runs when the
// direct path misses, which is the uncommon case.
func resolveModelToPath(model string, excluded ...string) (string, bool) {
	// Split optional "#filename" or "#pattern" suffix. The auto_deploy chain
	// appends one to carry the resolved variant filename through to the
	// launcher (e.g. "Qwen/Qwen2.5-0.5B-Instruct-GGUF#qwen2.5-0.5b-instruct-q4_k_m.gguf"
	// or "Qwen/...-GGUF#Q4_K_M" when the deploy side passed a pattern).
	repo, hint := model, ""
	if i := strings.Index(model, "#"); i > 0 {
		repo, hint = model[:i], model[i+1:]
	}

	// Already an absolute path that exists
	if filepath.IsAbs(repo) {
		if _, err := os.Stat(repo); err == nil {
			resolved := resolveDirToFile(repo, hint, excluded...)
			return resolved, resolved != ""
		}
	}

	modelsRoot, err := modelregistry.GetModelsRootDir()
	if err != nil {
		return "", false
	}

	// Direct path under models root
	direct := filepath.Join(modelsRoot, repo)
	if _, err := os.Stat(direct); err == nil {
		resolved := resolveDirToFile(direct, hint, excluded...)
		return resolved, resolved != ""
	}

	// Walk models root looking for a matching file or directory by base name
	var match string
	_ = filepath.WalkDir(modelsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || match != "" {
			return filepath.SkipDir
		}
		base := d.Name()
		nameNoExt := strings.TrimSuffix(base, filepath.Ext(base))
		if strings.EqualFold(base, repo) || strings.EqualFold(nameNoExt, repo) ||
			(!d.IsDir() && layout.IsGGUF(base) && strings.HasPrefix(strings.ToLower(nameNoExt), strings.ToLower(repo)+"-") && strings.EqualFold(logicalWeightsName(path, excluded...), repo)) {
			match = path
			return filepath.SkipAll
		}
		return nil
	})
	if match != "" {
		resolved := resolveDirToFile(match, hint, excluded...)
		return resolved, resolved != ""
	}

	return "", false
}

// resolveDirToFile narrows a model directory to the weights file an engine
// is pointed at: the GGUF variant hint names (layout.Pick), else the only
// one. A split variant resolves to its first shard. Feature files never
// count as weights. A directory with no GGUF, or with several and no hint
// that picks one, is returned as is.
func resolveDirToFile(path, hint string, excluded ...string) string {
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		if layout.Matches(filepath.Base(path), excluded) {
			return ""
		}
		return path
	}
	files, err := layout.OnDisk(path, excluded...)
	if err != nil {
		return path
	}
	variants := layout.GGUFVariants(files)
	v, ok := layout.Pick(variants, hint)
	if !ok {
		v, ok = layout.Pick(variants, "")
	}
	if !ok {
		return path
	}
	return filepath.Join(path, filepath.FromSlash(v.Files[0].Path))
}

// excludedParams is retired; the exclusion blocklist is gone per the v5 plan.
func (cb *CommandBuilder) excludedParams() []string {
	return nil
}

// buildParameterArgs converts a parameter map to CLI flag arguments.
// Parameters named in excluded are skipped. Boolean "true" values emit
// a flag-only argument (--flag), "false" values are omitted entirely.
// JSON string values are normalized (numeric strings → numbers).
func buildParameterArgs(params map[string]string, excluded []string) []string {
	if len(params) == 0 {
		return nil
	}

	excludedSet := make(map[string]bool, len(excluded))
	for _, e := range excluded {
		excludedSet[e] = true
	}

	args := make([]string, 0, len(params)*2)
	for key, value := range params {
		if excludedSet[key] {
			continue
		}

		// Normalize JSON string values to proper types
		if looksLikeJSON(value) {
			value = normalizeJSONNumbers(value)
		}

		// Convert parameter_name to --parameter-name
		flag := "--" + strings.ReplaceAll(key, "_", "-")
		if value == "" || value == "true" {
			args = append(args, flag)
		} else if value == "false" {
			continue // skip boolean false flags
		} else {
			args = append(args, flag, value)
		}
	}
	return args
}

// looksLikeJSON returns true if the string appears to be a JSON object or array.
func looksLikeJSON(s string) bool {
	s = strings.TrimSpace(s)
	return (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) ||
		(strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"))
}

// normalizeJSONNumbers converts string-encoded numbers in JSON to actual numbers.
func normalizeJSONNumbers(jsonStr string) string {
	var data any
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return jsonStr
	}
	data = convertNumericStrings(data)
	normalized, err := json.Marshal(data)
	if err != nil {
		return jsonStr
	}
	return string(normalized)
}

func convertNumericStrings(v any) any {
	switch val := v.(type) {
	case map[string]any:
		result := make(map[string]any, len(val))
		for k, v := range val {
			result[k] = convertNumericStrings(v)
		}
		return result
	case []any:
		result := make([]any, len(val))
		for i, v := range val {
			result[i] = convertNumericStrings(v)
		}
		return result
	case string:
		if num, err := strconv.ParseFloat(val, 64); err == nil {
			if num == float64(int64(num)) {
				return int64(num)
			}
			return num
		}
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
		return val
	default:
		return val
	}
}

func featureGlobs(svc *config.ServiceConfig) []string {
	var globs []string
	for _, feature := range svc.Features {
		globs = append(globs, feature.Files...)
	}
	return globs
}

// ModelDirectory resolves base weights through the same local lookup as launch.
func ModelDirectory(svc config.ServiceConfig, model string) (string, bool) {
	p, ok := resolveModelToPath(svc.WeightsOf(model), featureGlobs(&svc)...)
	if !ok {
		return "", false
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return p, true
	}
	if dir, found := manifestDir(p); found {
		return dir, true
	}
	return filepath.Dir(p), true
}
