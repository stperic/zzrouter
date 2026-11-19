//go:build !windows

package recovery

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// RequireOwner refuses, as not_owner, a recovery command that touches the
// installation in dir unless this process runs as the owner of that directory,
// read without following a link, and refuses a directory that is a link: the
// service account controls that directory, so a privileged process acting
// there could be steered outside it. rerun is the command to repeat as the
// owner.
func RequireOwner(dir, rerun string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !ok:
		return fmt.Errorf("cannot read the owner of %s", dir)
	case info.Mode()&fs.ModeSymlink != 0:
		return &Refusal{NotOwner, dir + " is a link", "Name the directory it points to with --dir, as its owner."}
	case int(st.Uid) == os.Geteuid():
		return nil
	}
	owner := strconv.FormatUint(uint64(st.Uid), 10)
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	return &Refusal{NotOwner, fmt.Sprintf("%s belongs to %s, and this command runs as uid %d", dir, owner, os.Geteuid()),
		"Run it as the installation's owner: sudo -u " + owner + " " + rerun}
}
