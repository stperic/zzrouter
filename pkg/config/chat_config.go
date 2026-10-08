package config

// ChatConfig holds chat interface configuration
type ChatConfig struct {
	// SlashCommands configuration for in-chat commands
	SlashCommands SlashCommandsConfig `mapstructure:"slash_commands" yaml:"slash_commands,omitempty"`
}

// SlashCommandsConfig holds slash command configuration
type SlashCommandsConfig struct {
	// Enabled controls whether slash commands are processed in chat messages
	// Default: true
	Enabled *bool `mapstructure:"enabled" yaml:"enabled,omitempty"`

	// Prefix is the command prefix character(s)
	// Default: "/"
	Prefix string `mapstructure:"prefix" yaml:"prefix,omitempty"`
}

// IsEnabled returns true if slash commands are enabled (default: true)
func (c *SlashCommandsConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true // Default: enabled
	}
	return *c.Enabled
}

// GetPrefix returns the command prefix (default: "/")
func (c *SlashCommandsConfig) GetPrefix() string {
	if c.Prefix == "" {
		return "/"
	}
	return c.Prefix
}
