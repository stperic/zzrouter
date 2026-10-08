package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrInstallDirNotWritable reports that this node cannot replace its own
// binary where that binary lives.
//
// Worth its own error because the two causes are both invisible from
// the message the syscall gives you. A systemd unit with
// ProtectSystem=strict mounts the install directory read-only unless it
// is named in ReadWritePaths, and a root-owned directory such as
// /usr/local/bin stays unwritable to the service user no matter what the
// sandbox allows. Either way the node can download and verify a release
// perfectly and then fail on the rename.
var ErrInstallDirNotWritable = errors.New("this node cannot replace its own binary")

// checkWritable reports whether the directory holding binaryPath can be
// written to. Replacing a binary is a rename within its directory, so
// directory permission is what matters, not the file's.
//
// Checked before the download rather than discovered during the swap:
// an operator who learns this from `update status` can fix it before a
// release lands, and a scheduler that learns it up front stops pulling
// artifacts it will not be able to install.
func checkWritable(binaryPath string) error {
	return checkDirWritable(filepath.Dir(binaryPath))
}

// checkDirWritable probes one directory. Split out because a managed
// install has two that matter — where a new version is written and
// where the symlink swap happens — and they can fail independently.
func checkDirWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".zzrouter-update-probe-*")
	if err != nil {
		return fmt.Errorf("%w: %s is not writable (%v). A systemd unit needs this directory in ReadWritePaths, and the directory itself must be owned by the user the service runs as",
			ErrInstallDirNotWritable, dir, err)
	}

	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}
