package config

import "fmt"

// SearchRegistry is a catalog-only provider — never serves inference,
// only model search metadata (today: huggingface). It carries an endpoint
// and optional catalog authentication; it has no runtime lifecycle.
type SearchRegistry struct {
	Connection  *ConnectionConfig `yaml:"connection,omitempty" json:"connection,omitempty"`
	Name        string            `yaml:"-" json:"name"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Enabled     *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Search      *AppSearchConfig  `yaml:"search,omitempty" json:"search,omitempty"`
	Runtime     RegistryRuntime   `yaml:"runtime" json:"runtime"`
}

// RegistryRuntime is the smallest of the four: just where to reach the
// catalog and how to format model page URLs.
type RegistryRuntime struct {
	API      *APIConfig `yaml:"api,omitempty" json:"api,omitempty"`
	Endpoint string     `yaml:"endpoint" json:"endpoint"`
	ModelURL string     `yaml:"model_url,omitempty" json:"model_url,omitempty"`
}

func (p *SearchRegistry) Kind() Kind             { return KindRegistry }
func (p *SearchRegistry) GetName() string        { return p.Name }
func (p *SearchRegistry) GetDescription() string { return p.Description }
func (p *SearchRegistry) IsEnabled() bool        { return p.Enabled != nil && *p.Enabled }
func (p *SearchRegistry) IsExplicitlyDisabled() bool {
	return p.Enabled != nil && !*p.Enabled
}
func (p *SearchRegistry) IsAutoDiscovery() bool { return p.Enabled == nil }

// Validate enforces the only thing a registry needs: a reachable endpoint.
func (p *SearchRegistry) Validate() error {
	if err := p.Connection.Validate(); err != nil {
		return fmt.Errorf("provider %q: %w", p.Name, err)
	}
	if err := p.Runtime.API.Validate(); err != nil {
		return err
	}
	if p.Runtime.API != nil && p.Connection == nil {
		return fmt.Errorf("registry authentication requires declared connection fields")
	}
	if p.Runtime.Endpoint == "" {
		return fmt.Errorf("provider '%s': runtime.endpoint is required for registry mode", p.Name)
	}
	return nil
}
