package config

import (
	"time"

	"github.com/stperic/zzrouter/pkg/model/pricing"
)

// ClientNodeConfig represents host configuration for client-side use
// This is used by client_manager.go for storing host connection information
type ClientNodeConfig struct {
	Name      string `mapstructure:"name" yaml:"name"`
	Address   string `mapstructure:"address" yaml:"address"`
	APIKey    string `mapstructure:"api_key" yaml:"api_key,omitempty"`
	TLSCACert string `mapstructure:"tls_ca_cert" yaml:"tls_ca_cert,omitempty"` // CA cert for self-signed server certs
}

// CacheConfig holds cache configuration
type CacheConfig struct {
	// Deprecated: the model catalog cache is now event-invalidated
	// (see internal/server/README.md). This field is retained so
	// pre-existing node.yaml files still parse; the server warns
	// once at startup if a non-zero value is present, then ignores it.
	ModelListTTLSeconds int `mapstructure:"model_list_ttl_seconds" yaml:"model_list_ttl_seconds,omitempty"`
}

// DiscoveryConfig holds discovery settings
// Note: Most discovery is controlled by MDNSDiscovery at the top level
type DiscoveryConfig struct {
	// Reserved for future use
}

// ModelsConfig holds model repository and storage configuration
type ModelsConfig struct {
	// Cache is the local cache directory for models (every node has this)
	// Default: platform-specific (~/.local/share/zzrouter/models on Linux)
	Cache string `mapstructure:"cache" yaml:"cache,omitempty"`

	// Shared is the optional shared storage path (NFS, cluster FS)
	// When set, models are sourced from here and cached locally on first use
	// Example: /mnt/nfs/zzrouter/models
	Shared string `mapstructure:"shared" yaml:"shared,omitempty"`

	// Dir is DEPRECATED: use Cache instead (kept for backward compatibility)
	Dir string `mapstructure:"dir" yaml:"dir,omitempty"`

	// Download strategy: smart-default, pre-converted-only, source-only, both
	DownloadStrategy string `mapstructure:"download_strategy" yaml:"download_strategy,omitempty"`

	// Conversion settings
	Conversion ConversionConfig `mapstructure:"conversion" yaml:"conversion,omitempty"`

	// Trusted sources for pre-converted models
	TrustedSources []TrustedSource `mapstructure:"trusted_sources" yaml:"trusted_sources,omitempty"`

	// Default quantization level for GGUF
	DefaultQuantization string `mapstructure:"default_quantization" yaml:"default_quantization,omitempty"`

	// Token-cost pricing data per model
	Pricing PricingConfig `mapstructure:"pricing" yaml:"pricing,omitempty"`
}

// PricingConfig holds token-cost pricing data configuration.
type PricingConfig struct {
	Enabled         *bool  `mapstructure:"enabled" yaml:"enabled,omitempty"`
	Source          string `mapstructure:"source" yaml:"source,omitempty"`
	RefreshInterval string `mapstructure:"refresh_interval" yaml:"refresh_interval,omitempty"`
}

// IsEnabled returns true if pricing is enabled (default: false).
func (c *PricingConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return false
	}
	return *c.Enabled
}

// GetSource returns the pricing data source URL, falling back to the
// default upstream if the user hasn't overridden it.
func (c *PricingConfig) GetSource() string {
	if c.Source == "" {
		return pricing.DefaultSource
	}
	return c.Source
}

// GetRefreshInterval parses the refresh interval (default: 24h).
func (c *PricingConfig) GetRefreshInterval() time.Duration {
	if c.RefreshInterval == "" {
		return 24 * time.Hour
	}
	d, err := time.ParseDuration(c.RefreshInterval)
	if err != nil {
		return 24 * time.Hour
	}
	return d
}

// GetCache returns the local cache directory for models
// Priority: Cache field > Dir field (deprecated) > ZZROUTER_MODEL_CACHE env > platform default
func (c *ModelsConfig) GetCache() string {
	if c.Cache != "" {
		return c.Cache
	}
	// Backward compatibility: fall back to deprecated Dir field
	if c.Dir != "" {
		return c.Dir
	}
	return "" // Let modelregistry handle default detection
}

// HasShared returns true if shared storage is configured
func (c *ModelsConfig) HasShared() bool {
	return c.Shared != ""
}

// GetShared returns the shared storage path, or empty string if not set
func (c *ModelsConfig) GetShared() string {
	return c.Shared
}

// ConversionConfig holds model conversion settings
type ConversionConfig struct {
	// Preferred converter for GGUF format (llama.cpp, auto)
	GGUFConverter string `mapstructure:"gguf_converter" yaml:"gguf_converter,omitempty"`

	// Path to llama.cpp installation (optional, auto-detected)
	LlamaCppPath string `mapstructure:"llama_cpp_path" yaml:"llama_cpp_path,omitempty"`

	// Preferred converter for TensorRT-LLM format
	TensorRTConverter string `mapstructure:"tensorrt_converter" yaml:"tensorrt_converter,omitempty"`

	// Preferred converter for ONNX format
	ONNXConverter string `mapstructure:"onnx_converter" yaml:"onnx_converter,omitempty"`

	// Enable automatic conversion on first run
	AutoConvert bool `mapstructure:"auto_convert" yaml:"auto_convert,omitempty"`

	// Show conversion progress
	ShowProgress bool `mapstructure:"show_progress" yaml:"show_progress,omitempty"`
}

// TrustedSource represents a trusted model repository source
type TrustedSource struct {
	Name        string   `mapstructure:"name" yaml:"name"`
	Description string   `mapstructure:"description" yaml:"description,omitempty"`
	Priority    int      `mapstructure:"priority" yaml:"priority"`
	Formats     []string `mapstructure:"formats" yaml:"formats"`
	Enabled     bool     `mapstructure:"enabled" yaml:"enabled"`
}

// ClientConfig represents client-side configuration
type ClientConfig struct {
	// Node connection info
	Node NodeConnection `mapstructure:"server" yaml:"server"`

	// Client preferences
	Preferences ClientPreferences `mapstructure:"preferences" yaml:"preferences"`
}
