package config

// CLIConfig holds CLI-specific configuration
type CLIConfig struct {
	Nodes []NodeConfig `mapstructure:"hosts"`
}

// NodeConnection represents connection info for a host
type NodeConnection struct {
	Address string `mapstructure:"address" yaml:"address"`
	Port    int    `mapstructure:"port" yaml:"port"`

	// Deprecated: Use Key instead
	APIKey string `mapstructure:"api_key" yaml:"api_key,omitempty"`

	// Key for authentication (can be env var name or literal value)
	Key string `mapstructure:"apikeyenv" yaml:"apiKeyEnv,omitempty"`

	Secure bool `mapstructure:"secure" yaml:"secure"`

	// TLS CA certificate for verifying the server certificate.
	// Required when the server uses self-signed or internal CA certificates.
	// If empty and Secure is true, the system trust store is used.
	TLSCACert string `mapstructure:"tls_ca_cert" yaml:"tls_ca_cert,omitempty"`
}

// ClientPreferences represents client-side preferences
type ClientPreferences struct {
	Timeout      int            `mapstructure:"timeout" yaml:"timeout"`
	OutputFormat string         `mapstructure:"output_format" yaml:"output_format"`
	Verbose      bool           `mapstructure:"verbose" yaml:"verbose"`
	DefaultModel string         `mapstructure:"default_model" yaml:"default_model"`
	Editor       string         `mapstructure:"editor" yaml:"editor,omitempty"` // Preferred editor for 'config --edit'
	Theme        string         `mapstructure:"theme" yaml:"theme,omitempty"`   // UI theme: auto, catppuccin-mocha, catppuccin-latte, tokyo-night
	Emoji        string         `mapstructure:"emoji" yaml:"emoji,omitempty"`   // Emoji display: auto (default), on, off
	Chat         ChatSettings   `mapstructure:"chat" yaml:"chat,omitempty"`     // Chat session preferences
	Search       SearchSettings `mapstructure:"search" yaml:"search,omitempty"` // Search preferences
}

// ChatSettings represents chat session preferences persisted across sessions
type ChatSettings struct {
	Verbose      bool    `mapstructure:"verbose" yaml:"verbose,omitempty"`
	Thinking     bool    `mapstructure:"thinking" yaml:"thinking,omitempty"`
	SystemPrompt string  `mapstructure:"system_prompt" yaml:"system_prompt,omitempty"`
	Temperature  float64 `mapstructure:"temperature" yaml:"temperature"`
	TopP         float64 `mapstructure:"top_p" yaml:"top_p"`
	MaxTokens    int     `mapstructure:"max_tokens" yaml:"max_tokens"`
}

// SearchSettings represents search-related preferences
type SearchSettings struct {
	ModelTags       []TagFilter                    `mapstructure:"model_tags" yaml:"model_tags,omitempty"`
	DefaultLogic    string                         `mapstructure:"default_logic" yaml:"default_logic,omitempty"`
	DefaultRegistry string                         `mapstructure:"default_registry" yaml:"default_registry,omitempty"` // Last used registry
	RegistryPrefs   map[string]SearchRegistryPrefs `mapstructure:"registry_prefs" yaml:"registry_prefs,omitempty"`     // Per-registry sort/tags
	DefaultProvider string                         `mapstructure:"default_provider" yaml:"default_provider,omitempty"` // Deprecated: use DefaultRegistry
	ProviderPrefs   map[string]SearchRegistryPrefs `mapstructure:"provider_prefs" yaml:"provider_prefs,omitempty"`     // Deprecated: use RegistryPrefs
}

// SearchRegistryPrefs stores per-registry search preferences (sort, tags).
type SearchRegistryPrefs struct {
	Sort string   `mapstructure:"sort" yaml:"sort,omitempty"` // Last used sort field
	Tags []string `mapstructure:"tags" yaml:"tags,omitempty"` // Last used tag filters
}

// TagFilter represents a tag filter with its logic
type TagFilter struct {
	Tags  []string `mapstructure:"tags" yaml:"tags"`
	Logic string   `mapstructure:"logic" yaml:"logic"` // AND or OR
}

// ClusterEndpoint represents a cluster network endpoint
type ClusterEndpoint struct {
	Address string `mapstructure:"address"`
	Port    int    `mapstructure:"port"`
}
