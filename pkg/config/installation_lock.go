package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/stperic/common/atomicfile"
)

// LockInstallation fences foreground startup and offline recovery. Keep the
// file in place: removing a lock inode would allow a second independent owner.
func LockInstallation(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := atomicfile.Lock(filepath.Join(dir, "installation.lock"))
	if err != nil {
		return nil, fmt.Errorf("installation is in use; stop the node before maintenance: %w", err)
	}
	return lock, nil
}
