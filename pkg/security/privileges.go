// Package security provides security utilities for zzRouter
package security

import (
	"fmt"
	"os"
	"runtime"
)

// CheckNotRoot verifies the process is not running as root/Administrator.
// This is a security best practice - zzRouter doesn't need elevated privileges
// and running as root increases the blast radius of any vulnerability.
//
// Returns nil if running as non-root, error with guidance if running as root.
func CheckNotRoot() error {
	// Skip check on Windows - different privilege model
	if runtime.GOOS == "windows" {
		return nil
	}

	if os.Getuid() == 0 {
		return fmt.Errorf(`zzrouter-node: refusing to run as root

Running as root is a security risk. zzRouter only needs:
  - Network port >= 1024 (no privileged ports)
  - Read/write access to config directory
  - GPU access (works with standard user permissions)

To fix this, create a dedicated service user:

  # Create user (Linux)
  sudo useradd -r -s /sbin/nologin -d /opt/zzrouter zzrouter

  # Create directories
  sudo mkdir -p /opt/zzrouter/{config,logs,models}
  sudo chown -R zzrouter:zzrouter /opt/zzrouter

  # Run as zzrouter user
  sudo -u zzrouter zzrouter-node run

Or use the systemd service file which handles this automatically:
  sudo systemctl start zzrouter-node

To bypass this check (NOT RECOMMENDED):
  export ZZROUTER_ALLOW_ROOT=1`)
	}

	return nil
}

// CheckNotRootWithBypass checks for root but allows bypass via environment variable.
// This is useful for development or containerized environments where running as
// root may be unavoidable.
func CheckNotRootWithBypass() error {
	// Check if bypass is explicitly enabled
	if os.Getenv("ZZROUTER_ALLOW_ROOT") == "1" {
		// Log warning but allow
		fmt.Fprintln(os.Stderr, "WARNING: Running as root with ZZROUTER_ALLOW_ROOT=1 bypass enabled")
		fmt.Fprintln(os.Stderr, "This is not recommended for production use")
		return nil
	}

	return CheckNotRoot()
}

// SetSecureUmask sets a restrictive umask for file creation.
// This ensures files created by zzRouter are not world-readable by default.
//
// umask 0027 means:
//   - Owner: full permissions (rwx)
//   - Group: read and execute (r-x)
//   - Other: no permissions (---)
//
// This is appropriate for a daemon that may create config files with secrets.
func SetSecureUmask() int {
	// Skip on Windows - different permission model
	if runtime.GOOS == "windows" {
		return 0
	}

	// Use syscall for umask - need to use platform-specific implementation
	return setUmask(0027)
}
