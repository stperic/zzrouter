package config

import "fmt"

// ExternalProvider is a daemon that runs independently of zzRouter and
// exposes an HTTP endpoint we connect to (today: ollama). zzRouter may
// still manage the service-level config via systemd/launchd/nssm (see
// ServiceManagement), but never launches it per model.
type ExternalProvider struct {
	Features      map[string]Feature `yaml:"features,omitempty" json:"features,omitempty"`
	Name          string             `yaml:"-" json:"name"`
	Description   string             `yaml:"description,omitempty" json:"description,omitempty"`
	Protocol      AppProtocol        `yaml:"protocol" json:"protocol"`
	Enabled       *bool              `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	PinnedVersion string             `yaml:"pinned_version,omitempty" json:"pinned_version,omitempty"`
	VersionSource *VersionSource     `yaml:"version_source,omitempty" json:"version_source,omitempty"`
	Requirements  *AppRequirements   `yaml:"requirements,omitempty" json:"requirements,omitempty"`
	// InstallVariants declares per-platform/hardware install artifacts so
	// zzRouter can install the daemon without hardcoded URLs in Go. Most
	// externals are user-installed and leave this empty; Ollama declares
	// variants because zzRouter ships an installer for it. Selection is
	// platform/GPU-aware via pkg/prov_apps/install/variant.Select.
	InstallVariants []AppInstallVariant  `yaml:"install_variants,omitempty" json:"install_variants,omitempty"`
	Capabilities    *AppCapabilities     `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Discovery       *AppDiscovery        `yaml:"discovery,omitempty" json:"discovery,omitempty"`
	Defaults        *AppDefaultsConfig   `yaml:"defaults" json:"defaults"`
	Models          map[string]ModelSpec `yaml:"models,omitempty" json:"models,omitempty"`
	Nodes           map[string]NodeSpec  `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	Service         *ServiceManagement   `yaml:"service,omitempty" json:"service,omitempty"`
	Search          *AppSearchConfig     `yaml:"search,omitempty" json:"search,omitempty"`
	Runtime         ExternalRuntime      `yaml:"runtime" json:"runtime"`
}

// ExternalRuntime carries only what we need to talk to an external daemon:
// endpoint, optional model-URL template (for web links), and a healthcheck.
// No Execution.Command — the daemon manages its own lifecycle.
type ExternalRuntime struct {
	Endpoint    string            `yaml:"endpoint" json:"endpoint"`
	ModelURL    string            `yaml:"model_url,omitempty" json:"model_url,omitempty"`
	KeepAlive   string            `yaml:"keep_alive,omitempty" json:"keep_alive,omitempty"`
	API         *APIConfig        `yaml:"api,omitempty" json:"api,omitempty"`
	HealthCheck HealthcheckConfig `yaml:"health_check,omitempty" json:"health_check,omitempty"`
}

func (p *ExternalProvider) Kind() Kind             { return KindExternal }
func (p *ExternalProvider) GetName() string        { return p.Name }
func (p *ExternalProvider) GetDescription() string { return p.Description }
func (p *ExternalProvider) IsEnabled() bool        { return p.Enabled != nil && *p.Enabled }
func (p *ExternalProvider) IsExplicitlyDisabled() bool {
	return p.Enabled != nil && !*p.Enabled
}
func (p *ExternalProvider) IsAutoDiscovery() bool { return p.Enabled == nil }

// Validate enforces the subset of ServiceConfig.Validate that applies to
// external daemons: protocol + endpoint. Credentials are optional (Ollama
// runs without auth by default), but auth_type must be one zzRouter knows.
func (p *ExternalProvider) Validate() error {
	if err := validateFeatures(p.Features); err != nil {
		return fmt.Errorf("provider %q: %w", p.Name, err)
	}
	if p.Protocol != ProtocolOpenAI && p.Protocol != ProtocolOllama {
		return fmt.Errorf("provider '%s': invalid protocol '%s', must be 'openai' or 'ollama'", p.Name, p.Protocol)
	}
	if p.Runtime.Endpoint == "" {
		return fmt.Errorf("provider '%s': runtime.endpoint is required for external mode", p.Name)
	}
	if !p.IsExplicitlyDisabled() {
		if err := p.Capabilities.ValidateWireEndpoints(); err != nil {
			return fmt.Errorf("provider '%s': %w", p.Name, err)
		}
	}
	if err := p.Runtime.API.Validate(); err != nil {
		return fmt.Errorf("provider '%s': %w", p.Name, err)
	}
	if err := ValidateInstallVariants(p.Name, p.InstallVariants); err != nil {
		return fmt.Errorf("provider '%s': %w", p.Name, err)
	}
	if err := checkModels(modelsNotLaunched, nil, p.Models); err != nil {
		return fmt.Errorf("provider '%s': %w", p.Name, err)
	}
	return nil
}

// NewOllamaConnectProvider builds an ExternalProvider pointing at a user-
// owned Ollama daemon on the LAN or a remote host. No install lifecycle —
// zzRouter does not manage the daemon's binary, only talks to it over HTTP.
// Install fields (PinnedVersion, InstallVariants, Service) are left unset
// so the installer never auto-triggers on connect entries.
func NewOllamaConnectProvider(name, endpoint, token string) *ExternalProvider {
	enabled := true
	// Ollama checks no caller key, so without a token of its own nothing is sent.
	api := &APIConfig{AuthType: AuthTypeNone}
	if token != "" {
		api = &APIConfig{
			AuthType:   AuthTypeBearer,
			AuthHeader: "Authorization",
			AuthPrefix: "Bearer ",
			Token:      token,
		}
	}
	return &ExternalProvider{
		Name:        name,
		Description: "Ollama connect",
		Protocol:    ProtocolOllama,
		Enabled:     &enabled,
		Runtime: ExternalRuntime{
			Endpoint: endpoint,
			API:      api,
		},
		Capabilities: &AppCapabilities{
			WireEndpoints: []string{"chat_completions", "completions", "embeddings", "responses_compat"},
		},
	}
}

// MarshalYAML keeps an explicit empty features block across provider writes.
func (p ExternalProvider) MarshalYAML() (any, error) {
	type plain ExternalProvider
	return marshalFeatureProvider(plain(p), p.Features)
}
