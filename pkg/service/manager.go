// Package service provides cross-platform service management for zzRouter.
// It handles service user creation, directory setup, and service installation
// across Linux (systemd), macOS (launchd), and Windows (SCM).
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
)

// ServiceName is the name of the zzRouter service
const ServiceName = "zzrouter"

// ServiceUser is the dedicated user for running the service (Linux only)
const ServiceUser = "zzrouter"

// Linux FHS-compliant directories - re-exported from config.Paths for convenience
// Use config.LinuxConfigDir, config.LinuxDataDir directly for new code
var (
	LinuxConfigDir = config.LinuxConfigDir
	LinuxDataDir   = config.LinuxDataDir
	LinuxLogDir    = config.LinuxLogDir
)

// ServiceManager provides cross-platform service management capabilities
type ServiceManager interface {
	// IsPrivileged returns true if running with elevated privileges (root/admin)
	IsPrivileged() bool

	// IsFirstRun returns true if this is the first time the service is being set up
	IsFirstRun() bool

	// IsServiceUser returns true if running as the service user
	IsServiceUser() bool

	// CreateServiceUser creates a dedicated service user (Linux only, no-op on other platforms)
	CreateServiceUser() error

	// SetupDirectories creates required directories with proper permissions
	SetupDirectories() error

	// InstallService installs the service into the system service manager
	InstallService() error

	// UninstallService removes the service from the system service manager
	UninstallService() error

	// StartService starts the service via the system service manager
	StartService() error

	// StopService stops the service via the system service manager
	StopService() error

	// RestartService restarts the service via the system service manager
	RestartService() error

	// RestartsOnExit reports whether the installed service definition
	// starts the process again after it exits on its own. Self-updating
	// code needs this before it exits into what would otherwise be a
	// dead node; the string explains the "false" for operators.
	RestartsOnExit() (bool, string)

	// GetStatus returns the current service status
	GetStatus() (*Status, error)

	// GetLogPath returns the path to service logs
	GetLogPath() string

	// GetConfigDir returns the service configuration directory
	GetConfigDir() string

	// GetDataDir returns the service data directory
	GetDataDir() string

	// GenerateAPIKey generates and saves an API key to the environment file
	GenerateAPIKey() (string, error)

	// SwitchToServiceUser re-executes the current process as the service user
	SwitchToServiceUser() error
}

// Status represents the current state of the service
type Status struct {
	// Running indicates if the service is currently running
	Running bool

	// Enabled indicates if the service is enabled to start at boot
	Enabled bool

	// PID is the process ID of the running service (0 if not running)
	PID int

	// User is the user the service is running as
	User string

	// Uptime is the duration the service has been running (human-readable)
	Uptime string

	// ConfigDir is the configuration directory being used
	ConfigDir string

	// LogPath is the path to service logs
	LogPath string

	// Error contains any error message if the service failed
	Error string
}

// NewServiceManager returns a platform-specific ServiceManager implementation
// This function is implemented in platform-specific files:
// - systemd.go (Linux)
// - launchd.go (macOS)
// - windows.go (Windows)
// - manager_other.go (fallback)

// Helper functions used by platform-specific implementations

// generateSecureKey generates a cryptographically secure API key
func generateSecureKey() (string, error) {
	return config.GenerateAPIKey()
}

// writeEnvFile writes environment variables to a .env file.
//
// The file contains secrets (admin/cluster API keys). On Unix the 0600 mode
// bits restrict access to the owner. On Windows, where mode bits are ignored
// by the filesystem, restrictEnvFilePerms applies a DACL that grants access
// only to the current user, SYSTEM, and BUILTIN\Administrators.
func writeEnvFile(path string, vars map[string]string) error {
	var content strings.Builder
	for key, value := range vars {
		content.WriteString(fmt.Sprintf("%s=%s\n", key, value))
	}
	if err := os.WriteFile(path, []byte(content.String()), 0600); err != nil {
		return err
	}
	return restrictEnvFilePerms(path)
}

// readEnvFile reads environment variables from a file
func readEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	vars := make(map[string]string)
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			vars[parts[0]] = parts[1]
		}
	}
	return vars, nil
}

// getExecutablePath returns the path to the current executable
func getExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}
	return filepath.EvalSymlinks(exe)
}
