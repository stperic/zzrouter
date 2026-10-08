package update

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/version"
)

// A managed install keeps every version it has ever run in its own
// directory and names the running one with a symlink:
//
//	<root>/versions/<version>/bin/zzrouter-node
//	<root>/versions/<version>/bin/zzrouter-launcher
//	<root>/bin/zzrouter-node          -> ../versions/<version>/bin/zzrouter-node
//	/usr/local/bin/zzrouter-node      -> <root>/bin/zzrouter-node
//
// Installing writes a directory that nothing points at yet, and the
// update takes effect in the single rename that moves the symlink. That
// buys three things the in-place swap could not:
//
//   - Rollback is re-pointing a symlink at a directory that was never
//     modified, rather than restoring a copy. The whole class of bug
//     around naming, pairing and consuming backup files goes away with
//     the backup files.
//   - The version directory is immutable once written, so the launcher
//     and the node that embeds its hash cannot be observed apart. The
//     in-place path had a window between replacing the two.
//   - Nothing writes inside the directory the running process is
//     executing from, which is what lets the whole tree stay root-owned
//     while an unprivileged node keeps running out of it.
//
// The layout is discovered from the path of the running binary rather
// than configured, so it is described entirely by what is on disk and a
// node cannot be told it is managed when it is not.
const (
	layoutVersionsDirName = config.SubdirVersions
	layoutBinDirName      = config.SubdirBin
	layoutStateFileName   = "state.json"
)

// sharedDirMode is the permission every directory in the managed tree
// needs: writable only by its owner (root), traversable by everyone,
// because the unprivileged service user has to reach the binary it runs.
const sharedDirMode = 0755

// mkdirShared creates dir and makes every level from base down to it
// traversable by the service user.
//
// The chmod is not redundant with the mode passed to MkdirAll. The node
// sets umask 0027 at startup (security.SetSecureUmask), so MkdirAll(0755)
// actually produces 0750, the service user cannot enter its own install
// tree, and systemd reports 203/EXEC and restart-loops the unit. Chmod
// is not umask-masked, so it has to be applied afterwards -- to each
// level, since MkdirAll creates the parents under the same umask.
func mkdirShared(base, dir string) error {
	if err := os.MkdirAll(dir, sharedDirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Outside the base: chmod what was asked for and nothing above
		// it, rather than walking up an unknown tree.
		return chmodShared(dir)
	}

	p := base
	if err := chmodShared(p); err != nil {
		return err
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		p = filepath.Join(p, part)
		if err := chmodShared(p); err != nil {
			return err
		}
	}
	return nil
}

func chmodShared(dir string) error {
	if err := os.Chmod(dir, sharedDirMode); err != nil {
		return fmt.Errorf("make %s traversable: %w", dir, err)
	}
	return nil
}

// layout is a managed install rooted at a directory.
type layout struct {
	root string
}

// binDir holds the symlinks naming the active version.
func (l *layout) binDir() string { return filepath.Join(l.root, layoutBinDirName) }

// versionsDir holds one directory per installed version.
func (l *layout) versionsDir() string { return filepath.Join(l.root, layoutVersionsDirName) }

// versionBinDir is where the binaries of one version live.
func (l *layout) versionBinDir(ver string) string {
	return filepath.Join(l.versionsDir(), ver, layoutBinDirName)
}

// statePath records what this install ran before the active version.
func (l *layout) statePath() string { return filepath.Join(l.root, layoutStateFileName) }

// detectLayout recognises a managed install from the resolved path of
// the running binary, and returns nil for anything else.
//
// resolvedExe must already have had its symlinks resolved: the point of
// the layout is that the binary is reached through symlinks, so the
// unresolved path is always <root>/bin or /usr/local/bin and never says
// which version is running.
func detectLayout(resolvedExe string) *layout {
	binDir := filepath.Dir(resolvedExe)
	if filepath.Base(binDir) != layoutBinDirName {
		return nil
	}
	versionDir := filepath.Dir(binDir)
	versionsDir := filepath.Dir(versionDir)
	if filepath.Base(versionsDir) != layoutVersionsDirName {
		return nil
	}
	// The directory name has to be a version. Without this check any
	// path ending .../bin/ two levels under a directory called
	// "versions" would be claimed as managed.
	if _, err := version.ParseVersion(filepath.Base(versionDir)); err != nil {
		return nil
	}
	return &layout{root: filepath.Dir(versionsDir)}
}

// layoutState is the history a symlink cannot carry.
//
// Which version is active is deliberately NOT stored here: the symlink
// is the only thing that decides what runs, so recording it separately
// would create a second answer that can disagree with the first. This
// file holds only the question the symlink cannot answer — what to go
// back to.
type layoutState struct {
	// Previous is the stack of versions this install ran before the
	// active one, oldest first. Installing pushes the version being
	// replaced; rolling back pops.
	Previous []string `json:"previous"`
}

func (l *layout) loadState() *layoutState {
	data, err := os.ReadFile(l.statePath())
	if err != nil {
		// A missing or unreadable state file costs the rollback target,
		// not the install. Starting from empty is right for a first
		// install and harmless otherwise: Rollback reports that it has
		// nowhere to go, which is true of what it can see.
		if !os.IsNotExist(err) {
			slog.Warn("could not read the update state file; rollback has no history to work from",
				"path", l.statePath(), "err", err)
		}
		return &layoutState{}
	}
	state := &layoutState{}
	if err := json.Unmarshal(data, state); err != nil {
		slog.Warn("update state file is not readable JSON; ignoring it",
			"path", l.statePath(), "err", err)
		return &layoutState{}
	}
	return state
}

func (l *layout) saveState(state *layoutState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode update state: %w", err)
	}
	tmp := l.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write update state: %w", err)
	}
	if err := os.Rename(tmp, l.statePath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace update state: %w", err)
	}
	return nil
}

// activeVersion reads the version the bin symlink currently names.
func (l *layout) activeVersion() (string, error) {
	link := filepath.Join(l.binDir(), nodeBinaryName)
	target, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", link, err)
	}
	// Targets are relative (../versions/<ver>/bin/<name>) so the tree
	// survives being moved, but absolute ones are read the same way.
	return versionFromBinaryPath(target)
}

// versionFromBinaryPath pulls the version out of a path that ends
// .../versions/<version>/bin/<binary>.
func versionFromBinaryPath(p string) (string, error) {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i := len(parts) - 1; i > 0; i-- {
		if parts[i-1] == layoutVersionsDirName {
			return parts[i], nil
		}
	}
	return "", fmt.Errorf("%q does not name a version directory", p)
}

// installedVersions lists the versions present on disk, newest first.
//
// The names returned are the directory names as they are on disk, never
// the parsed version re-rendered. Every caller joins these back into a
// path, and the two are not always the same string: a directory called
// "01.02.03" parses to 1.2.3, so re-rendering would have prune delete a
// path that does not exist while the real directory survives, and would
// have repairPermissions create an empty one that never should.
// Only the SORT goes through the parser.
func (l *layout) installedVersions() []string {
	entries, err := os.ReadDir(l.versionsDir())
	if err != nil {
		return nil
	}
	type installed struct {
		name string
		ver  *version.Version
	}
	var found []installed
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		v, err := version.ParseVersion(entry.Name())
		if err != nil {
			continue
		}
		found = append(found, installed{name: entry.Name(), ver: v})
	}
	slices.SortFunc(found, func(a, b installed) int {
		switch {
		case a.ver.IsGreaterThan(b.ver):
			return -1
		case a.ver.IsLessThan(b.ver):
			return 1
		default:
			return 0
		}
	})
	names := make([]string, 0, len(found))
	for _, f := range found {
		names = append(names, f.name)
	}
	return names
}

// hasVersion reports whether a version directory holds a node binary.
func (l *layout) hasVersion(ver string) bool {
	info, err := os.Stat(filepath.Join(l.versionBinDir(ver), nodeBinaryName))
	return err == nil && info.Mode().IsRegular()
}

// activate points the bin symlinks at ver.
//
// Each link is created under a temporary name and renamed into place,
// because there is no atomic "repoint an existing symlink" call:
// removing and recreating leaves a window where the node has no binary
// at all, and a crash inside that window leaves the install unbootable.
// rename(2) over an existing symlink is atomic, so a reader either sees
// the old target or the new one.
func (l *layout) activate(ver string) error {
	if !l.hasVersion(ver) {
		return fmt.Errorf("version %s is not installed under %s", ver, l.versionsDir())
	}
	if err := mkdirShared(l.root, l.binDir()); err != nil {
		return err
	}

	for _, name := range managedBinaryNames {
		target := filepath.Join(l.versionBinDir(ver), name)
		if _, err := os.Stat(target); err != nil {
			// A release that ships no launcher leaves the previous
			// link alone, exactly as the in-place path left the
			// previous launcher file alone.
			if name == launcherBinaryName {
				continue
			}
			return fmt.Errorf("stat %s: %w", target, err)
		}
		// Relative, so moving the whole tree (or exporting it to
		// another machine) does not break the link.
		rel := filepath.Join("..", layoutVersionsDirName, ver, layoutBinDirName, name)
		if err := replaceSymlink(rel, filepath.Join(l.binDir(), name)); err != nil {
			return err
		}
	}
	return nil
}

// managedBinaryNames are the binaries a managed install links. The
// client CLI is deliberately absent: it is not what the service runs,
// and versioning it here would make an operator's `zzrouter` change
// under them on a node update.
var managedBinaryNames = []string{nodeBinaryName, launcherBinaryName}

// replaceSymlink atomically makes link point at target.
func replaceSymlink(target, link string) error {
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("create symlink %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("move symlink into place at %s: %w", link, err)
	}
	return nil
}

// prune removes version directories that are neither active nor
// reachable by rolling back keep times.
//
// Reachability, not recency, decides: the newest directories on disk
// are not necessarily the ones the rollback stack names, and deleting a
// directory the stack points at turns a rollback into a failure at the
// worst possible moment.
func (l *layout) prune(active string, state *layoutState, keep int) {
	if keep < 0 {
		keep = 0
	}
	reachable := map[string]bool{active: true}
	for i := len(state.Previous) - 1; i >= 0 && len(reachable) <= keep; i-- {
		reachable[state.Previous[i]] = true
	}

	for _, ver := range l.installedVersions() {
		if reachable[ver] {
			continue
		}
		dir := filepath.Join(l.versionsDir(), ver)
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("could not remove an old version directory", "dir", dir, "err", err)
			continue
		}
		slog.Info("removed an old version", "version", ver, "dir", dir)
	}
}

// forget drops versions from the rollback stack that are no longer on
// disk, so a rollback target is always one prune cannot have removed.
func (l *layout) forget(state *layoutState) {
	kept := state.Previous[:0]
	for _, ver := range state.Previous {
		if l.hasVersion(ver) {
			kept = append(kept, ver)
		}
	}
	state.Previous = kept
}
