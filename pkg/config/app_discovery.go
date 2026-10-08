// Package config provides OS-agnostic configuration file management for zzRouter
// App discovery and version detection types
package config

// AppDiscovery represents discovery rules in v2.0 format
type AppDiscovery struct {
	Detection []AppDetectionRule `yaml:"detection"`
	Runtime   []AppRuntimeRule   `yaml:"runtime"`
}

// AppDetectionRule represents a detection rule
type AppDetectionRule struct {
	Method    string   `yaml:"method"`
	Target    string   `yaml:"target"`
	Priority  int      `yaml:"priority"`
	Platforms []string `yaml:"platforms"`

	// Version extraction (optional, method-specific)
	VersionCommand string `yaml:"version_command,omitempty"` // For executables: command arg (e.g., "--version")
	VersionPath    string `yaml:"version_path,omitempty"`    // For HTTP: JSON path (e.g., "version" or "build.version")
	VersionPattern string `yaml:"version_pattern,omitempty"` // For text extraction: regex pattern with capture group (e.g., "version: (.+)")
}

// AppRuntimeRule represents a runtime detection rule
type AppRuntimeRule struct {
	Method    string   `yaml:"method"`
	Target    string   `yaml:"target"`
	Priority  int      `yaml:"priority"`
	Platforms []string `yaml:"platforms"`
}
