//go:build unix

package security

import "syscall"

// setUmask sets the umask on Unix systems
func setUmask(mask int) int {
	return syscall.Umask(mask)
}
