//go:build windows

package recovery

import (
	"errors"

	"golang.org/x/sys/windows"
)

// osDiskFull reports the platform's error for a write the disk had no room for.
func osDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}
