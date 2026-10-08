// Package config provides OS-agnostic configuration file management for zzRouter
// Node configuration management functionality
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/constants"
	"gopkg.in/yaml.v3"
)

// Node Configuration Defaults
// Timeout constants are centralized in pkg/constants/timeouts.go
// Port constants are centralized in pkg/constants/defaults.go
const (
	// DefaultVersion is the default version string (should be overridden by build)
	DefaultVersion = "1.0.0"

	// DefaultNodeName is the default host name when none is provided
	DefaultNodeName = "local-host"
)

// Port constants - re-exported from pkg/constants for backward compatibility
const (
	DefaultZZROUTERPort = constants.DefaultZZROUTERPort
	DefaultOllamaPort   = constants.DefaultOllamaPort
	DefaultLlamaCppPort = constants.DefaultLlamaCppPort
	DefaultVLLMPort     = constants.DefaultVLLMPort
)

// Standard ports that don't need to be shown in host display
var DefaultStandardPorts = constants.StandardPorts

// Common ports to check when discovering zzRouter nodes
var DefaultCommonPorts = constants.CommonZZROUTERPorts

// App default ports mapping
var AppDefaultPorts = constants.AppDefaultPorts

// LoadNodeConfig loads host configuration from file
// Searches in priority order: current directory, then user config directory
func (cm *ConfigManager) LoadNodeConfig() (*NodeConfig, error) {
	// Load .env from config directory so env var references (e.g. ZZROUTER_ADMIN_API_KEY) resolve
	cm.LoadConfigDirEnv()

	// Try to find existing config file (current dir first, then user config)
	configFile := cm.FindNodeConfigFile()

	// If no config found, create default in user config directory
	if configFile == "" {
		configDir := cm.GetNodeConfigDir()
		if err := cm.EnsureDir(configDir); err != nil {
			return nil, fmt.Errorf("failed to create host config dir: %w", err)
		}
		return createNodeConfigFile(filepath.Join(configDir, hostConfigFileName))
	}

	return readNodeConfigFile(configFile)
}

// createNodeConfigFile writes the default node template to path and
// returns it parsed. The file is written first and parsed back so a
// fresh node and a reloaded one cannot disagree about what the
// defaults are.
func createNodeConfigFile(path string) (*NodeConfig, error) {
	templateContent := templates.GetNodeTemplate()
	if err := os.WriteFile(path, []byte(templateContent), 0600); err != nil {
		return nil, fmt.Errorf("failed to write template to file: %w", err)
	}

	// A separate viper instance, so parsing cannot modify the file.
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader([]byte(templateContent))); err != nil {
		return nil, fmt.Errorf("failed to parse template config: %w", err)
	}
	return unmarshalNodeConfig(v, path)
}

// readNodeConfigFile parses an existing node config file.
func readNodeConfigFile(path string) (*NodeConfig, error) {
	// Refuse to load if the file is world-readable. Config files may
	// contain API keys and cluster network secrets; loading them under
	// loose perms is a mistake. Override with ZZROUTER_ALLOW_INSECURE_PERMS=1.
	if err := requireSafePerms(path); err != nil {
		return nil, err
	}

	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read host config from %s: %w", path, err)
	}
	return unmarshalNodeConfig(v, path)
}

// unmarshalNodeConfig decodes v and applies the normalization every
// load path shares, so no caller sees a half-resolved config.
func unmarshalNodeConfig(v *viper.Viper, path string) (*NodeConfig, error) {
	var config NodeConfig
	if err := v.Unmarshal(&config, viper.DecodeHook(nodeConfigDecodeHook())); err != nil {
		return nil, fmt.Errorf("failed to unmarshal host config: %w", err)
	}
	config.SourcePath = path
	config.applyLoadDefaults()
	return &config, nil
}

// SaveNodeConfig saves host configuration to file
func (cm *ConfigManager) SaveNodeConfig(config *NodeConfig) error {
	configDir := cm.GetNodeConfigDir()
	return cm.SaveNodeConfigToDir(config, configDir)
}

// SaveNodeConfigToDir saves host configuration to a specific directory.
// Uses proper YAML marshal instead of regex text replacement.
func (cm *ConfigManager) SaveNodeConfigToDir(config *NodeConfig, dir string) error {
	if err := cm.EnsureDir(dir); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}

	configFile := filepath.Join(dir, hostConfigFileName)

	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Preserve existing file ownership when overwriting.
	// This matters when root CLI writes to zzrouter-owned /etc/zzrouter/node.yaml.
	// Only call Chown if current process UID differs from the file owner —
	// avoids triggering seccomp filters when the service writes its own files.
	var owner fileOwner
	var hasOwner bool
	if info, err := os.Stat(configFile); err == nil {
		owner, hasOwner = getFileOwner(info)
	}

	if err := os.WriteFile(configFile, data, 0600); err != nil {
		return err
	}

	if hasOwner && owner.uid != os.Getuid() {
		_ = os.Chown(configFile, owner.uid, owner.gid)
	}

	return nil
}

// LoadNodeConfig convenience function that creates a ConfigManager and loads host config
func LoadNodeConfig(appName string) (*NodeConfig, error) {
	cm := NewConfigManager(appName)
	return cm.LoadNodeConfig()
}

// LoadNodeConfigFromDir loads host config with priority: flag > env var > default discovery
func (cm *ConfigManager) LoadNodeConfigFromDir(configDir string) (*NodeConfig, error) {
	// Priority 1: Use config directory from flag if provided
	if configDir != "" {
		return cm.loadNodeConfigFromSpecificDir(configDir)
	}

	// Priority 2: Use ZZROUTER_CONFIG_DIR environment variable if set
	if envDir := os.Getenv("ZZROUTER_CONFIG_DIR"); envDir != "" {
		return cm.loadNodeConfigFromSpecificDir(envDir)
	}

	// Priority 3: Use default discovery (current dir, then user config dir)
	return cm.LoadNodeConfig()
}

// loadNodeConfigFromSpecificDir loads host config from a specific directory
func (cm *ConfigManager) loadNodeConfigFromSpecificDir(dir string) (*NodeConfig, error) {
	// Ensure directory exists
	if err := cm.EnsureDir(dir); err != nil {
		return nil, fmt.Errorf("failed to create config directory %s: %w", dir, err)
	}

	configFile := filepath.Join(dir, hostConfigFileName)

	// If config doesn't exist in the specified directory, create default
	if !fileExists(configFile) {
		return createNodeConfigFile(configFile)
	}

	return readNodeConfigFile(configFile)
}

// nodeConfigDecodeHook composes viper's two default hooks with
// stringToClusterPeerHook.
//
// Passing viper.DecodeHook REPLACES the default rather than adding to
// it, so the duration and comma-slice hooks must be restated here or
// every `timeout: 30s` in node.yaml stops decoding.
func nodeConfigDecodeHook() mapstructure.DecodeHookFunc {
	return mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
		stringToClusterPeerHook(),
	)
}

// stringToClusterPeerHook accepts a bare address where a
// ClusterPeer is expected, so a membership list written before
// names existed still loads:
//
//	endpoints:
//	  - 192.0.2.10:9090            # -> {Address: "192.0.2.10:9090"}
//	  - address: 198.51.100.235:9090  # -> {Address: ..., Name: ...}
//	    name: WINDOWS-WORKER
func stringToClusterPeerHook() mapstructure.DecodeHookFuncType {
	return func(from, to reflect.Type, data any) (any, error) {
		if from.Kind() != reflect.String || to != reflect.TypeOf(ClusterPeer{}) {
			return data, nil
		}
		addr, _ := data.(string)
		return ClusterPeer{Address: addr}, nil
	}
}
