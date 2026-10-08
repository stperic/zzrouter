//go:build windows

package fsroot

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// LockFile provides per-provider file-based install locking on Windows.
type LockFile struct {
	path string
	file *os.File
}

// DefaultLockDir returns the directory for install lock files.
func DefaultLockDir() string {
	return os.TempDir()
}

// NewLockFile creates a lock file for a provider.
func NewLockFile(provider string) *LockFile {
	dir := DefaultLockDir()
	return &LockFile{
		path: filepath.Join(dir, fmt.Sprintf("install-%s.lock", provider)),
	}
}

// Lock acquires an exclusive, non-blocking lock.
// Returns ErrInstallInProgress if another install is running.
func (l *LockFile) Lock() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}

	// LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY
	const (
		lockfileExclusiveLock   = 0x00000002
		lockfileFailImmediately = 0x00000001
	)

	ol := new(syscall.Overlapped)
	handle := syscall.Handle(f.Fd())

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	lockFileEx := kernel32.NewProc("LockFileEx")

	ret, _, _ := lockFileEx.Call(
		uintptr(handle),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0, // reserved
		1, // nNumberOfBytesToLockLow
		0, // nNumberOfBytesToLockHigh
		uintptr(unsafe.Pointer(ol)),
	)
	if ret == 0 {
		_ = f.Close()
		return ErrInstallInProgress
	}

	l.file = f
	return nil
}

// Unlock releases the lock.
func (l *LockFile) Unlock() {
	if l.file != nil {
		_ = l.file.Close()
		_ = os.Remove(l.path)
		l.file = nil
	}
}
