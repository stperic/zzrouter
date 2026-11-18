//go:build !windows

package security

import "os"

// RestrictFilePermissions limits a secret file to its owner, including existing files.
func RestrictFilePermissions(path string) error { return os.Chmod(path, 0o600) }

// RestrictDirectoryPermissions protects an application-owned secret directory.
func RestrictDirectoryPermissions(path string) error { return os.Chmod(path, 0o700) }
