//go:build !windows

package recovery

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stretchr/testify/require"
)

// inodes maps every path under dir, relative to it, to its inode.
func inodes(t *testing.T, dir string) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		out[filepath.ToSlash(rel)] = info.Sys().(*syscall.Stat_t).Ino
		return err
	}))
	return out
}

// ownedIn collects the paths the owned steps among steps reported.
func ownedIn(steps ...[]string) map[string]bool {
	owned := map[string]bool{}
	for _, list := range steps {
		for _, step := range list {
			if given, ok := strings.CutPrefix(step, "owned "); ok {
				for _, rel := range strings.Split(given, ", ") {
					owned[rel] = true
				}
			}
		}
	}
	return owned
}

// requireHandedOver requires every path under dir with a new inode since
// before to be among owned.
func requireHandedOver(t *testing.T, dir string, before map[string]uint64, owned map[string]bool, context string) {
	t.Helper()
	for rel, ino := range inodes(t, dir) {
		if old, ok := before[rel]; ok && old == ino || rel == "." {
			continue
		}
		require.True(t, owned[rel], "%s: %s is new and was never given the installation's owner", context, rel)
	}
}

// TestActivationHandsOverEveryNewPath kills a real process at each step of an
// activation, on an existing and on a fresh installation, then rolls it back
// or finishes it: every path with a new inode was handed to the installation's
// owner, by the killed run before the kill or by the last run, and a rollback
// on a fresh installation leaves neither an access nor a rollback directory.
// It forks, so it does not run in parallel.
func TestActivationHandsOverEveryNewPath(t *testing.T) {
	existing, staging, _ := activationFixture(t)
	fresh := t.TempDir()
	lock, err := config.LockInstallation(fresh)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	for name, dir := range map[string]string{"existing": existing, "fresh": fresh} {
		steps := stepsOf(t, dir, copyTree(t, staging), false)
		for k := range steps[:len(steps)-1] {
			for _, rollback := range []bool{true, false} {
				killed, staged := copyTree(t, dir), copyTree(t, staging)
				before := inodes(t, killed)
				killAt(t, killed, staged, k+1, false)
				var last []string
				_, err := Activate(sqlitedb.WithStep(t.Context(), func(step string) { last = append(last, step) }), killed, staged, ActivateOptions{Rollback: rollback})
				require.NoError(t, err)
				requireHandedOver(t, killed, before, ownedIn(steps[:k+1], last), name+" killed at "+steps[k])
				if name == "fresh" && rollback {
					require.NoDirExists(t, filepath.Join(killed, "access"), "the access directory the activation created")
					require.NoDirExists(t, filepath.Join(killed, rollbackDir), "the rollback directory the activation created")
				}
			}
		}
	}
}

// TestRollbackRerunHandsOverEveryNewPath: an activation killed once the store
// is replaced but before its hand-over, a rollback killed at each of its steps,
// and the rollback run again: every path with a new inode was handed over.
// It forks, so it does not run in parallel.
func TestRollbackRerunHandsOverEveryNewPath(t *testing.T) {
	dir, staging, _ := activationFixture(t)
	steps := stepsOf(t, dir, copyTree(t, staging), false)
	k := len(steps) - 3
	require.Equal(t, "store marker", steps[k], "the store written, nothing handed over yet")
	probe, probed := copyTree(t, dir), copyTree(t, staging)
	killAt(t, probe, probed, k+1, false)
	undo := stepsOf(t, probe, probed, true)
	for j := range undo[:len(undo)-1] {
		killed, staged := copyTree(t, dir), copyTree(t, staging)
		before := inodes(t, killed)
		killAt(t, killed, staged, k+1, false)
		killAt(t, killed, staged, j+1, true)
		var last []string
		_, err := Activate(sqlitedb.WithStep(t.Context(), func(step string) { last = append(last, step) }), killed, staged, ActivateOptions{Rollback: true})
		require.NoError(t, err)
		requireHandedOver(t, killed, before, ownedIn(steps[:k+1], undo[:j+1], last), "rollback killed at "+undo[j])
	}
}
