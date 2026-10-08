package process

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// FileMountProcessor handles file mount validation.
type FileMountProcessor struct{}

// NewFileMountProcessor creates a new file mount processor.
func NewFileMountProcessor() *FileMountProcessor {
	return &FileMountProcessor{}
}

// ValidateFileMount validates a single file mount.
func (fmp *FileMountProcessor) ValidateFileMount(file instance.FileMount) error {
	if file.Name == "" {
		return fmt.Errorf("file name is required")
	}
	if file.NodePath == "" {
		return fmt.Errorf("host path is required")
	}

	if !file.IsOutput {
		if err := validateNodePath(file.NodePath); err != nil {
			return err
		}
	} else {
		parentDir := filepath.Dir(file.NodePath)
		if _, err := os.Stat(parentDir); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("parent directory does not exist: %s", parentDir)
			}
			return fmt.Errorf("cannot access parent directory: %w", err)
		}
	}
	return nil
}

// checkPathSafety verifies that a resolved real path does not point to a dangerous
// system location (/etc, /root, /sys, /proc, /boot, /dev).
func checkPathSafety(realPath, originalPath string) error {
	for _, dangerous := range dangerousPaths {
		if strings.HasPrefix(realPath, dangerous+string(filepath.Separator)) || realPath == dangerous {
			return fmt.Errorf("path resolves to dangerous location: %s -> %s", originalPath, realPath)
		}
	}
	return nil
}

// validateNodePath validates that a host path exists and is safe.
// Checks for path traversal and dangerous symlinks.
func validateNodePath(path string) error {
	if strings.Contains(path, "..") {
		return fmt.Errorf("path traversal not allowed: %s", path)
	}

	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("path does not exist: %s", path)
		}
		return fmt.Errorf("cannot access path: %w", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("cannot resolve symlink: %w", err)
		}
		if err := checkPathSafety(realPath, path); err != nil {
			return err
		}
		if _, err := os.Stat(realPath); err != nil {
			return fmt.Errorf("symlink target does not exist: %s -> %s", path, realPath)
		}
	}

	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("cannot get absolute path: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return fmt.Errorf("path validation failed: %w", err)
	}

	return checkPathSafety(realPath, path)
}
