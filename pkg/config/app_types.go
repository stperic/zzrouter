// Package config provides OS-agnostic configuration file management for zzRouter
// App types and core configuration structures for the providers/ directory tree
package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// jsonUnmarshal / jsonMarshal aliased to keep PythonRequirement's custom
// (un)marshaler calls explicit when scanning the file — plain encoding/json
// calls in a struct with YAML tags everywhere else are easy to miss.
var (
	jsonUnmarshal = json.Unmarshal
	jsonMarshal   = json.Marshal
)

// AppProtocol represents the API protocol used by the app
type AppProtocol string

const (
	// ProtocolOllama is the Ollama-specific API protocol
	ProtocolOllama AppProtocol = "ollama"
	// ProtocolOpenAI is the OpenAI-compatible API protocol
	ProtocolOpenAI AppProtocol = "openai"
)

// ServiceConfig represents a single app's configuration (synthesized
// view of a typed Provider in the providers/ directory tree)
type ServiceConfig struct {
	Install  *InstallConfig     `yaml:"install,omitempty" json:"install,omitempty"`
	Features map[string]Feature `yaml:"features,omitempty" json:"features,omitempty"`
	// Core app settings
	// Enabled uses three-state logic:
	//   - nil (not set):    Auto-discovery mode - enable if detected (default)
	//   - &true:            Always enabled
	//   - &false:           Explicitly disabled - never auto-enable
	Enabled       *bool       `yaml:"enabled,omitempty"`
	Name          string      `yaml:"name"`
	Description   string      `yaml:"description,omitempty"`
	Protocol      AppProtocol `yaml:"protocol"`
	Mode          string      `yaml:"mode"`
	PinnedVersion string      `yaml:"pinned_version,omitempty"` // Pin to specific version (e.g., "0.8.5", "b5604")
	// Platforms lists the (os, arch) tuples where this provider can install.
	// Empty/nil = no constraint (all platforms allowed). Used to gate
	// install at the InstallCoordinator boundary so vllm-on-darwin etc.
	// fail with ErrUnsupportedPlatform instead of relying on hardcoded
	// Go-side conditionals. Arch="" matches any arch on the named OS.
	Platforms    []InstallPlatform `yaml:"platforms,omitempty" json:"platforms,omitempty"`
	Requirements *AppRequirements  `yaml:"requirements,omitempty"` // System prerequisites for install
	// InstallVariants declares platform/hardware-specific download artifacts.
	// When set, the installer selects the best-matching variant based on the
	// target node's OS/arch and GPU. Used to replace hardcoded archive-name
	// logic in Go (per "config-driven everything" principle).
	InstallVariants []AppInstallVariant `yaml:"install_variants,omitempty"`
	Configurator    string              `yaml:"configurator,omitempty"` // Path to configurator YAML file
	Capabilities    *AppCapabilities    `yaml:"capabilities,omitempty"` // Model format capabilities
	Discovery       *AppDiscovery       `yaml:"discovery,omitempty"`
	// VersionSource declares where upstream publishes this provider's
	// releases, so zzRouter can report whether a newer one exists.
	VersionSource *VersionSource `yaml:"version_source,omitempty" json:"version_source,omitempty"`

	// App-level defaults (apply to all models)
	// NOTE: No omitempty so that initialized (even if empty) defaults always serialize
	Defaults *AppDefaultsConfig `yaml:"defaults"`

	// Tier 1 — per-model parameter/environment trees.
	Models map[string]ModelSpec `yaml:"models,omitempty"`

	// Tier 2+3 — per-node tree, with per-node-per-model cells.
	Nodes map[string]NodeSpec `yaml:"nodes,omitempty"`

	// Service management (mode: service only)
	// Controls how zzRouter manages the system daemon's configuration and restarts.
	Service *ServiceManagement `yaml:"service,omitempty"`

	// Search registry configuration
	Search *AppSearchConfig `yaml:"search,omitempty"`

	// Infrastructure/runtime configuration
	Runtime *AppRuntimeConfig `yaml:"runtime"`
}

// AppRequirements defines system prerequisites for installing a provider.
// All fields are optional — only set what the provider actually needs.
type AppRequirements struct {
	DiskSpace string             `yaml:"disk_space,omitempty" json:"disk_space,omitempty"` // Minimum free disk (e.g., "500MB", "10GB")
	GPU       *GPURequirement    `yaml:"gpu,omitempty" json:"gpu,omitempty"`               // GPU policy: advisory | required
	Python    *PythonRequirement `yaml:"python,omitempty" json:"python,omitempty"`         // Python interpreter version range
}

// PythonRequirement expresses a provider's Python interpreter compatibility
// range. Supports two YAML/JSON shapes for backward compatibility with the
// single-field schema that predated the 3.14 install incident:
//
//	python: "3.9"                      # bare string — minimum only
//	python: { min: "3.9", max: "3.13" } # explicit range
//
// Max is EXCLUSIVE of the next minor (">=3.9,<3.14"). A bare "3.13" as Max
// rejects 3.14.x, accepts 3.13.y for any y. Empty Max means unbounded.
//
// The range exists because Python minor-version upgrades break runtime
// libraries that depend on C-API or threading internals (mlx_lm on 3.14
// deadlocks inside lock_PyThread_acquire_lock mid-SSE stream). A provider
// that hasn't been validated against a given minor must be able to refuse
// it up front rather than silently install into a venv that will wedge
// at first request.
type PythonRequirement struct {
	Min string `yaml:"min,omitempty" json:"min,omitempty"` // Minimum inclusive (e.g., "3.9")
	Max string `yaml:"max,omitempty" json:"max,omitempty"` // Maximum exclusive by minor (e.g., "3.14" rejects 3.14.x)
}

// UnmarshalYAML accepts either a bare string (treated as Min-only, for
// compatibility with existing provider YAMLs that predated the range
// schema) or a {min,max} mapping. Signature is the yaml.v3 shape — the
// earlier yaml.v2 shape (`func(any) error`) is NOT called by yaml.v3
// and silently falls back to the default struct decoder, which fails
// with a type error on the bare-string form. That failure mode is
// exactly what broke provider loading on the first version of this
// commit; if you're reading this and tempted to switch back, don't.
func (p *PythonRequirement) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		p.Min = node.Value
		p.Max = ""
		return nil
	case yaml.MappingNode:
		var raw struct {
			Min string `yaml:"min"`
			Max string `yaml:"max"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		p.Min = raw.Min
		p.Max = raw.Max
		return nil
	default:
		// Alias / sequence / doc nodes aren't meaningful here; let the
		// default decoder produce a YAML-shaped error pointing at the
		// offending line.
		var raw struct {
			Min string `yaml:"min"`
			Max string `yaml:"max"`
		}
		return node.Decode(&raw)
	}
}

// MarshalYAML emits the bare-string form when Max is empty so round-tripping
// legacy provider YAMLs doesn't silently upgrade them to the struct form.
func (p PythonRequirement) MarshalYAML() (any, error) {
	if p.Max == "" {
		return p.Min, nil
	}
	return struct {
		Min string `yaml:"min,omitempty"`
		Max string `yaml:"max,omitempty"`
	}{p.Min, p.Max}, nil
}

// UnmarshalJSON mirrors UnmarshalYAML for the same dual-shape reason — the
// public providers API at /zzrouter/v1/providers/* round-trips AppRequirements
// over JSON, so both wire formats must accept the bare-string form.
func (p *PythonRequirement) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if len(s) > 0 && s[0] == '"' {
		var str string
		if err := jsonUnmarshal(data, &str); err != nil {
			return err
		}
		p.Min = str
		p.Max = ""
		return nil
	}
	var raw struct {
		Min string `json:"min"`
		Max string `json:"max"`
	}
	if err := jsonUnmarshal(data, &raw); err != nil {
		return err
	}
	p.Min = raw.Min
	p.Max = raw.Max
	return nil
}

// MarshalJSON emits the bare-string form when Max is empty for symmetry
// with MarshalYAML (see rationale there).
func (p PythonRequirement) MarshalJSON() ([]byte, error) {
	if p.Max == "" {
		return jsonMarshal(p.Min)
	}
	return jsonMarshal(struct {
		Min string `json:"min,omitempty"`
		Max string `json:"max,omitempty"`
	}{p.Min, p.Max})
}

// GPURequirement expresses how a provider's install should react to
// GPU availability on the target node. Matches the semantics exposed
// by pkg/discovery/gpu: we probe the requested Vendor and then gate
// on the result according to Policy.
//
//   - Policy "advisory" — install always proceeds. If the driver is
//     not present a warning is recorded in the preflight report so
//     the user knows they're about to run on CPU.
//   - Policy "required" — install is hard-gated. Only StateDriverOK
//     passes; any other state fails preflight.
//
// The shape mirrors VariantGPURequirement (same Vendor values) so
// config authors use one vocabulary for both install selection and
// preflight gating.
type GPURequirement struct {
	Vendor string `yaml:"vendor,omitempty" json:"vendor,omitempty"` // "nvidia", "amd", "apple". Empty defaults to "nvidia".
	Policy string `yaml:"policy,omitempty" json:"policy,omitempty"` // "advisory" | "required". Empty defaults to "required".
}

// AppInstallVariant describes a single downloadable install artifact targeting
// a specific platform/hardware combination. Used by installers that ship
// multiple pre-built binaries (e.g., llama.cpp CPU vs CUDA vs ROCm).
//
// Selection algorithm (see pkg/prov_apps/install/variant.go):
//  1. Filter by platform (OS/arch must match target node).
//  2. Prefer variants whose GPU filter matches the target node's hardware.
//  3. Fall back to the first variant with no GPU filter (CPU / generic build).
//  4. If no variant matches, the install is aborted with a clear error —
//     there is NO hardcoded fallback in Go code.
type AppInstallVariant struct {
	// ID is a short human-readable slug ("win-amd64-cuda-12.4") for logs/errors.
	ID string `yaml:"id" json:"id"`

	// Platforms lists the (os, arch) tuples this variant targets.
	// At least one is required. Empty list = no match.
	Platforms []InstallPlatform `yaml:"platforms" json:"platforms"`

	// Artifact is the singular-form release asset filename, kept for
	// back-compat with older configs. New configs should use Artifacts.
	// Parsed-then-folded into Artifacts[0] by NormalizeArtifacts; consumers
	// must not read it directly. Supports {version} substitution.
	Artifact string `yaml:"artifact,omitempty" json:"artifact,omitempty"`

	// Artifacts lists every release asset that must be downloaded and
	// extracted into the install dir for this variant to be functional.
	// Order is not significant for correctness — file sets must be disjoint
	// across artifacts (no overlay semantics, fail loudly on collision).
	// One-element lists are the trivial case (single-zip variants).
	//
	// llama.cpp's Windows + CUDA variants are the canonical example: the
	// binary zip and the cudart redistributable zip are peer assets of one
	// release, both required for llama-server.exe to load.
	//
	// Each element supports {version} substitution in Filename so
	// release-tag templating works the same way as the singular form.
	Artifacts []VariantArtifact `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`

	// URLPattern overrides the default download URL for ALL artifacts in
	// this variant. Supports {version} and {artifact} substitutions. When
	// empty, installers synthesize a URL per artifact from their own
	// release-host convention. Per-artifact override lives on VariantArtifact.
	URLPattern string `yaml:"url_pattern,omitempty" json:"url_pattern,omitempty"`

	// GPU, when set, restricts this variant to target nodes that have a
	// matching GPU. Leave nil for CPU / generic variants.
	GPU *VariantGPURequirement `yaml:"gpu,omitempty" json:"gpu,omitempty"`
}

// VariantArtifact is one downloadable file belonging to an install variant.
// Multi-artifact variants list peers (e.g. llama.cpp Windows CUDA: binaries
// zip + cudart runtime zip) — the install pipeline downloads each and
// extracts them into the same install dir.
type VariantArtifact struct {
	// Filename is the release asset name. Supports {version} substitution.
	// Need not contain {version} — companion runtime zips like
	// "cudart-llama-bin-win-cuda-13.1-x64.zip" have stable filenames across
	// llama.cpp releases.
	Filename string `yaml:"filename" json:"filename"`

	// URLPattern, when set, overrides the variant- and installer-level URL
	// patterns for THIS artifact only. Useful when a single variant pulls
	// from heterogeneous hosts. Supports {version} and {filename}.
	URLPattern string `yaml:"url_pattern,omitempty" json:"url_pattern,omitempty"`
}

// NormalizeArtifacts folds the back-compat singular Artifact field into
// the canonical Artifacts slice. Idempotent. Callers that mutate
// AppInstallVariant should re-run normalization.
//
// The rule: if Artifacts is empty AND Artifact is non-empty, treat the
// single string as Artifacts[0]. If both are populated, Artifacts wins —
// the singular form is the back-compat affordance, not an additive field.
func (v *AppInstallVariant) NormalizeArtifacts() {
	if len(v.Artifacts) == 0 && v.Artifact != "" {
		v.Artifacts = []VariantArtifact{{Filename: v.Artifact}}
	}
}

// InstallPlatform is a single OS/arch tuple used for variant filtering.
type InstallPlatform struct {
	OS   string `yaml:"os" json:"os"`     // "windows", "linux", "darwin"
	Arch string `yaml:"arch" json:"arch"` // "amd64", "arm64"
}

// VariantGPURequirement filters an AppInstallVariant to target nodes with a
// matching GPU. All non-empty fields are AND-ed.
type VariantGPURequirement struct {
	// Vendor matches hardware.HardwareInfo.GPUType
	// ("nvidia", "amd", "apple", "intel"). Empty = any GPU vendor.
	Vendor string `yaml:"vendor,omitempty" json:"vendor,omitempty"`

	// CUDAMin is the minimum CUDA runtime version on the target node
	// (e.g., "12.0"). Matched against hardware.GPUInfo.CUDAVersion of
	// the first detected GPU. Empty = no CUDA version check.
	CUDAMin string `yaml:"cuda_min,omitempty" json:"cuda_min,omitempty"`
}

// AppSearchConfig defines how a provider relates to the model search system.
// A provider can either BE a search registry (with sort_options, etc.)
// or ACTIVATE another registry when enabled (via the Registry field).
type AppSearchConfig struct {
	// Registry names the model registry this provider draws from.
	// Two consumers today:
	//   1. Search UI — which registry to query when the provider is the
	//      active search source.
	//   2. Auto-deploy on POST /runs (admin-gated) — where to fetch the
	//      model from when auto_deploy=true and the model isn't yet in
	//      the cache.
	// Empty means the provider itself is the registry (huggingface,
	// ollama, cloud providers). Both consumers must agree, so the field
	// stays single until a real divergence appears; revisit if a provider
	// ever needs different sources for search vs download.
	Registry string `yaml:"registry,omitempty" json:"registry,omitempty"`

	// Search registry metadata (only used when this provider IS a registry)
	SortOptions  []string `yaml:"sort_options,omitempty" json:"sort_options,omitempty"`   // Available sort options (first is default)
	ClientSorts  []string `yaml:"client_sorts,omitempty" json:"client_sorts,omitempty"`   // Sorts handled client-side
	ClientFilter bool     `yaml:"client_filter,omitempty" json:"client_filter,omitempty"` // Name filtering uses cached results
	HasTags      bool     `yaml:"has_tags,omitempty" json:"has_tags,omitempty"`           // Supports tag-based filtering
}

// ServiceManagement configures how zzRouter manages a system daemon (mode: service).
// Used for apps like Ollama that run as persistent services.
type ServiceManagement struct {
	// Manager specifies the service manager to use for env var persistence and restarts.
	// "auto" detects: systemd (Linux) → launchd (macOS) → nssm (Windows)
	// Explicit values: "systemd", "launchd", "nssm"
	Manager string `yaml:"manager" json:"manager"`
}

// Tier is the provenance tier in the 4-tier resolver tree. Ordered low → high.
type Tier int

const (
	TierDefault Tier = iota
	TierModel
	TierNode
	TierNodeModel
	TierRequest
)

// String returns the on-wire label for a Tier value. Used by HTTP + UI to
// surface provenance without leaking the enum integer.
func (t Tier) String() string {
	switch t {
	case TierDefault:
		return "default"
	case TierModel:
		return "model"
	case TierNode:
		return "node"
	case TierNodeModel:
		return "node-model"
	case TierRequest:
		return "request"
	default:
		return "unknown"
	}
}

// ResolvedValue carries a resolved parameter/env value plus its source tier.
type ResolvedValue struct {
	Value any    `json:"value"`
	Tier  Tier   `json:"-"`
	Node  string `json:"node,omitempty"`
	Model string `json:"model,omitempty"`
	// Pattern is the model key that matched, when it is not the model's
	// own name: a glob.
	Pattern  string `json:"pattern,omitempty"`
	Endpoint string `json:"endpoint,omitempty"` // non-empty when supplied by an endpoints[E] overlay
}

// EndpointOverlay refines a resolver tier when Resolve is called with a
// matching endpoint key. Layered on top of the tier's base after the
// tier matches; tier precedence is unchanged.
type EndpointOverlay struct {
	Parameters  map[string]string `yaml:"parameters,omitempty" json:"parameters,omitempty"`
	Environment map[string]string `yaml:"environment,omitempty" json:"environment,omitempty"`
}

// ResolvedParams is the output of ServiceConfig.Resolve — key → value +
// provenance, for parameters, environment and request defaults.
type ResolvedParams struct {
	Parameters  map[string]ResolvedValue
	Environment map[string]ResolvedValue
	// Request is the request-body defaults for the model. They depend on
	// neither node nor endpoint.
	Request map[string]ResolvedValue
	// From is the variant's base when the model is a variant.
	From string
}

// ModelSpec is the per-model cell of the models tier.
//
// Parameters, Environment and Endpoints are launch-time: they become the
// process's argv and env. Request is request-time: body defaults for each
// request addressed to the model. From makes the entry a variant: a name
// of its own over another model's weights (Ollama's FROM). See
// docs/plan_model_templates_and_variants.md.
type ModelSpec struct {
	// From names the model whose weights this variant runs. Only a
	// literal models key may carry it, and it names weights, never
	// another variant.
	From        string                     `yaml:"from,omitempty" json:"from,omitempty"`
	Parameters  map[string]string          `yaml:"parameters,omitempty" json:"parameters,omitempty"`
	Environment map[string]string          `yaml:"environment,omitempty" json:"environment,omitempty"`
	Endpoints   map[string]EndpointOverlay `yaml:"endpoints,omitempty" json:"endpoints,omitempty"`
	// Request holds top-level request-body fields applied to each request
	// for this model when the client did not send them.
	Request map[string]any `yaml:"request,omitempty" json:"request,omitempty"`
}

// IsEmpty reports whether the cell says nothing, so it can be dropped.
func (m ModelSpec) IsEmpty() bool {
	return m.From == "" && len(m.Parameters) == 0 && len(m.Environment) == 0 &&
		len(m.Endpoints) == 0 && len(m.Request) == 0
}

// NodeModelSpec is the Tier 3 (node × model) cell. Same shape as ModelSpec,
// kept as a distinct type so the 4-tier tree is unambiguous at call sites.
type NodeModelSpec struct {
	Parameters  map[string]string          `yaml:"parameters,omitempty" json:"parameters,omitempty"`
	Environment map[string]string          `yaml:"environment,omitempty" json:"environment,omitempty"`
	Endpoints   map[string]EndpointOverlay `yaml:"endpoints,omitempty" json:"endpoints,omitempty"`
}

// NodeSpec is the Tier 2 per-node cell, with an embedded Tier 3 map.
type NodeSpec struct {
	Install     *InstallConfig             `yaml:"install,omitempty" json:"install,omitempty"`
	Parameters  map[string]string          `yaml:"parameters,omitempty" json:"parameters,omitempty"`
	Environment map[string]string          `yaml:"environment,omitempty" json:"environment,omitempty"`
	Endpoints   map[string]EndpointOverlay `yaml:"endpoints,omitempty" json:"endpoints,omitempty"`
	Models      map[string]NodeModelSpec   `yaml:"models,omitempty" json:"models,omitempty"`
}

// AppDefaultsConfig represents app-level defaults.
// Split into user-facing settings (parameters, environment) and
// app behavioral metadata (process patterns, timeouts, etc.).
type AppDefaultsConfig struct {
	Install *InstallConfig `yaml:"install,omitempty" json:"install,omitempty"`
	// User-facing defaults (editable via parameter editor)
	Parameters  map[string]string          `yaml:"parameters"`
	Environment map[string]string          `yaml:"environment"`
	Endpoints   map[string]EndpointOverlay `yaml:"endpoints,omitempty" json:"endpoints,omitempty"`

	// App behavioral metadata (config-driven, replaces hardcoded switch statements)
	Metadata *AppMetadata `yaml:"metadata,omitempty"`
}

// AppMetadata contains behavioral configuration for an app.
// This is config-driven metadata that replaces hardcoded switch statements.
type AppMetadata struct {
	ProcessPatterns []string `yaml:"process_patterns,omitempty"` // Patterns for orphan process detection (e.g., ["vllm.entrypoints"])
	StartupPatterns []string `yaml:"startup_patterns,omitempty"` // Log patterns indicating readiness (e.g., ["Application startup complete"])
	ShutdownTimeout string   `yaml:"shutdown_timeout,omitempty"` // Graceful shutdown: "30s", "gpu" (30s), or empty (5s default)
	ProcessorType   string   `yaml:"processor_type,omitempty"`   // Display label: "GPU", "Metal GPU", "CPU"
	ContextParam    string   `yaml:"context_param,omitempty"`    // Parameter name for context length: "max-model-len", "ctx-size"
}

// AppCapabilities defines what model formats an app supports
type AppCapabilities struct {
	Formats    []string `yaml:"formats"`               // Supported formats: gguf, safetensors, pytorch, etc.
	FormatNote string   `yaml:"format_note,omitempty"` // Optional note to display with format (e.g., "ollama" -> "gguf(ollama)")
	Models     []string `yaml:"models"`                // Explicit model assignments (overrides format matching, supports wildcards)
	Priority   int      `yaml:"priority"`              // Priority for auto-assignment (0-10, higher = more preferred)
	// DefaultVariant selects which file to download from a multi-variant
	// repo (e.g. GGUF Q4_K_M vs Q8_0 on the same HF model). Read by the
	// auto-deploy path when /runs is invoked with a model that isn't yet
	// in the cache; ignored if the source repo doesn't carry variants.
	DefaultVariant string `yaml:"default_variant,omitempty"`
	// WireEndpoints declares which compat HTTP routes the provider's
	// process serves. Used by the router to gate /v1/* requests against
	// backends that would otherwise 404 the upstream. Required field —
	// config load fails for enabled providers that omit it. The accepted
	// values are the keys of validWireEndpoints.
	//
	// "responses" means the provider's process serves /v1/responses
	// natively on its own HTTP wire (vLLM, OpenAI cloud, OpenRouter
	// responses-aware deployments). zzRouter passes through.
	//
	// "responses_compat" means the provider does NOT serve
	// /v1/responses natively, but zzRouter provides client-facing
	// compatibility via the Responses ↔ Chat Completions translation
	// shim (responses_translator.go). Use this when you want
	// /v1/responses traffic to land on a chat-completions-only
	// backend. Mutually informative with "responses": a provider
	// declaring both is a config error (zzRouter will not validate
	// that, but the gate prefers native passthrough).
	//
	// "messages" means the provider serves the Anthropic Messages API
	// (/v1/messages, /v1/messages/count_tokens) natively. zzRouter
	// passes through.
	//
	// "messages_compat" means it does not, and zzRouter translates the
	// Messages API to and from Chat Completions for it
	// (pkg/protocol/anthropic), the way responses_compat does for
	// /v1/responses.
	WireEndpoints []string `yaml:"wire_endpoints"`
}

// validWireEndpoints is the closed set of values WireEndpoints accepts.
// Adding a new wire endpoint is a code change so the gate at the router
// stays a closed enum and can't drift into typos at the YAML edge.
var validWireEndpoints = map[string]struct{}{
	"chat_completions": {},
	"completions":      {},
	"embeddings":       {},
	"messages":         {},
	"rerank":           {},
	"responses":        {},
	"responses_compat": {},
	"messages_compat":  {},
}

// compatShims maps each translation shim to the native endpoint it
// stands in for. A shim translates to Chat Completions, so it needs
// chat_completions, and it excludes its native endpoint: a provider
// either speaks the API or has it translated.
var compatShims = map[string]string{
	"responses_compat": "responses",
	"messages_compat":  "messages",
}

// wireEndpointList renders validWireEndpoints for error messages, so the
// message cannot drift from the set it describes.
func wireEndpointList() string {
	return strings.Join(slices.Sorted(maps.Keys(validWireEndpoints)), ", ")
}

// SupportsWireEndpoint reports whether the named OpenAI-compat HTTP
// route is served by the provider's process. Returns false on a nil
// capabilities pointer so callers don't need a separate nil check.
func (c *AppCapabilities) SupportsWireEndpoint(endpoint string) bool {
	if c == nil {
		return false
	}
	for _, e := range c.WireEndpoints {
		if e == endpoint {
			return true
		}
	}
	return false
}

// ValidateWireEndpoints enforces presence + closed-enum membership at
// config load. Returns an error suitable for wrapping at the per-
// provider validation site so the load failure names the offending
// provider.
func (c *AppCapabilities) ValidateWireEndpoints() error {
	if c == nil || len(c.WireEndpoints) == 0 {
		return fmt.Errorf("capabilities.wire_endpoints is required and must list at least one of: %s", wireEndpointList())
	}
	for _, e := range c.WireEndpoints {
		if _, ok := validWireEndpoints[e]; !ok {
			return fmt.Errorf("capabilities.wire_endpoints: unknown value %q (valid: %s)", e, wireEndpointList())
		}
	}
	for _, shim := range slices.Sorted(maps.Keys(compatShims)) {
		if !c.SupportsWireEndpoint(shim) {
			continue
		}
		native := compatShims[shim]
		if c.SupportsWireEndpoint(native) {
			return fmt.Errorf("capabilities.wire_endpoints: '%s' (native) and '%s' (translation shim) are mutually exclusive", native, shim)
		}
		if !c.SupportsWireEndpoint("chat_completions") {
			return fmt.Errorf("capabilities.wire_endpoints: '%s' requires 'chat_completions' (the shim translates to Chat Completions)", shim)
		}
	}
	return nil
}
