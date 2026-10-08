package config

import "fmt"

// CloudProvider is a hosted HTTPS API gateway (openai, anthropic, groq,
// gemini, openrouter, cloudflare, bedrock, azure, ollama-cloud). Cloud
// providers never ship parameters or install variants — they're pure auth
// + endpoint + occasional catalog-listing metadata.
type CloudProvider struct {
	Features    map[string]Feature `yaml:"features,omitempty" json:"features,omitempty"`
	Name        string             `yaml:"-" json:"name"`
	Description string             `yaml:"description,omitempty" json:"description,omitempty"`
	Enabled     *bool              `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Capabilities.Models holds the registered model catalog for this cloud
	// provider. AddCloudModel/RemoveCloudModels persist into this slice; the
	// /v1/models surface and inference routing both depend on it.
	Capabilities *AppCapabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Search       *AppSearchConfig `yaml:"search,omitempty" json:"search,omitempty"`
	Runtime      CloudRuntime     `yaml:"runtime" json:"runtime"`
}

// CloudRuntime: endpoint + required API auth + optional catalog-listing
// overrides. API is a value (not pointer) — cloud providers always have
// an auth config, even if auth_type is "none".
type CloudRuntime struct {
	Endpoint          string            `yaml:"endpoint" json:"endpoint"`
	ModelURL          string            `yaml:"model_url,omitempty" json:"model_url,omitempty"`
	ModelsPath        string            `yaml:"models_path,omitempty" json:"models_path,omitempty"`
	ModelsResponseKey string            `yaml:"models_response_key,omitempty" json:"models_response_key,omitempty"`
	API               APIConfig         `yaml:"api" json:"api"`
	HealthCheck       HealthcheckConfig `yaml:"health_check,omitempty" json:"health_check,omitempty"`
}

func (p *CloudProvider) Kind() Kind             { return KindCloud }
func (p *CloudProvider) GetName() string        { return p.Name }
func (p *CloudProvider) GetDescription() string { return p.Description }
func (p *CloudProvider) IsEnabled() bool        { return p.Enabled != nil && *p.Enabled }
func (p *CloudProvider) IsExplicitlyDisabled() bool {
	return p.Enabled != nil && !*p.Enabled
}
func (p *CloudProvider) IsAutoDiscovery() bool { return p.Enabled == nil }

// IsAvailable returns true when the provider is enabled AND its credentials
// resolve (env vars are set). Replaces ServiceConfig.IsCloudAvailable.
func (p *CloudProvider) IsAvailable() bool {
	if !p.IsEnabled() {
		return false
	}
	return p.Runtime.API.HasCredentials()
}

// Validate enforces cloud invariants: endpoint required, auth config
// present (the API value is always present, but auth_type must be set to
// one of the known values — empty is not allowed for cloud).
func (p *CloudProvider) Validate() error {
	if err := validateFeatures(p.Features); err != nil {
		return fmt.Errorf("provider %q: %w", p.Name, err)
	}
	if p.Runtime.Endpoint == "" {
		return fmt.Errorf("provider '%s': runtime.endpoint is required for cloud mode", p.Name)
	}
	if p.Runtime.API.AuthType == "" {
		return fmt.Errorf("provider '%s': runtime.api.auth_type is required for cloud mode", p.Name)
	}
	if err := p.Runtime.API.Validate(); err != nil {
		return fmt.Errorf("provider '%s': %w", p.Name, err)
	}
	if !p.Runtime.API.SendsOwnCredential() {
		return fmt.Errorf("provider '%s': a cloud provider authenticates with its own credential, so runtime.api.auth_type cannot be %q", p.Name, p.Runtime.API.AuthType)
	}
	if !p.IsExplicitlyDisabled() {
		if err := p.Capabilities.ValidateWireEndpoints(); err != nil {
			return fmt.Errorf("provider '%s': %w", p.Name, err)
		}
	}
	return nil
}

// MarshalYAML keeps an explicit empty features block across provider writes.
func (p CloudProvider) MarshalYAML() (any, error) {
	type plain CloudProvider
	return marshalFeatureProvider(plain(p), p.Features)
}
