//go:build unix

package update

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSecureUmask runs the node's production umask for the duration of a
// test.
//
// Worth the trouble because a test that does not set it asserts a
// permission the running node never actually produces. The node calls
// security.SetSecureUmask (0027) at startup, so MkdirAll(0755) lands as
// 0750 and WriteFile(0644) as 0640 -- and the first version of this
// package shipped exactly that, leaving the service user unable to
// traverse the directory holding the binary it runs. systemd answered
// with 203/EXEC and a restart loop.
//
// Not parallel-safe: umask is per-process. These tests must not call
// t.Parallel.
func withSecureUmask(t *testing.T) {
	t.Helper()
	previous := syscall.Umask(0027)
	t.Cleanup(func() { syscall.Umask(previous) })
}

func TestManagedInstall_StaysTraversableUnderTheNodesUmask(t *testing.T) {
	withSecureUmask(t)
	l, i := newManagedInstall(t, "1.0.0", 2)

	_, err := i.Install(context.Background(),
		releaseArchive(t, "2.0.0", nodeBinaryName, launcherBinaryName), mustVersion(t, "2.0.0"))
	require.NoError(t, err)

	// Every level from the root down, because the service user has to
	// traverse all of them to reach the binary and MkdirAll creates the
	// parents under the same umask as the leaf.
	for _, dir := range []string{
		l.root,
		l.versionsDir(),
		filepath.Join(l.versionsDir(), "2.0.0"),
		l.versionBinDir("2.0.0"),
		l.binDir(),
	} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.NotZero(t, info.Mode().Perm()&0001,
			"%s is %v: the service user cannot traverse it, and systemd answers 203/EXEC", dir, info.Mode().Perm())
	}

	info, err := os.Stat(filepath.Join(l.versionBinDir("2.0.0"), nodeBinaryName))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0005, "the node binary must be readable and executable by the service user")
}

func TestHandoff_StatusStaysReadableUnderTheNodesUmask(t *testing.T) {
	withSecureUmask(t)
	h := NewHandoff(t.TempDir(), t.TempDir())

	require.NoError(t, h.Publish(&RunStatus{Action: ActionCheck}))

	// Written by root, read by the unprivileged node. Without this the
	// node's own /update/status cannot say what the updater did.
	info, err := os.Stat(h.StatusPath())
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0044,
		"status is %v: the service user cannot read what the privileged updater published", info.Mode().Perm())
}

// TestPublish_DoesNotFollowAPlantedSymlink pins the fix for a real
// local privilege escalation.
//
// The privileged updater runs as root and publishes its status where the
// unprivileged node can read it. If it created that file in the
// directory the SERVICE USER owns, that user could pre-plant the temp
// name as a symlink -- O_CREATE follows symlinks -- and root's write
// would land wherever the link pointed. Demonstrated on a real worker:
// the service user planted a link and root created a root-owned file at
// the chosen path.
//
// The fix is ownership, not cleverness: root only ever creates files in
// a directory root owns.
func TestPublish_DoesNotFollowAPlantedSymlink(t *testing.T) {
	requestDir := t.TempDir() // stands in for the service-user-owned half
	statusDir := t.TempDir()  // stands in for the root-owned half
	h := NewHandoff(requestDir, statusDir)

	victim := filepath.Join(t.TempDir(), "root-only-file")
	// What an attacker who owns the request directory would plant.
	require.NoError(t, os.Symlink(victim, filepath.Join(requestDir, "status.json.tmp")))
	require.NoError(t, os.Symlink(victim, filepath.Join(requestDir, "status.json")))

	require.NoError(t, h.Publish(&RunStatus{Action: ActionCheck}))

	assert.NoFileExists(t, victim,
		"the status write followed a symlink planted in the request directory")
	assert.FileExists(t, h.StatusPath(), "the status still has to be published")
}

// TestClaim_DoesNotFollowASymlinkedRequest: the request lives in a
// directory the service user owns, so the name can be a link to
// something root can read but was never meant to open.
func TestClaim_DoesNotFollowASymlinkedRequest(t *testing.T) {
	requestDir := t.TempDir()
	h := NewHandoff(requestDir, t.TempDir())

	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte(`{"action":"apply"}`), 0600))
	require.NoError(t, os.Symlink(secret, h.RequestPath()))

	_, err := h.Claim()
	require.Error(t, err, "a symlinked request must not be read through")

	// And the link is gone, so it cannot wedge the watcher. Unlinking
	// removes the link, never the target.
	assert.NoFileExists(t, h.RequestPath())
	assert.FileExists(t, secret, "removing the request must not remove what it pointed at")
}
