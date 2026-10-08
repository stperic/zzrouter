//go:build unix

package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// DiskSpace verifies the target directory has enough free space.
// If the directory doesn't exist yet, it walks up to the nearest existing ancestor.
func DiskSpace(dir string, requiredBytes uint64) error {
	check := dir
	for {
		if _, err := os.Stat(check); err == nil {
			break
		}
		parent := filepath.Dir(check)
		if parent == check {
			break
		}
		check = parent
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(check, &stat); err != nil {
		return fmt.Errorf("failed to check disk space at %s: %w", dir, err)
	}
	available := stat.Bavail * uint64(stat.Bsize)
	if available < requiredBytes {
		return fmt.Errorf("insufficient disk space: %d MB available, %d MB required",
			available/(1024*1024), requiredBytes/(1024*1024))
	}
	return nil
}

// WritePermission verifies the process can write to the target directory.
// If the directory doesn't exist yet, it checks the nearest existing ancestor.
func WritePermission(dir string) error {
	// Walk up to find an existing directory
	check := dir
	for {
		info, err := os.Stat(check)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("path %s exists but is not a directory", check)
			}
			break
		}
		parent := filepath.Dir(check)
		if parent == check {
			// Reached filesystem root
			break
		}
		check = parent
	}

	// Try to create a temp file to verify write access
	probe := filepath.Join(check, ".zzrouter-write-probe")
	f, err := os.Create(probe)
	if err != nil {
		return fmt.Errorf("%w on %s: %w", ErrNoWritePermission, check, err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return nil
}

// MSVCRedist is a no-op on Unix — Visual C++ redistributable is a
// Windows-only concern. Returns a passing Result so callers can include
// the check unconditionally without platform branching at the call site.
func MSVCRedist() Result {
	return Result{
		Check:   "msvc-redist",
		Passed:  true,
		Message: "skipped: MSVC redistributable is a Windows-only check",
	}
}
