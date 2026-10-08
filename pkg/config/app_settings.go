// Package config provides OS-agnostic configuration file management for zzRouter
// Global and unified settings for provider config
package config

import "time"

// SecuritySettings represents security configuration
type SecuritySettings struct {
	ValidateImages         bool     `yaml:"validate_images"`
	AllowedImageRegistries []string `yaml:"allowed_image_registries"`
	AllowedVolumePaths     []string `yaml:"allowed_volume_paths"`
	AllowedExecutablePaths []string `yaml:"allowed_executable_paths"` // Executable path whitelist for native processes
	RequireVersionPin      bool     `yaml:"require_version_pin"`
}

// GlobalSettings represents global zzRouter settings (v1.0 format)
type GlobalSettings struct {
	Security            SecuritySettings `yaml:"security"`
	AutoPullImages      bool             `yaml:"auto_pull_images"`
	AlwaysPullLatest    bool             `yaml:"always_pull_latest"`
	CleanupOnExit       bool             `yaml:"cleanup_on_exit"`
	LogLevel            string           `yaml:"log_level"`
	MaxConcurrentModels int              `yaml:"max_concurrent_models"`
	PortRange           []int            `yaml:"port_range,omitempty"` // Format: [start, end]
}

// UnifiedSettings represents global settings in v2.0 unified format
type UnifiedSettings struct {
	Discovery          DiscoverySettings `yaml:"discovery"`
	Security           SecuritySettings  `yaml:"security"`
	Runtime            RuntimeSettings   `yaml:"runtime"`
	Network            NetworkSettings   `yaml:"network"`
	Updates            UpdateSettings    `yaml:"updates,omitempty"`
	SamplingParameters map[string]string `yaml:"sampling_parameters,omitempty"` // Global sampling parameter defaults
}

// UpdateSettings controls whether zzRouter consults provider upstreams to
// report that a newer release exists. It never triggers an upgrade.
type UpdateSettings struct {
	// ProviderVersionChecks gates every outbound lookup. Absent means
	// enabled. An air-gapped cluster sets this false and zzRouter then makes
	// no upstream request at all, reporting every provider as unknown rather
	// than silently timing out on each one.
	ProviderVersionChecks *bool `yaml:"provider_version_checks,omitempty"`
	// CheckInterval is how long a successful lookup is reused, as a Go
	// duration. Absent or unparseable means the package default.
	CheckInterval string `yaml:"check_interval,omitempty"`
}

// ProviderVersionChecksEnabled reports whether upstream lookups may run,
// defaulting to true when unset.
func (u UpdateSettings) ProviderVersionChecksEnabled() bool {
	return u.ProviderVersionChecks == nil || *u.ProviderVersionChecks
}

// CheckIntervalOrDefault parses CheckInterval, falling back to fallback for
// an absent, unparseable or non-positive value. A bad duration must not
// disable caching and start hammering upstreams, so it degrades to the
// default rather than to zero.
func (u UpdateSettings) CheckIntervalOrDefault(fallback time.Duration) time.Duration {
	if u.CheckInterval == "" {
		return fallback
	}
	d, err := time.ParseDuration(u.CheckInterval)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// DiscoverySettings represents discovery configuration
type DiscoverySettings struct {
	Timeout         int  `yaml:"timeout"`
	Parallel        bool `yaml:"parallel"`
	CacheTTL        int  `yaml:"cache_ttl"`
	RespectPlatform bool `yaml:"respect_platform"`
}

// RuntimeSettings represents runtime configuration
type RuntimeSettings struct {
	PortRange                []int             `yaml:"port_range"`
	MaxConcurrentModels      int               `yaml:"max_concurrent_models"`
	CleanupOnExit            bool              `yaml:"cleanup_on_exit"`
	CleanupOrphanedProcesses bool              `yaml:"cleanup_orphaned_processes"` // Clean up orphaned on-demand processes on startup
	LogLevel                 string            `yaml:"log_level"`
	ModelPaths               map[string]string `yaml:"model_paths"`
	CachePaths               map[string]string `yaml:"cache_paths"`
}

// NetworkSettings represents network configuration
type NetworkSettings struct {
	Name   string `yaml:"name"`
	Driver string `yaml:"driver"`
}
