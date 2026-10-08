package config

// ProviderConfig holds configuration for a provider service (legacy node.yaml type)
type ProviderConfig struct {
	Name    string `mapstructure:"name" yaml:"name"`
	Type    string `mapstructure:"type" yaml:"type"` // vllm, llama.cpp, ollama
	Enabled bool   `mapstructure:"enabled" yaml:"enabled"`

	// Connection mode: external (connect to existing), docker (launch in Docker), native (launch as process)
	Mode string `mapstructure:"mode" yaml:"mode,omitempty"` // external, docker, native (default: external)

	// For external (existing) apps
	Address string   `mapstructure:"address" yaml:"address,omitempty"`
	Port    int      `mapstructure:"port" yaml:"port,omitempty"`
	Models  []string `mapstructure:"models" yaml:"models,omitempty"` // models served by this app

	// For launched apps (docker/native)
	Launch *LaunchConfig `mapstructure:"launch" yaml:"launch,omitempty"`
}

// LaunchConfig contains configuration for launching app instances
// Note: This is a simplified config type. The server converts this to runs.InstanceConfig
type LaunchConfig struct {
	ModelName  string            `mapstructure:"model_name" yaml:"modelName,omitempty"`
	Port       int               `mapstructure:"port" yaml:"port,omitempty"`
	EnvVars    map[string]string `mapstructure:"env_vars" yaml:"envVars,omitempty"`
	Parameters map[string]string `mapstructure:"parameters" yaml:"parameters,omitempty"`
	// Files and Docker/Native configs are handled by the server layer
	// to avoid import cycles with pkg/runs
}
