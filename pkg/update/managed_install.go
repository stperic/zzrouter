package update

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/version"
)

// ManagedRoot is where a Linux service install keeps its binaries.
//
// The same directory root already carries the provider installs, so a
// machine-wide zzRouter is one tree rather than binaries in /usr/local
// and everything else in /opt.
func ManagedRoot() string { return config.LinuxRootDir }

// ManagedNodePath is the stable path to the node binary on a managed
// install: a symlink that always names the running version. This is
// what a unit file's ExecStart should point at, never a version
// directory, which changes under it on every update.
func ManagedNodePath() string {
	return filepath.Join(ManagedRoot(), config.SubdirBin, nodeBinaryName)
}

// ErrNotRelocatable reports that the binaries could not be moved into
// the managed layout, so the install stays where it is. An unmanaged
// install still runs; it just cannot update itself without help.
var ErrNotRelocatable = errors.New("cannot place the binaries in the managed layout")

// EnsureManagedLayout moves a service install's binaries into
// <root>/versions/<version>/bin and leaves symlinks behind.
//
// Called once, during service installation, as root. Afterwards:
//
//	/opt/zzrouter/versions/0.1.1/bin/zzrouter-node   the real file
//	/opt/zzrouter/bin/zzrouter-node                  -> ../versions/0.1.1/bin/...
//	/usr/local/bin/zzrouter-node                     -> /opt/zzrouter/bin/...
//
// The /usr/local/bin symlink is not cosmetic. Installer.getCurrentExecutable
// resolves symlinks, so operator and service reach one real file and
// cannot drift onto different versions -- which two real copies would
// guarantee the first time the node updated itself.
//
// It returns the stable path the unit file should run, and reports
// whether anything moved. Doing nothing is a normal outcome: a
// re-install finds the layout already in place.
func EnsureManagedLayout() (nodePath string, relocated bool, err error) {
	if runtime.GOOS != "linux" {
		return "", false, fmt.Errorf("%w: only Linux service installs use it", ErrNotRelocatable)
	}

	exe, err := os.Executable()
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
	}

	// Already managed: a re-install, or a node started from the tree it
	// was relocated into last time. Still re-assert the permissions
	// rather than returning early, so this doubles as a repair -- a
	// directory left un-traversable by the umask is invisible until
	// systemd answers 203/EXEC, and the operator's move is to run this
	// again.
	if l := detectLayout(resolved); l != nil {
		if err := repairPermissions(l, filepath.Dir(resolved)); err != nil {
			return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
		}
		return ManagedNodePath(), false, nil
	}

	ver := version.Current.String()
	l := &layout{root: ManagedRoot()}
	verBinDir := l.versionBinDir(ver)
	if err := mkdirShared(l.root, verBinDir); err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
	}

	// Copied, not moved. The binary being copied is the one running
	// this code, and the operator's shell is very likely sitting in the
	// directory it came from; leaving the original in place until the
	// symlink is ready means an interrupted relocation degrades to "no
	// change" rather than "no binary".
	for _, name := range managedBinaryNames {
		src := filepath.Join(filepath.Dir(resolved), name)
		if _, statErr := os.Stat(src); statErr != nil {
			if name == launcherBinaryName {
				slog.Warn("no launcher next to the node binary; provider processes will not be tracked",
					"looked_in", filepath.Dir(resolved))
				continue
			}
			return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, statErr)
		}
		dst := filepath.Join(verBinDir, name)
		if err := copyFileWithSync(src, dst); err != nil {
			return "", false, fmt.Errorf("%w: place %s: %w", ErrNotRelocatable, dst, err)
		}
		if err := os.Chmod(dst, binaryFileMode); err != nil {
			return "", false, fmt.Errorf("%w: make %s executable: %w", ErrNotRelocatable, dst, err)
		}
	}

	if err := l.activate(ver); err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
	}

	// Replace the originals with links to the managed copies, so there
	// is exactly one real binary per version on the machine.
	for _, name := range managedBinaryNames {
		original := filepath.Join(filepath.Dir(resolved), name)
		if _, statErr := os.Stat(original); statErr != nil {
			continue
		}
		target := filepath.Join(l.binDir(), name)
		if original == target {
			continue
		}
		if err := replaceSymlink(target, original); err != nil {
			// Not fatal: the managed copy is in place and the unit file
			// points at it. What is lost is the operator's PATH still
			// reaching the same file, which is worth a loud warning and
			// not worth failing an install over.
			slog.Warn("could not replace the original binary with a link to the managed one; "+
				"the CLI on your PATH and the service may now be different versions",
				"path", original, "target", target, "err", err)
		}
	}

	slog.Info("placed the node binaries in the managed layout", "version", ver, "root", l.root)
	return ManagedNodePath(), true, nil
}

// repairPermissions makes an install tree traversable again.
//
// Every version directory, not just the running one: a rollback targets
// a directory installed earlier, and one left un-traversable would turn
// the recovery path into the same 203/EXEC restart loop it was meant to
// escape -- at the worst possible moment, since a rollback only happens
// when something has already gone wrong.
func repairPermissions(l *layout, activeBinDir string) error {
	if err := mkdirShared(l.root, l.binDir()); err != nil {
		return err
	}
	if err := mkdirShared(l.root, activeBinDir); err != nil {
		return err
	}
	for _, ver := range l.installedVersions() {
		if err := mkdirShared(l.root, l.versionBinDir(ver)); err != nil {
			return err
		}
	}
	return nil
}
