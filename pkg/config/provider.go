package config

// Kind identifies which family a provider belongs to.
// Replaces the Mode string discriminator on the old ServiceConfig — each
// Kind corresponds to exactly one typed provider struct, so fields that
// only make sense for one kind (e.g. install_variants on on-demand,
// api.token on cloud) can't silently appear on another.
type Kind string

const (
	// KindOnDemand: zzRouter launches the provider per model request.
	// Current providers: vllm, llamacpp, mlx.
	KindOnDemand Kind = "on-demand"

	// KindExternal: the provider runs as a standalone daemon; zzRouter
	// connects to it but does not launch it. Current providers: ollama.
	KindExternal Kind = "external"

	// KindCloud: a hosted HTTPS API gateway. Current providers: openai,
	// anthropic, groq, gemini, openrouter, cloudflare, bedrock, azure,
	// ollama-cloud.
	KindCloud Kind = "cloud"

	// KindRegistry: a search-only catalog, never serves inference.
	// Current providers: huggingface.
	KindRegistry Kind = "registry"
)

// Provider is the kind-agnostic surface every typed provider implements.
// Consumers that don't care about kind-specific fields (most of them —
// see Appendix A of the refactor plan) use this interface; installers and
// cloud-search paths type-assert to the concrete kind at the boundary.
type Provider interface {
	Kind() Kind
	GetName() string
	GetDescription() string
	IsEnabled() bool
	IsExplicitlyDisabled() bool
	IsAutoDiscovery() bool
	Validate() error
}
