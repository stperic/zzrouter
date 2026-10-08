package config

// RoutingConfig holds resource-aware routing configuration
type RoutingConfig struct {
	// DefaultMode: "auto" (resource-based), "priority" (config priority only), "round-robin"
	DefaultMode string `mapstructure:"default_mode" yaml:"default_mode,omitempty"`

	// Resource thresholds for filtering candidates
	ResourceThresholds ResourceThresholds `mapstructure:"resource_thresholds" yaml:"resource_thresholds,omitempty"`

	// Format-specific node priorities (e.g., gguf, hf_transformers)
	FormatPriorities map[string][]NodePriority `mapstructure:"format_priorities" yaml:"format_priorities,omitempty"`

	// Model-specific routing rules (exact match or wildcard)
	ModelRouting map[string]ModelRoutingRule `mapstructure:"model_routing" yaml:"model_routing,omitempty"`

	// InjectUsageMetadata injects routing metadata (zz_provider, zz_model, zz_node) into
	// the usage block of OpenAI-compatible responses. Useful for clients like Open WebUI
	// that only display the usage object.
	InjectUsageMetadata bool `mapstructure:"inject_usage_metadata" yaml:"inject_usage_metadata,omitempty"`

	// RoutePrefix is prepended to route (model group) names when exposed via the
	// /v1/models API so that routes are visually distinguishable from real models
	// in client UIs. Incoming requests with this prefix are matched back to the
	// route. Set to "" to disable prefixing. Default: "route-".
	RoutePrefix *string `mapstructure:"route_prefix" yaml:"route_prefix,omitempty"`
}

// DefaultRoutePrefix is the default prefix added to route names in /v1/models.
const DefaultRoutePrefix = "route-"

// GetRoutePrefix returns the configured route prefix, or the default if not set.
func (rc *RoutingConfig) GetRoutePrefix() string {
	if rc.RoutePrefix != nil {
		return *rc.RoutePrefix
	}
	return DefaultRoutePrefix
}

// ResourceThresholds defines minimum resource requirements for routing
type ResourceThresholds struct {
	// Minimum free GPU memory in MB to consider a node available
	MinFreeGPUMemoryMB int64 `mapstructure:"min_free_gpu_memory_mb" yaml:"min_free_gpu_memory_mb,omitempty"`

	// Minimum free RAM in MB to consider a node available
	MinFreeRAMMB int64 `mapstructure:"min_free_ram_mb" yaml:"min_free_ram_mb,omitempty"`

	// Maximum GPU utilization percentage (0-100) to consider a node available
	MaxGPUUtilization float64 `mapstructure:"max_gpu_utilization" yaml:"max_gpu_utilization,omitempty"`
}

// NodePriority defines priority for a specific node/app combination
type NodePriority struct {
	// Node name (hostname) - use "*" for wildcard (any node)
	Node string `mapstructure:"node" yaml:"node"`

	// App name (optional) - specific provider app on the node
	App string `mapstructure:"provider" yaml:"provider,omitempty"`

	// Priority value (1-100, higher = preferred)
	Priority int `mapstructure:"priority" yaml:"priority"`
}

// ModelRoutingRule defines routing rules for a specific model or pattern
type ModelRoutingRule struct {
	// LockedTo forces this model to a specific node (no fallback)
	LockedTo string `mapstructure:"locked_to" yaml:"locked_to,omitempty"`

	// Priorities for this specific model
	Priorities []NodePriority `mapstructure:"priorities" yaml:"priorities,omitempty"`

	// FallbackEnabled allows routing to next priority if preferred unavailable
	FallbackEnabled bool `mapstructure:"fallback_enabled" yaml:"fallback_enabled,omitempty"`
}

// GetDefaultMode returns the routing mode, defaulting to "auto" if not set
func (rc *RoutingConfig) GetDefaultMode() string {
	if rc.DefaultMode == "" {
		return "auto"
	}
	return rc.DefaultMode
}

// GetMinFreeGPUMemoryMB returns the threshold with a sensible default (1GB)
func (t *ResourceThresholds) GetMinFreeGPUMemoryMB() int64 {
	if t.MinFreeGPUMemoryMB <= 0 {
		return 1024 // Default: 1GB
	}
	return t.MinFreeGPUMemoryMB
}

// GetMinFreeRAMMB returns the threshold with a sensible default (2GB)
func (t *ResourceThresholds) GetMinFreeRAMMB() int64 {
	if t.MinFreeRAMMB <= 0 {
		return 2048 // Default: 2GB
	}
	return t.MinFreeRAMMB
}

// GetMaxGPUUtilization returns the threshold with a sensible default (90%)
func (t *ResourceThresholds) GetMaxGPUUtilization() float64 {
	if t.MaxGPUUtilization <= 0 {
		return 90.0 // Default: 90%
	}
	return t.MaxGPUUtilization
}
