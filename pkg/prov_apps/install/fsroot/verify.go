package fsroot

import (
	"fmt"
	"os"
	"regexp"
)

// versionRegex allows only PEP 440 / semver-style version strings.
// Blocks shell metacharacters that could lead to command injection when
// the version is interpolated into pip install commands via sh -c.
var versionRegex = regexp.MustCompile(`^[a-zA-Z0-9._+\-]+$`)

// ValidateVersion checks that a version string is safe for use in shell commands.
func ValidateVersion(version string) error {
	if version == "" {
		return nil
	}
	if len(version) > 128 {
		return fmt.Errorf("version string too long (%d chars)", len(version))
	}
	if !versionRegex.MatchString(version) {
		return fmt.Errorf("invalid version %q: must contain only alphanumeric, dot, hyphen, underscore, plus", version)
	}
	return nil
}

// VerifySymlinkSafe checks that a path is not a symlink.
// Use before os.Rename to prevent symlink attacks.
func VerifySymlinkSafe(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // path doesn't exist yet, safe
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path %s is a symlink: refusing to overwrite (possible attack)", path)
	}
	return nil
}
