package update

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/version"
)

// requireSymlinks skips on platforms where an unprivileged process
// cannot create one. The managed layout is a Unix service-install
// concern; Windows keeps the in-place path.
func requireSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the managed layout is not used on Windows")
	}
}

// newManagedInstall builds a managed tree already running ver, and an
// Installer pointed at it the way the real one points at itself.
func newManagedInstall(t *testing.T, ver string, keep int) (*layout, *Installer) {
	t.Helper()
	requireSymlinks(t)

	l := &layout{root: t.TempDir()}
	seedVersion(t, l, ver)
	require.NoError(t, l.activate(ver))

	i := NewInstaller(filepath.Join(l.root, "backups"), keep)
	i.SetCurrentExecutable(filepath.Join(l.versionBinDir(ver), nodeBinaryName))
	return l, i
}

// seedVersion writes a version directory without activating it.
func seedVersion(t *testing.T, l *layout, ver string) {
	t.Helper()
	binDir := l.versionBinDir(ver)
	require.NoError(t, os.MkdirAll(binDir, 0755))
	for _, name := range managedBinaryNames {
		require.NoError(t, os.WriteFile(filepath.Join(binDir, name), fakeBinary(name, ver), 0755))
	}
}

// releaseArchive writes a tar.gz carrying the named binaries at ver,
// in its own directory so extraction cannot collide between calls.
func releaseArchive(t *testing.T, ver string, names ...string) string {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range names {
		files[name+"-"+runtime.GOOS+"-"+runtime.GOARCH] = fakeBinary(name, ver)
	}
	archivePath := filepath.Join(t.TempDir(), "release.tar.gz")
	createTestTarGz(t, archivePath, files)
	return archivePath
}

func mustVersion(t *testing.T, s string) *version.Version {
	t.Helper()
	v, err := version.ParseVersion(s)
	require.NoError(t, err)
	return v
}

// activeNodeContent reads what the bin symlink actually resolves to, so
// assertions are about the file that would run, not about bookkeeping.
func activeNodeContent(t *testing.T, l *layout) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(l.binDir(), nodeBinaryName))
	require.NoError(t, err)
	return string(data)
}

func TestDetectLayout(t *testing.T) {
	tests := []struct {
		name     string
		exe      string
		wantRoot string
	}{
		{"managed install", "/opt/zzrouter/versions/1.2.3/bin/zzrouter-node", "/opt/zzrouter"},
		{"prerelease version", "/opt/zzrouter/versions/1.2.3-rc1/bin/zzrouter-node", "/opt/zzrouter"},
		{"plain install", "/usr/local/bin/zzrouter-node", ""},
		{"user install", "/home/operator/.local/bin/zzrouter-node", ""},
		// Guards against claiming any path that merely has the right
		// shape: the directory naming the version has to parse as one.
		{"versions dir but no version", "/opt/zzrouter/versions/current/bin/zzrouter-node", ""},
		{"right depth, wrong parent", "/opt/zzrouter/releases/1.2.3/bin/zzrouter-node", ""},
		{"not under bin", "/opt/zzrouter/versions/1.2.3/zzrouter-node", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectLayout(filepath.FromSlash(tt.exe))
			if tt.wantRoot == "" {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, filepath.FromSlash(tt.wantRoot), got.root)
		})
	}
}

func TestManagedInstall_MovesSymlinkAndLeavesOldVersionIntact(t *testing.T) {
	l, i := newManagedInstall(t, "1.0.0", 2)

	result, err := i.Install(context.Background(), releaseArchive(t, "2.0.0", nodeBinaryName, launcherBinaryName), mustVersion(t, "2.0.0"))
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Equal(t, "2.0.0", result.Version)
	assert.Equal(t, "1.0.0", result.PreviousVersion)

	active, err := l.activeVersion()
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", active)
	assert.Contains(t, activeNodeContent(t, l), "version 2.0.0")

	// The whole point of the layout: the directory the running process
	// was executing out of was not touched.
	old, err := os.ReadFile(filepath.Join(l.versionBinDir("1.0.0"), nodeBinaryName))
	require.NoError(t, err)
	assert.Contains(t, string(old), "version 1.0.0")

	assert.Equal(t, []string{"1.0.0"}, l.loadState().Previous)
}

func TestManagedInstall_LinkIsRelative(t *testing.T) {
	l, i := newManagedInstall(t, "1.0.0", 2)
	_, err := i.Install(context.Background(), releaseArchive(t, "2.0.0", nodeBinaryName, launcherBinaryName), mustVersion(t, "2.0.0"))
	require.NoError(t, err)

	// Relative targets are what let the tree be moved or mounted
	// elsewhere without every link dangling.
	target, err := os.Readlink(filepath.Join(l.binDir(), nodeBinaryName))
	require.NoError(t, err)
	assert.False(t, filepath.IsAbs(target), "symlink target should be relative, got %q", target)
	assert.Equal(t, filepath.Join("..", "versions", "2.0.0", "bin", nodeBinaryName), target)
}

func TestManagedInstall_RefusesTheVersionAlreadyRunning(t *testing.T) {
	_, i := newManagedInstall(t, "1.0.0", 2)

	_, err := i.Install(context.Background(), releaseArchive(t, "1.0.0", nodeBinaryName), mustVersion(t, "1.0.0"))
	require.ErrorIs(t, err, ErrVersionAlreadyActive)
}

func TestManagedInstall_FailedSmokeTestLeavesEverythingAlone(t *testing.T) {
	l, i := newManagedInstall(t, "1.0.0", 2)

	// The archive says 2.0.0; the binary inside reports something else.
	archivePath := filepath.Join(t.TempDir(), "release.tar.gz")
	createTestTarGz(t, archivePath, map[string][]byte{
		nodeBinaryName: fakeBinary(nodeBinaryName, "9.9.9"),
	})

	_, err := i.Install(context.Background(), archivePath, mustVersion(t, "2.0.0"))
	require.ErrorIs(t, err, ErrSmokeTestFailed)

	active, err := l.activeVersion()
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", active)
	assert.NoDirExists(t, filepath.Join(l.versionsDir(), "2.0.0"))
}

func TestManagedInstall_CarriesTheLauncherForwardWhenTheReleaseHasNone(t *testing.T) {
	l, i := newManagedInstall(t, "1.0.0", 2)

	_, err := i.Install(context.Background(), releaseArchive(t, "2.0.0", nodeBinaryName), mustVersion(t, "2.0.0"))
	require.NoError(t, err)

	// The node looks for its launcher beside itself, so a version
	// directory without one costs every provider process its PID
	// tracking -- silently.
	carried, err := os.ReadFile(filepath.Join(l.versionBinDir("2.0.0"), launcherBinaryName))
	require.NoError(t, err)
	assert.Contains(t, string(carried), "version 1.0.0")

	info, err := os.Stat(filepath.Join(l.versionBinDir("2.0.0"), launcherBinaryName))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0111, "carried-forward launcher must stay executable")
}

func TestManagedRollback_ReturnsToThePreviousVersion(t *testing.T) {
	l, i := newManagedInstall(t, "1.0.0", 2)
	_, err := i.Install(context.Background(), releaseArchive(t, "2.0.0", nodeBinaryName, launcherBinaryName), mustVersion(t, "2.0.0"))
	require.NoError(t, err)

	i.SetCurrentExecutable(filepath.Join(l.versionBinDir("2.0.0"), nodeBinaryName))
	result, err := i.Rollback()
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Equal(t, "1.0.0", result.Version)

	active, err := l.activeVersion()
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", active)
	assert.Contains(t, activeNodeContent(t, l), "version 1.0.0")

	// The version rolled away from stays on disk: nothing was consumed,
	// so the operator can still inspect it.
	assert.FileExists(t, filepath.Join(l.versionBinDir("2.0.0"), nodeBinaryName))
	assert.Empty(t, l.loadState().Previous)
}

func TestManagedRollback_TwiceWalksTwoVersionsBack(t *testing.T) {
	l, i := newManagedInstall(t, "1.0.0", 3)

	for _, ver := range []string{"2.0.0", "3.0.0"} {
		_, err := i.Install(context.Background(), releaseArchive(t, ver, nodeBinaryName, launcherBinaryName), mustVersion(t, ver))
		require.NoError(t, err)
		i.SetCurrentExecutable(filepath.Join(l.versionBinDir(ver), nodeBinaryName))
	}
	assert.Equal(t, []string{"1.0.0", "2.0.0"}, l.loadState().Previous)

	_, err := i.Rollback()
	require.NoError(t, err)
	active, err := l.activeVersion()
	require.NoError(t, err)
	require.Equal(t, "2.0.0", active)

	// The bug this pins: a second rollback that walks forward again
	// instead of back, because "the newest version that is not the
	// active one" is 3.0.0.
	i.SetCurrentExecutable(filepath.Join(l.versionBinDir("2.0.0"), nodeBinaryName))
	_, err = i.Rollback()
	require.NoError(t, err)
	active, err = l.activeVersion()
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", active)
	assert.Contains(t, activeNodeContent(t, l), "version 1.0.0")
}

func TestManagedRollback_NothingToGoBackTo(t *testing.T) {
	_, i := newManagedInstall(t, "1.0.0", 2)

	_, err := i.Rollback()
	require.ErrorIs(t, err, ErrNoBackupsAvailable)
}

func TestManagedPrune_KeepsTheReachableVersionsAndDropsTheRest(t *testing.T) {
	l, i := newManagedInstall(t, "1.0.0", 1)

	for _, ver := range []string{"2.0.0", "3.0.0"} {
		_, err := i.Install(context.Background(), releaseArchive(t, ver, nodeBinaryName, launcherBinaryName), mustVersion(t, ver))
		require.NoError(t, err)
		i.SetCurrentExecutable(filepath.Join(l.versionBinDir(ver), nodeBinaryName))
	}

	// keep=1: the active version plus one rollback target.
	assert.DirExists(t, filepath.Join(l.versionsDir(), "3.0.0"))
	assert.DirExists(t, filepath.Join(l.versionsDir(), "2.0.0"))
	assert.NoDirExists(t, filepath.Join(l.versionsDir(), "1.0.0"))

	// And what prune removed is no longer offered as a rollback target,
	// so a rollback cannot be sent at a directory that is gone.
	assert.Equal(t, []string{"2.0.0"}, l.loadState().Previous)
}

func TestManagedPrune_NeverRemovesTheActiveVersion(t *testing.T) {
	l, i := newManagedInstall(t, "5.0.0", 0)

	// An older version number installed over a newer one: prune sorts by
	// version, so "keep the newest directories" would delete the one
	// that is running.
	_, err := i.Install(context.Background(), releaseArchive(t, "4.0.0", nodeBinaryName, launcherBinaryName), mustVersion(t, "4.0.0"))
	require.NoError(t, err)

	assert.DirExists(t, filepath.Join(l.versionsDir(), "4.0.0"))
	assert.NoDirExists(t, filepath.Join(l.versionsDir(), "5.0.0"))
	assert.Contains(t, activeNodeContent(t, l), "version 4.0.0")
}

func TestManagedInstall_DiscardsAnInterruptedInstallOfTheSameVersion(t *testing.T) {
	l, i := newManagedInstall(t, "1.0.0", 2)

	// What an install that died between mkdir and activate leaves.
	orphan := l.versionBinDir("2.0.0")
	require.NoError(t, os.MkdirAll(orphan, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, nodeBinaryName), []byte("truncated"), 0755))

	_, err := i.Install(context.Background(), releaseArchive(t, "2.0.0", nodeBinaryName, launcherBinaryName), mustVersion(t, "2.0.0"))
	require.NoError(t, err)
	assert.Contains(t, activeNodeContent(t, l), "version 2.0.0")
}

func TestLayoutForget_DropsRollbackTargetsThatAreGone(t *testing.T) {
	requireSymlinks(t)
	l := &layout{root: t.TempDir()}
	seedVersion(t, l, "1.0.0")

	state := &layoutState{Previous: []string{"0.9.0", "1.0.0"}}
	l.forget(state)
	assert.Equal(t, []string{"1.0.0"}, state.Previous)
}

func TestVersionFromBinaryPath(t *testing.T) {
	got, err := versionFromBinaryPath("../versions/1.2.3/bin/zzrouter-node")
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", got)

	got, err = versionFromBinaryPath("/opt/zzrouter/versions/2.0.0/bin/zzrouter-node")
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", got)

	_, err = versionFromBinaryPath("/usr/local/bin/zzrouter-node")
	assert.Error(t, err)
}

func TestPushVersion_DoesNotRepeatOnInstallRollbackCycles(t *testing.T) {
	stack := pushVersion(nil, "1.0.0")
	stack = pushVersion(stack, "2.0.0")
	stack = pushVersion(stack, "1.0.0")

	// 1.0.0 moves to the top rather than appearing twice, so a cycle of
	// installs and rollbacks cannot grow the stack without bound.
	assert.Equal(t, []string{"2.0.0", "1.0.0"}, stack)
}
