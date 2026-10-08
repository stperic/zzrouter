package config

import (
	"fmt"
	"slices"
	"strings"
)

// OnDemandProvider is a provider that zzRouter launches per model request
// (vllm, llamacpp, mlx). It is the most configuration-heavy kind: it carries
// install variants, readiness probes, a port-pool, and optional node/model
// parameter overrides. Everything a local inference engine needs.
type OnDemandProvider struct {
	Install         *InstallConfig       `yaml:"install,omitempty" json:"install,omitempty"`
	Features        map[string]Feature   `yaml:"features,omitempty" json:"features,omitempty"`
	Name            string               `yaml:"-" json:"name"`
	Description     string               `yaml:"description,omitempty" json:"description,omitempty"`
	Protocol        AppProtocol          `yaml:"protocol" json:"protocol"`
	Enabled         *bool                `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	PinnedVersion   string               `yaml:"pinned_version,omitempty" json:"pinned_version,omitempty"`
	VersionSource   *VersionSource       `yaml:"version_source,omitempty" json:"version_source,omitempty"`
	Platforms       []InstallPlatform    `yaml:"platforms,omitempty" json:"platforms,omitempty"`
	Requirements    *AppRequirements     `yaml:"requirements,omitempty" json:"requirements,omitempty"`
	InstallVariants []AppInstallVariant  `yaml:"install_variants,omitempty" json:"install_variants,omitempty"`
	Capabilities    *AppCapabilities     `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Discovery       *AppDiscovery        `yaml:"discovery,omitempty" json:"discovery,omitempty"`
	Defaults        *AppDefaultsConfig   `yaml:"defaults" json:"defaults"`
	ModelDefaults   map[string]ModelSpec `yaml:"model_defaults,omitempty" json:"model_defaults,omitempty"`
	Models          map[string]ModelSpec `yaml:"models,omitempty" json:"models,omitempty"`
	Nodes           map[string]NodeSpec  `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	Search          *AppSearchConfig     `yaml:"search,omitempty" json:"search,omitempty"`
	Runtime         OnDemandRuntime      `yaml:"runtime" json:"runtime"`
}

// OnDemandRuntime configures how a per-model inference engine is launched
// and supervised. Execution.Command is required (Python or CLI). Port
// assignment is dynamic within PortRange; BasePort anchors the pool.
// Container/Resources are preserved from legacy AppRuntimeConfig — not
// currently used by the shipped vllm/llamacpp/mlx defaults, but kept so
// a Docker-based on-demand variant doesn't silently lose its config
// during the bridge.
type OnDemandRuntime struct {
	PortRange             []int             `yaml:"port_range" json:"port_range"`
	BasePort              int               `yaml:"base_port" json:"base_port"`
	KeepAlive             string            `yaml:"keep_alive,omitempty" json:"keep_alive,omitempty"`
	MaxConcurrentRequests int               `yaml:"max_concurrent_requests,omitempty" json:"max_concurrent_requests,omitempty"`
	QueueTimeout          string            `yaml:"queue_timeout,omitempty" json:"queue_timeout,omitempty"`
	Execution             ExecutionConfig   `yaml:"execution" json:"execution"`
	HealthCheck           HealthcheckConfig `yaml:"health_check,omitempty" json:"health_check,omitempty"`
	Container             *ContainerConfig  `yaml:"container,omitempty" json:"container,omitempty"`
	Resources             *ResourcesConfig  `yaml:"resources,omitempty" json:"resources,omitempty"`
}

// Provider interface satisfaction.

func (p *OnDemandProvider) Kind() Kind             { return KindOnDemand }
func (p *OnDemandProvider) GetName() string        { return p.Name }
func (p *OnDemandProvider) GetDescription() string { return p.Description }
func (p *OnDemandProvider) IsEnabled() bool        { return p.Enabled != nil && *p.Enabled }
func (p *OnDemandProvider) IsExplicitlyDisabled() bool {
	return p.Enabled != nil && !*p.Enabled
}
func (p *OnDemandProvider) IsAutoDiscovery() bool { return p.Enabled == nil }

// Validate enforces the invariants from the old ServiceConfig.Validate
// that are specific to on-demand providers: protocol, a non-empty port
// pool, and a valid execution type. Command may be empty for template
// entries awaiting configurator fill.
func (p *OnDemandProvider) Validate() error {
	if err := providerToServiceConfig(p).ValidateInstall(); err != nil {
		return err
	}
	if err := validateFeatures(p.Features); err != nil {
		return fmt.Errorf("provider %q: %w", p.Name, err)
	}
	if p.Protocol != ProtocolOpenAI && p.Protocol != ProtocolOllama {
		return fmt.Errorf("provider '%s': invalid protocol '%s', must be 'openai' or 'ollama'", p.Name, p.Protocol)
	}
	if p.Runtime.BasePort == 0 && len(p.Runtime.PortRange) == 0 {
		return fmt.Errorf("provider '%s': runtime.base_port or runtime.port_range is required for on-demand mode", p.Name)
	}
	validTypes := []string{"cli", "python"}
	if !slices.Contains(validTypes, p.Runtime.Execution.Type) {
		return fmt.Errorf("provider '%s': invalid execution type '%s', must be one of: %s",
			p.Name, p.Runtime.Execution.Type, strings.Join(validTypes, ", "))
	}
	if err := ValidateExecutionCommand(p.Name, p.Runtime.Execution); err != nil {
		return err
	}
	if err := ValidateInstallVariants(p.Name, p.InstallVariants); err != nil {
		return fmt.Errorf("provider '%s': %w", p.Name, err)
	}
	if p.Runtime.Container != nil {
		for _, vol := range p.Runtime.Container.Volumes {
			if len(strings.Split(vol, ":")) < 2 {
				return fmt.Errorf("provider '%s': invalid volume spec '%s', must be 'host:container' or 'host:container:ro'", p.Name, vol)
			}
		}
	}
	if !p.IsExplicitlyDisabled() {
		if err := p.Capabilities.ValidateWireEndpoints(); err != nil {
			return fmt.Errorf("provider '%s': %w", p.Name, err)
		}
	}
	if err := checkModels(modelsLaunched, p.ModelDefaults, p.Models); err != nil {
		return fmt.Errorf("provider '%s': %w", p.Name, err)
	}
	return nil
}

// MarshalYAML keeps an explicit empty features block across provider writes.
func (p OnDemandProvider) MarshalYAML() (any, error) {
	type plain OnDemandProvider
	return marshalFeatureProvider(plain(p), p.Features)
}
