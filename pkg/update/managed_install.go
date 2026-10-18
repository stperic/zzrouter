package update

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/stperic/zzinstall/install"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/security"
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

	originalBinDir := filepath.Dir(resolved)
	if detectLayout(resolved) != nil {
		originalBinDir = "/usr/local/bin"
	}
	return ensureManagedLayout(&layout{root: ManagedRoot()}, resolved, originalBinDir, version.Current.String())
}

func ensureManagedLayout(l *layout, resolved, originalBinDir, ver string) (string, bool, error) {
	// Repair the active links, even when invoked from an older executable.
	if l := detectLayout(resolved); l != nil {
		active, err := l.activeVersion()
		if err != nil {
			return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
		}
		if err := repairPermissions(l, l.versionBinDir(active)); err != nil {
			return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
		}
		if err := l.activate(active); err != nil {
			return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
		}
		if err := l.linkOriginals(originalBinDir); err != nil {
			return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
		}
		return filepath.Join(l.binDir(), nodeBinaryName), false, nil
	}

	verBinDir := l.versionBinDir(ver)
	staged := ""
	if _, err := os.Lstat(filepath.Dir(verBinDir)); !os.IsNotExist(err) {
		if err != nil {
			return "", false, fmt.Errorf("%w: stat version directory: %w", ErrNotRelocatable, err)
		}
		if err := l.verifyRelocationSources(filepath.Dir(resolved), ver); err != nil {
			return "", false, fmt.Errorf("%w: cannot rewrite existing version directory %s: %w", ErrNotRelocatable, ver, err)
		}
	} else {
		if err := mkdirShared(l.root, l.versionsDir()); err != nil {
			return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
		}
		var err error
		staged, err = os.MkdirTemp(l.versionsDir(), ".relocate-")
		if err != nil {
			return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
		}
		defer func() {
			if err := os.RemoveAll(staged); err != nil {
				slog.Warn("could not clean up binary staging", "dir", staged, "err", err)
			}
		}()
		stagedBinDir := filepath.Join(staged, layoutBinDirName)
		if err := mkdirShared(l.root, stagedBinDir); err != nil {
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
				if os.IsNotExist(statErr) && !managedBinaryRequired(name) {
					slog.Warn("no companion next to the node binary", "binary", name, "looked_in", filepath.Dir(resolved))
					continue
				}
				return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, statErr)
			}
			dst := filepath.Join(stagedBinDir, name)
			if err := copyFileWithSync(src, dst); err != nil {
				return "", false, fmt.Errorf("%w: place %s: %w", ErrNotRelocatable, dst, err)
			}
			if err := os.Chmod(dst, binaryFileMode); err != nil {
				return "", false, fmt.Errorf("%w: make %s executable: %w", ErrNotRelocatable, dst, err)
			}
		}

		if err := os.Rename(staged, filepath.Dir(verBinDir)); err != nil {
			return "", false, fmt.Errorf("%w: publish version directory: %w", ErrNotRelocatable, err)
		}
	}
	if err := l.activate(ver); err != nil {
		if staged != "" && !l.linkedVersions()[ver] {
			// Put our unpublished version back under the staging cleanup.
			if cleanupErr := os.Rename(filepath.Dir(verBinDir), staged); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("recover failed relocation: %w", cleanupErr))
			}
		}
		return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
	}
	if err := l.linkOriginals(originalBinDir); err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrNotRelocatable, err)
	}

	slog.Info("placed the node binaries in the managed layout", "version", ver, "root", l.root)
	return filepath.Join(l.binDir(), nodeBinaryName), true, nil
}

// verifyRelocationSources permits resuming a published directory without rewriting it.
func (l *layout) verifyRelocationSources(sourceBinDir, ver string) error {
	for _, name := range managedBinaryNames {
		src := filepath.Join(sourceBinDir, name)
		dst := filepath.Join(l.versionBinDir(ver), name)
		digest, err := security.ComputeSHA256(src)
		if os.IsNotExist(err) && !managedBinaryRequired(name) {
			if _, err := os.Lstat(dst); os.IsNotExist(err) {
				continue
			}
		}
		if err != nil {
			return fmt.Errorf("read relocation source %s: %w", src, err)
		}
		if err := security.VerifySHA256(dst, digest); err != nil {
			return fmt.Errorf("compare published binary %s: %w", dst, err)
		}
	}
	return nil
}

// linkOriginals also replaces stale public files on an already-managed install.
func (l *layout) linkOriginals(originalBinDir string) error {
	for _, name := range managedBinaryNames {
		original := filepath.Join(originalBinDir, name)
		target := filepath.Join(l.binDir(), name)
		if _, err := os.Stat(target); err != nil {
			if os.IsNotExist(err) && !managedBinaryRequired(name) {
				slog.Warn("no managed companion; preserving the public binary", "binary", name, "path", original)
				continue
			}
			return fmt.Errorf("stat %s: %w", target, err)
		}
		if original == target {
			continue
		}
		if err := install.ReplaceSymlink(target, original); err != nil {
			return err
		}
	}
	return nil
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
