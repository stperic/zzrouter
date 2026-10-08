package utils

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ErrPostRenameDurability: rename succeeded, parent-dir fsync didn't.
// Data is on disk; survival across power loss is unverified. Distinct
// sentinel so callers don't retry a save that already succeeded.
var ErrPostRenameDurability = errors.New("post-rename parent directory fsync skipped")

// AtomicWriteFile writes data to path durably and atomically via
// tempfile → fsync → chmod → rename → parent-dir fsync. Without the
// fsyncs the rename can be durable while the data isn't — on ext4 /
// xfs / btrfs that leaves path pointing at an empty inode after power
// loss. perm is the destination mode; on Windows only owner-write is
// honored.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return atomicWriteFileWithRename(path, data, perm, os.Rename)
}

func atomicWriteFileWithRename(path string, data []byte, perm os.FileMode, rename func(string, string) error) error {
	dir := filepath.Dir(path)
	// Hidden, so a directory scan never takes an in-flight or
	// crash-orphaned write for a real file.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	// Chmod before rename: a reader of the new path must never observe
	// a wider mode than intended.
	if err := os.Chmod(tmpPath, perm); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := rename(tmpPath, path); err != nil {
		// A refused replacement must not delete the previous destination.
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	// Windows: opening a directory for Sync returns EINVAL; NTFS
	// journaling covers the rename's durability.
	if runtime.GOOS != "windows" {
		d, err := os.Open(dir)
		if err == nil {
			_ = d.Sync()
			_ = d.Close()
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %v", ErrPostRenameDurability, err)
		}
	}
	return nil
}
