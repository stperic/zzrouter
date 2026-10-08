// Package config provides OS-agnostic configuration file management for zzRouter
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kirsle/configdir"
)

// Constants for configuration management
const (
	clientConfigFileName = "cli.yaml"
	hostConfigFileName   = "node.yaml"
)

// ConfigManager handles OS-agnostic configuration file operations.
// It delegates path resolution to PathResolver for consistency.
type ConfigManager struct {
	appName string
	paths   *PathResolver
}

// NewConfigManager creates a new configuration manager.
func NewConfigManager(appName string) *ConfigManager {
	return &ConfigManager{
		appName: appName,
		paths:   NewPathResolver(appName),
	}
}

// --- Client Config (XDG only, no service mode) ---

// GetClientConfigDir returns the client-specific config directory.
func (cm *ConfigManager) GetClientConfigDir() string {
	return configdir.LocalConfig(cm.appName)
}

// GetClientConfigPath returns the full path to the client config file.
func (cm *ConfigManager) GetClientConfigPath() string {
	return filepath.Join(cm.GetClientConfigDir(), clientConfigFileName)
}

// FindClientConfigFile returns the client config path if it exists, empty string otherwise.
func (cm *ConfigManager) FindClientConfigFile() string {
	path := cm.GetClientConfigPath()
	if fileExists(path) {
		return path
	}
	return ""
}

// --- Node Config (uses PathResolver for service mode awareness) ---

// GetNodeConfigDir returns the host-specific config directory.
// Delegates to PathResolver for platform/context awareness.
func (cm *ConfigManager) GetNodeConfigDir() string {
	return cm.paths.GetConfigDir()
}

// GetNodeDataDir returns the data directory for models, logs, and runtime data.
// Delegates to PathResolver for platform/context awareness.
func (cm *ConfigManager) GetNodeDataDir() string {
	return cm.paths.GetDataDir()
}

// GetNodeConfigPath returns the full path to the host config file.
func (cm *ConfigManager) GetNodeConfigPath() string {
	return cm.paths.GetNodeConfigPath()
}

// GetAppsConfigDir returns the full path to the providers/ directory.
func (cm *ConfigManager) GetAppsConfigDir() string {
	return cm.paths.GetAppsConfigDir()
}

// FindNodeConfigFile returns the node config path if it exists, empty string otherwise.
func (cm *ConfigManager) FindNodeConfigFile() string {
	path := cm.GetNodeConfigPath()
	if fileExists(path) {
		return path
	}
	return ""
}

// FindAppsConfigDir returns the providers/ directory path if it exists,
// empty string otherwise.
func (cm *ConfigManager) FindAppsConfigDir() string {
	path := cm.GetAppsConfigDir()
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return path
	}
	return ""
}

// --- Directory Management ---

// EnsureDir creates a directory if it doesn't exist.
func (cm *ConfigManager) EnsureDir(dir string) error {
	return configdir.MakePath(dir)
}

// --- Utility Functions ---

// fileExists checks if a file exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ErrFilePermsTooOpen: config file is world-readable. Override with
// env ZZROUTER_ALLOW_INSECURE_PERMS=1 (literal "1" only).
var ErrFilePermsTooOpen = errors.New("config file permissions too open")

// requireSafePerms refuses to proceed when path is world-readable. Nil
// on missing file (caller's read will surface that), tight perms, or
// env override. Windows delegates to permcheck_windows.go since the
// synthesized POSIX mode there doesn't reflect NTFS DACL.
func requireSafePerms(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil //nolint:nilerr // missing file is handled downstream by the caller's read
	}
	if !isWorldReadable(info) {
		return nil
	}
	if os.Getenv("ZZROUTER_ALLOW_INSECURE_PERMS") == "1" {
		return nil
	}
	return fmt.Errorf("%w: %s has mode %v (chmod 600 %s, or set ZZROUTER_ALLOW_INSECURE_PERMS=1 to override)",
		ErrFilePermsTooOpen, path, info.Mode(), path)
}
