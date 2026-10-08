package modelregistry

import (
	"strings"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

// Identifier represents a parsed identifier that can contain various combinations of:
// host, provider key, repository/source, model name, and tag
type Identifier struct {
	Node        string // Network location (hostname, IP, port) - can be empty
	ProviderKey string // Provider key from YAML (e.g., "llamacpp", "ollama", "vllm") - stable identifier
	Repository  string // Model source/registry (hf, ollama, gguf, local) - can be empty
	Namespace   string // Publisher within the registry (e.g., "meta-llama", "bartowski", "anthropic") - can be empty
	ModelName   string // Model name (can include namespace like meta-llama/Llama-3) - can be empty
	Tag         string // Version/variant (e.g., latest, 8b, q5_K_M) - can be empty
	File        string // GGUF variant file suffix after '#' (e.g., "Q4_K_M", "model.Q4_K_M.gguf") - can be empty
	Raw         string // Original input string
}

// ParseOpts carries optional hints for Parse.
// Zero value is valid — behaves as a best-effort parse with no config lookup.
type ParseOpts struct {
	Config   *pkgConfig.AppsConfig // dynamic provider/registry lookup
	Registry string                // registry hint; cloud → opaque parse
}

// Parse converts a model identifier string into structured components.
//
// Grammar: <repo-or-model>[:tag][#file][@node]  (with :: as alternate node separator)
//
// With opts.Registry pointing to a cloud provider (per opts.Config), the parser
// treats the input as opaque: it returns the input as Identifier.ModelName and
// opts.Registry as Identifier.Repository, skipping @, #, :, and / splitting.
// Rationale: cloud model IDs reuse these separators with different semantics
// (e.g., "@cf/meta/llama-3", "openai/gpt-4o-mini:2024-07-18").
//
// Without opts.Registry, if the first path component is a known cloud registry
// in opts.Config, the parser auto-short-circuits the same way.
func Parse(input string, opts ParseOpts) (*Identifier, error) {
	original := input
	input = strings.TrimSpace(input)

	if input == "" {
		return &Identifier{Raw: original}, nil
	}

	id := &Identifier{Raw: original}

	// Cloud short-circuit: explicit registry hint wins.
	if opts.Registry != "" && isCloudRegistry(opts.Config, opts.Registry) {
		id.Repository = opts.Registry
		id.ModelName = strings.TrimPrefix(input, opts.Registry+"/")
		id.Namespace = extractNamespace(opts.Registry, id.ModelName)
		return id, nil
	}

	// Cloud auto-detect: first path component matches a known cloud registry.
	if opts.Config != nil {
		if slash := strings.Index(input, "/"); slash > 0 {
			first := input[:slash]
			if isCloudRegistry(opts.Config, first) {
				id.Repository = first
				id.ModelName = input[slash+1:]
				id.Namespace = extractNamespace(first, id.ModelName)
				return id, nil
			}
		}
	}

	// Full-parse path below.
	if strings.Contains(input, "::") && strings.Contains(input, "@") {
		return nil, ErrIdentifierMixedSeparators
	}

	var fullIdentifier string

	if strings.Contains(input, "::") {
		parts := strings.SplitN(input, "::", 2)
		id.Node = parts[0]
		if len(parts) > 1 {
			fullIdentifier = parts[1]
		}
	} else if strings.Contains(input, "@") {
		lastAt := strings.LastIndex(input, "@")
		id.Node = input[lastAt+1:]
		fullIdentifier = input[:lastAt]
	} else {
		fullIdentifier = input
	}

	// Extract the optional '#file' variant suffix before tag parsing.
	// Canonical grammar: <repo-or-model>[:tag][#file][@node]. '@' is already
	// stripped above; '#' separates a GGUF variant filename. LastIndex keeps
	// behavior consistent with the '@' handling — rightmost wins.
	if idx := strings.LastIndex(fullIdentifier, "#"); idx >= 0 {
		id.File = fullIdentifier[idx+1:]
		fullIdentifier = fullIdentifier[:idx]
	}

	if fullIdentifier != "" {
		if err := parseFullIdentifierWithConfig(id, fullIdentifier, opts.Config); err != nil {
			return nil, err
		}
	}

	// Namespace extraction: the Registry tells us the split convention.
	// Only runs when the Registry is known — caller-supplied or parsed. ModelName
	// is intentionally left unchanged; Namespace is additive metadata.
	registry := opts.Registry
	if registry == "" {
		registry = id.Repository
	}
	if registry != "" && id.ModelName != "" {
		id.Namespace = extractNamespace(registry, id.ModelName)
	}

	return id, nil
}

// extractNamespace returns the publisher/namespace portion of a model name
// given its registry. Conventions differ per registry; only known ones are
// extracted — unknown registries return "".
//
// Examples:
//
//	huggingface: "meta-llama/Llama-3.2-3B" → "meta-llama"
//	cloudflare:  "@cf/meta/llama-3.1-8b"   → "meta"   (strips the static @cf/ marker)
//	openrouter:  "anthropic/claude-4.5"    → "anthropic"
//	ollama, openai, groq, anthropic, bare names → "" (no namespace)
func extractNamespace(registry, model string) string {
	if model == "" {
		return ""
	}
	switch strings.ToLower(registry) {
	case "huggingface", "hf":
		// HF model IDs are strictly org/repo. Split on the last '/' so we
		// handle both forms defensively.
		if idx := strings.LastIndex(model, "/"); idx > 0 {
			return model[:idx]
		}
	case "cloudflare":
		// Cloudflare Workers AI: '@cf/<vendor>/<model>'. Strip the '@cf/'
		// static marker (redundant with Registry=cloudflare), then the
		// first segment is the vendor (Meta, Mistral, Microsoft, ...).
		s := strings.TrimPrefix(model, "@cf/")
		if idx := strings.Index(s, "/"); idx > 0 {
			return s[:idx]
		}
	case "openrouter":
		// OpenRouter: '<provider>/<model>' (e.g. 'anthropic/claude-sonnet-4.5').
		if idx := strings.Index(model, "/"); idx > 0 {
			return model[:idx]
		}
	}
	// Unknown registries: no-op. Publisher is implicit in the registry name
	// (ollama, openai, groq, anthropic, ...) or undefined (bare local models).
	return ""
}

// isCloudRegistry reports whether name is an enabled-or-declared cloud provider
// in the given config. Nil config or empty name returns false.
func isCloudRegistry(config *pkgConfig.AppsConfig, name string) bool {
	if config == nil || name == "" {
		return false
	}
	app, ok := config.LookupApp(name)
	if !ok {
		return false
	}
	return app.IsCloudProvider()
}

// parseFullIdentifierWithConfig parses the full identifier portion: [provider/]<repo>/<model-name>[:tag].
// Checks against enabled apps in config if provided.
// Error return is reserved for future malformed-ID rejection; today the
// parse is always successful and Identifier carries the result.
//
//nolint:unparam // error return is the signature contract, not dead code
func parseFullIdentifierWithConfig(id *Identifier, fullID string, config *pkgConfig.AppsConfig) error {
	// Step 1: Extract tag (everything after last colon that's not part of a port)
	tagSeparatorIdx := -1
	lastSlashIdx := strings.LastIndex(fullID, "/")
	lastColonIdx := strings.LastIndex(fullID, ":")

	if lastColonIdx > lastSlashIdx && lastColonIdx > 0 {
		tagSeparatorIdx = lastColonIdx
	}

	var pathPart string
	if tagSeparatorIdx >= 0 {
		id.Tag = fullID[tagSeparatorIdx+1:]
		pathPart = fullID[:tagSeparatorIdx]
	} else {
		pathPart = fullID
	}

	if pathPart == "" {
		return nil
	}

	parts := strings.Split(pathPart, "/")
	if len(parts) == 0 {
		return nil
	}

	startIdx := 0
	firstPart := parts[0]

	isProvider := false
	providerKey := ""
	if config != nil {
		lower := strings.ToLower(firstPart)
		config.RangeApps(func(key string, provider pkgConfig.ServiceConfig) bool {
			if provider.IsEnabled() && strings.ToLower(key) == lower {
				isProvider = true
				providerKey = key
				return false
			}
			return true
		})
	} else {
		isProvider = constants.IsKnownApp(firstPart)
		if isProvider {
			providerKey = constants.NormalizeApp(firstPart)
		}
	}

	isRepo := constants.IsKnownRegistry(firstPart)

	if len(parts) == 1 {
		id.ModelName = firstPart
		return nil
	}

	if isProvider && !isRepo {
		id.ProviderKey = providerKey
		startIdx = 1
	} else if isRepo && !isProvider {
		id.Repository = constants.NormalizeRegistry(firstPart)
		if len(parts) > 1 {
			id.ModelName = strings.Join(parts[1:], "/")
		}
		return nil
	} else if isProvider && isRepo {
		if len(parts) > 1 && constants.IsKnownRegistry(parts[1]) {
			id.ProviderKey = providerKey
			startIdx = 1
		} else {
			id.Repository = constants.NormalizeRegistry(firstPart)
			if len(parts) > 1 {
				id.ModelName = strings.Join(parts[1:], "/")
			}
			return nil
		}
	} else {
		id.ModelName = strings.Join(parts, "/")
		return nil
	}

	remaining := parts[startIdx:]

	if len(remaining) == 0 {
		return nil
	}

	if len(remaining) == 1 {
		id.ModelName = remaining[0]
	} else {
		if constants.IsKnownRegistry(remaining[0]) {
			id.Repository = constants.NormalizeRegistry(remaining[0])
			if len(remaining) > 1 {
				id.ModelName = strings.Join(remaining[1:], "/")
			}
		} else {
			id.ModelName = strings.Join(remaining, "/")
		}
	}

	return nil
}

// GetFullModelName returns the complete model identifier including tag if present
func (id *Identifier) GetFullModelName() string {
	if id.ModelName == "" {
		return ""
	}
	if id.Tag != "" {
		return id.ModelName + ":" + id.Tag
	}
	return id.ModelName
}
