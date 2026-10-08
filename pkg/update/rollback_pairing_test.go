package update

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rollbackFixture is an installed node/launcher pair plus an installer
// pointed at them.
type rollbackFixture struct {
	installer *Installer
	node      string
	launcher  string
	dir       string
}

func newRollbackFixture(t *testing.T) *rollbackFixture {
	t.Helper()

	dir := t.TempDir()
	backupDir := filepath.Join(dir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	node := filepath.Join(dir, "zzrouter-node")
	launcher := filepath.Join(dir, "zzrouter-launcher")
	require.NoError(t, os.WriteFile(node, fakeBinary("zzrouter-node", "1.0.0"), 0750))
	require.NoError(t, os.WriteFile(launcher, fakeBinary("zzrouter-launcher", "1.0.0"), 0750))

	i := NewInstaller(backupDir, 5)
	i.SetCurrentExecutable(node)
	return &rollbackFixture{installer: i, node: node, launcher: launcher, dir: dir}
}

// install publishes an archive and installs it. Passing an empty
// launcher version ships a release with no launcher in it.
func (f *rollbackFixture) install(t *testing.T, nodeVer, launcherVer string) {
	t.Helper()

	members := map[string][]byte{
		"zzrouter-node-linux-amd64": fakeBinary("zzrouter-node", nodeVer),
	}
	if launcherVer != "" {
		members["zzrouter-launcher-linux-amd64"] = fakeBinary("zzrouter-launcher", launcherVer)
	}

	archive := filepath.Join(f.dir, "update-"+nodeVer+".tar.gz")
	createTestTarGz(t, archive, members)

	parsed, err := version.ParseVersion(nodeVer)
	require.NoError(t, err)
	result, err := f.installer.Install(context.Background(), archive, parsed)
	require.NoError(t, err)
	require.True(t, result.Success)
}

func (f *rollbackFixture) read(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
}

// TestRollback_PairsLauncherByInstall covers the sequence where the
// newest launcher backup does NOT belong to the node being restored:
// release 2 ships a launcher, release 3 does not. Rolling back 3 must
// leave release 2's launcher alone rather than restoring release 1's.
func TestRollback_PairsLauncherByInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	f := newRollbackFixture(t)

	f.install(t, "2.0.0", "2.0.0") // backs up launcher 1.0.0
	f.install(t, "3.0.0", "")      // no launcher in this release, no backup taken

	require.NoError(t, func() error { _, err := f.installer.Rollback(); return err }())

	assert.Equal(t, fakeBinary("zzrouter-node", "2.0.0"), f.read(t, f.node))
	assert.Equal(t, fakeBinary("zzrouter-launcher", "2.0.0"), f.read(t, f.launcher),
		"rolling back to 2.0.0 must keep 2.0.0's launcher, not restore 1.0.0's")
}

// TestRollback_TwiceKeepsGoingBack: a second rollback must continue
// backwards, not reinstall the binary the first one rolled away from.
func TestRollback_TwiceKeepsGoingBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	f := newRollbackFixture(t)

	f.install(t, "2.0.0", "2.0.0")
	f.install(t, "3.0.0", "3.0.0")

	_, err := f.installer.Rollback()
	require.NoError(t, err)
	require.Equal(t, fakeBinary("zzrouter-node", "2.0.0"), f.read(t, f.node))

	_, err = f.installer.Rollback()
	require.NoError(t, err)
	assert.Equal(t, fakeBinary("zzrouter-node", "1.0.0"), f.read(t, f.node),
		"a second rollback must reach 1.0.0, not return to 3.0.0")
	assert.Equal(t, fakeBinary("zzrouter-launcher", "1.0.0"), f.read(t, f.launcher))
}

// TestRollback_SetsAsideOutsideTheBackupDir pins the mechanism the test
// above depends on: the displaced binary is preserved, but somewhere
// listBackups will never choose it from.
func TestRollback_SetsAsideOutsideTheBackupDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	f := newRollbackFixture(t)
	f.install(t, "2.0.0", "2.0.0")

	_, err := f.installer.Rollback()
	require.NoError(t, err)

	aside, err := os.ReadDir(filepath.Join(f.dir, "backups", rolledBackDirName))
	require.NoError(t, err)
	require.NotEmpty(t, aside, "the displaced binary should still be recoverable by hand")

	selectable, err := f.installer.listBackups("zzrouter-node")
	require.NoError(t, err)
	for _, b := range selectable {
		assert.NotContains(t, b, rolledBackDirName)
	}
}

func TestBackupStamp(t *testing.T) {
	// The binary name contains a dash and so does the stamp, so neither
	// end can be found by splitting on one.
	stamp, ok := backupStamp("zzrouter-node-20260101-000000.000000000", "zzrouter-node")
	require.True(t, ok)
	assert.Equal(t, "20260101-000000.000000000", stamp)

	_, ok = backupStamp("zzrouter-launcher-20260101-000000.000000000", "zzrouter-node")
	assert.False(t, ok, "a launcher backup must not read as a node backup")

	_, ok = backupStamp("zzrouter-node-notatimestamp", "zzrouter-node")
	assert.False(t, ok)
}

func TestCheckWritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not gate root or Windows the same way")
	}

	dir := t.TempDir()
	require.NoError(t, checkWritable(filepath.Join(dir, "zzrouter-node")))

	require.NoError(t, os.Chmod(dir, 0500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0750) })

	err := checkWritable(filepath.Join(dir, "zzrouter-node"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInstallDirNotWritable)
	assert.Contains(t, err.Error(), dir)
}

// TestInstall_RefusesWhenTheInstallDirIsReadOnly is the systemd case:
// under ProtectSystem=strict the install directory is mounted read-only,
// and the failure used to surface as a rename error partway through.
func TestInstall_RefusesWhenTheInstallDirIsReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not gate root or Windows the same way")
	}

	f := newRollbackFixture(t)
	require.NoError(t, os.Chmod(f.dir, 0500))
	t.Cleanup(func() { _ = os.Chmod(f.dir, 0750) })

	archive := filepath.Join(t.TempDir(), "update.tar.gz")
	createTestTarGz(t, archive, map[string][]byte{
		"zzrouter-node-linux-amd64": fakeBinary("zzrouter-node", "2.0.0"),
	})

	_, err := f.installer.Install(context.Background(), archive, &version.Version{Major: 2})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInstallDirNotWritable)
}
