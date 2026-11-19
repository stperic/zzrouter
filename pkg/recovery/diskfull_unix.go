//go:build !windows

package recovery

import (
	"errors"
	"syscall"
)

// osDiskFull reports the platform's error for a write the disk had no room for.
func osDiskFull(err error) bool { return errors.Is(err, syscall.ENOSPC) }
