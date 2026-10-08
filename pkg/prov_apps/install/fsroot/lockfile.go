//go:build unix

package fsroot

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile provides per-provider flock-based install locking.
// Only one install operation per provider can run at a time.
type LockFile struct {
	path string
	file *os.File
}

// DefaultLockDir returns the directory for install lock files.
func DefaultLockDir() string {
	// Prefer /var/run/zzrouter if available (systemd RuntimeDirectory)
	if info, err := os.Stat("/var/run/zzrouter"); err == nil && info.IsDir() {
		return "/var/run/zzrouter"
	}
	// Fallback to temp dir
	return os.TempDir()
}

// NewLockFile creates a lock file for the given provider.
func NewLockFile(provider string) *LockFile {
	dir := DefaultLockDir()
	return &LockFile{
		path: filepath.Join(dir, fmt.Sprintf("install-%s.lock", provider)),
	}
}

// Lock acquires an exclusive lock. Returns error if already locked.
func (l *LockFile) Lock() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0640)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return ErrInstallInProgress
	}

	l.file = f
	return nil
}

// Unlock releases the lock.
func (l *LockFile) Unlock() {
	if l.file != nil {
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		_ = l.file.Close()
		_ = os.Remove(l.path)
		l.file = nil
	}
}
