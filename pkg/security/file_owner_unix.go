//go:build !windows

package security

import (
	"os"
	"syscall"
)

// GiveOwner gives path the owner and group of reference, unless this process
// already runs as reference's owner, so what a privileged command writes into
// an installation stays usable by the installation's account. Skipping the
// call that would change nothing also keeps a service clear of a seccomp
// filter on chown.
func GiveOwner(path, reference string) error {
	info, err := os.Stat(reference)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) == os.Geteuid() {
		return nil
	}
	return os.Lchown(path, int(owner.Uid), int(owner.Gid))
}
