// Package config provides OS-agnostic configuration file management for zzRouter
// App validation and volume path security checks
package config

import (
	"path/filepath"
	"strings"
)

// resolveVolumePath resolves relative paths in volume specifications to absolute paths
func resolveVolumePath(volumeSpec, configDir string) string {
	// Parse volume spec (format: host:container or host:container:mode)
	parts := strings.Split(volumeSpec, ":")
	if len(parts) < 2 {
		return volumeSpec // Invalid format, return as-is for validation to catch
	}

	hostPath := parts[0]

	// Skip named volumes (they don't start with . or /)
	if !strings.HasPrefix(hostPath, ".") && !strings.HasPrefix(hostPath, "/") {
		return volumeSpec // Named volume, no resolution needed
	}

	// Skip already absolute paths
	if filepath.IsAbs(hostPath) {
		return volumeSpec
	}

	// Resolve relative path to absolute based on config directory
	absPath := filepath.Join(configDir, hostPath)
	absPath = filepath.Clean(absPath)

	// Reconstruct volume spec with absolute path
	parts[0] = absPath
	return strings.Join(parts, ":")
}
