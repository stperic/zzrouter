package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/stperic/zzrouter/pkg/version"
)

// ErrVersionAlreadyActive reports an install of the version already
// running. Refused rather than treated as a no-op: writing the version
// directory would mean writing the directory the running process is
// executing out of, which is the one thing the managed layout exists to
// avoid.
var ErrVersionAlreadyActive = errors.New("that version is already the active one")

// errManagedNeedsVersion reports an install into a managed layout with
// no version to name the directory after.
var errManagedNeedsVersion = errors.New("a managed install needs the version the release carries")

// binaryFileMode is the permission a managed install gives the binaries
// it writes. World-executable so the operator's CLI can run the same
// file the service does; writable only by the owner, which is root on a
// managed install and is the whole point of the layout.
const binaryFileMode = 0755

// installManaged writes a new version directory and moves the symlink
// onto it.
//
// Ordering is what makes this safe to interrupt. Everything up to
// activate is invisible to the running node: a crash leaves an unlinked
// directory that the next prune collects. The update happens in
// activate's rename, and everything after it is bookkeeping that costs
// a rollback target at worst, never a working node.
func (i *Installer) installManaged(ctx context.Context, l *layout, archivePath string, expected *version.Version) (*InstallResult, error) {
	if expected == nil {
		return nil, errManagedNeedsVersion
	}
	newVer := expected.String()

	active, activeErr := l.activeVersion()
	if activeErr != nil {
		// Worth continuing: the install itself does not need to know
		// what came before, only the rollback stack and the prune do.
		// Both are skipped below rather than guessed at.
		slog.Warn("could not read which version is active; installing anyway, but this update will not be rollback-able",
			"err", activeErr)
	}
	if active != "" && active == newVer {
		return nil, fmt.Errorf("%w: %s", ErrVersionAlreadyActive, newVer)
	}

	if err := i.checkManagedWritable(l); err != nil {
		return nil, err
	}

	// Stage into the download's temp directory and prove the binary
	// runs before any part of the install tree is touched.
	extractedNode, err := i.extractBinary(archivePath, nodeBinaryName)
	if err != nil {
		return nil, fmt.Errorf("failed to extract binary: %w", err)
	}
	if err := smokeTest(ctx, extractedNode, expected); err != nil {
		return nil, err
	}
	extractedLauncher, err := i.extractLauncher(archivePath, launcherBinaryName)
	if err != nil {
		return nil, err
	}

	// A version directory that is not the active one is not being
	// executed by anything, so a half-written leftover from an
	// interrupted install is safe to discard and rewrite.
	verBinDir := l.versionBinDir(newVer)
	if err := os.RemoveAll(filepath.Dir(verBinDir)); err != nil {
		return nil, fmt.Errorf("clear stale version directory: %w", err)
	}
	if err := mkdirShared(l.root, verBinDir); err != nil {
		return nil, err
	}

	staged := map[string]string{nodeBinaryName: extractedNode}
	if extractedLauncher != "" {
		staged[launcherBinaryName] = extractedLauncher
	}
	for name, src := range staged {
		dst := filepath.Join(verBinDir, name)
		// Copied rather than renamed: the staging directory is under
		// the node's data dir and the install tree is usually under
		// /opt, which are routinely separate mounts.
		if err := copyFileWithSync(src, dst); err != nil {
			return nil, fmt.Errorf("place %s: %w", dst, err)
		}
		if err := os.Chmod(dst, binaryFileMode); err != nil {
			return nil, fmt.Errorf("make %s executable: %w", dst, err)
		}
	}

	// A release that ships no launcher of its own still needs one next
	// to its node binary, because the node looks for it beside itself.
	if extractedLauncher == "" {
		i.carryForwardLauncher(l, active, verBinDir)
	}

	if err := l.activate(newVer); err != nil {
		return nil, fmt.Errorf("failed to activate %s: %w", newVer, err)
	}

	result := &InstallResult{
		Success:         true,
		NewPath:         filepath.Join(l.binDir(), nodeBinaryName),
		Version:         newVer,
		PreviousVersion: active,
	}
	// Gated on the launcher, not on hasVersion, which asks about the
	// node binary and would report a launcher path when none was
	// installed or carried forward.
	if _, err := os.Stat(filepath.Join(verBinDir, launcherBinaryName)); err == nil {
		result.LauncherPath = filepath.Join(l.binDir(), launcherBinaryName)
	} else {
		slog.Warn("this version has no launcher beside its node binary; provider processes will not be tracked",
			"version", newVer, "dir", verBinDir)
	}

	if activeErr == nil {
		state := l.loadState()
		state.Previous = pushVersion(state.Previous, active)
		// Prune reads the full stack to decide what is reachable, and
		// forget then drops whatever it removed. Saving between the two
		// would leave the stack naming a directory prune had deleted,
		// so a rollback would aim at a version that is not there.
		l.prune(newVer, state, i.keepPreviousVersions)
		l.forget(state)
		if err := l.saveState(state); err != nil {
			slog.Warn("update installed, but the rollback history could not be saved", "err", err)
		}
	}

	return result, nil
}

// carryForwardLauncher copies the active version's launcher into a new
// version directory.
//
// Needed because the node finds its launcher beside its own executable,
// and in this layout "beside" is the new version directory. A release
// that ships no launcher would otherwise land in a directory with no
// launcher at all, which costs every provider process its PID tracking
// — and silently, since an absent launcher is skipped rather than
// reported. Copying the previous one is right: a release built without
// a launcher also embeds no launcher hash to check it against.
func (i *Installer) carryForwardLauncher(l *layout, active, verBinDir string) {
	if active == "" {
		// Nothing installed before this, so there is no launcher to
		// carry. The caller reports the consequence.
		return
	}
	src := filepath.Join(l.versionBinDir(active), launcherBinaryName)
	if _, err := os.Stat(src); err != nil {
		return
	}
	dst := filepath.Join(verBinDir, launcherBinaryName)
	if err := copyFileWithSync(src, dst); err != nil {
		slog.Warn("could not carry the launcher forward to the new version", "from", src, "err", err)
		return
	}
	if err := os.Chmod(dst, binaryFileMode); err != nil {
		slog.Warn("could not make the carried-forward launcher executable", "path", dst, "err", err)
	}
}

// rollbackManaged points the symlink back at the version this install
// ran before the active one.
//
// Nothing is copied and nothing is deleted: the target directory has
// been sitting untouched since it was installed, so the rollback is the
// same rename the install was, in the other direction.
func (i *Installer) rollbackManaged(l *layout) (*InstallResult, error) {
	state := l.loadState()
	l.forget(state)
	if len(state.Previous) == 0 {
		return nil, ErrNoBackupsAvailable
	}

	target := state.Previous[len(state.Previous)-1]
	active, err := l.activeVersion()
	if err != nil {
		slog.Warn("could not read which version is active before rolling back", "err", err)
	}

	if err := l.activate(target); err != nil {
		return nil, fmt.Errorf("failed to activate %s: %w", target, err)
	}

	// Popped only after the symlink moved. Popping first and then
	// failing to activate would lose the rollback target while leaving
	// the node on the version it was trying to leave.
	state.Previous = state.Previous[:len(state.Previous)-1]
	if err := l.saveState(state); err != nil {
		slog.Warn("rolled back, but the rollback history could not be saved", "err", err)
	}

	return &InstallResult{
		Success:         true,
		NewPath:         filepath.Join(l.binDir(), nodeBinaryName),
		LauncherPath:    filepath.Join(l.binDir(), launcherBinaryName),
		Version:         target,
		PreviousVersion: active,
	}, nil
}

// pushVersion appends ver to the rollback stack, keeping it free of the
// repeats that install/rollback/install cycles would otherwise pile up.
func pushVersion(stack []string, ver string) []string {
	if ver == "" {
		return stack
	}
	kept := make([]string, 0, len(stack)+1)
	for _, v := range stack {
		if v != ver {
			kept = append(kept, v)
		}
	}
	return append(kept, ver)
}

// checkManagedWritable reports whether this process can write the two
// directories a managed install touches.
//
// Both are checked because they fail for different reasons: the
// versions directory is where a new install is written, and the bin
// directory is where the symlink swap happens. On a systemd install
// both are root-owned and this is exactly what fails when the
// unprivileged node tries to update itself instead of handing the job
// to the privileged updater.
func (i *Installer) checkManagedWritable(l *layout) error {
	for _, dir := range []string{l.versionsDir(), l.binDir()} {
		// A directory that does not exist yet has to be created, and
		// whether that is possible is a question about its parent. The
		// root always exists: the running binary is underneath it.
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			dir = l.root
		}
		if err := checkDirWritable(dir); err != nil {
			return err
		}
	}
	return nil
}

// IsManaged reports whether the running binary sits in a managed
// layout, where the privileged updater rather than this process owns
// every write to the install tree.
func (i *Installer) IsManaged() bool {
	exe, err := i.getCurrentExecutable()
	if err != nil {
		return false
	}
	return detectLayout(exe) != nil
}

// CheckWritable answers "could this node install an update at all",
// without needing a release to have shown up first.
func (i *Installer) CheckWritable() error {
	exe, err := i.getCurrentExecutable()
	if err != nil {
		return err
	}
	if l := detectLayout(exe); l != nil {
		return i.checkManagedWritable(l)
	}
	return checkWritable(exe)
}
