package update

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertManagedBinaries(t *testing.T, l *layout, ver string) {
	t.Helper()
	for _, name := range []string{nodeBinaryName, launcherBinaryName, clientBinaryName} {
		resolved, err := filepath.EvalSymlinks(filepath.Join(l.binDir(), name))
		require.NoError(t, err)
		expected, err := filepath.EvalSymlinks(filepath.Join(l.versionBinDir(ver), name))
		require.NoError(t, err)
		assert.Equal(t, expected, resolved)
		data, err := os.ReadFile(resolved)
		require.NoError(t, err)
		assert.Equal(t, fakeBinary(name, ver), data)
	}
}

func TestManagedInstall_ClientFailureChangesNothing(t *testing.T) {
	for _, tt := range []struct {
		name   string
		client []byte
		want   error
	}{
		{"missing", nil, errBinaryNotInArchive},
		{"wrong version", fakeBinary(clientBinaryName, "9.9.9"), ErrSmokeTestFailed},
		{"version substring", fakeBinary(clientBinaryName, "12.0.0"), ErrSmokeTestFailed},
		{"prerelease for stable", fakeBinary(clientBinaryName, "2.0.0-rc1"), ErrSmokeTestFailed},
		{"cannot execute", []byte("not an executable"), ErrSmokeTestFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l, i := newManagedInstall(t, "1.0.0", 0)
			seedVersion(t, l, "0.9.0")
			seedVersion(t, l, "2.0.0")
			require.NoError(t, l.saveState(&layoutState{Previous: []string{"0.9.0"}}))
			files := map[string][]byte{nodeBinaryName: fakeBinary(nodeBinaryName, "2.0.0")}
			if tt.client != nil {
				files[clientBinaryName] = tt.client
			}
			archive := filepath.Join(t.TempDir(), "release.tar.gz")
			createTestTarGz(t, archive, files)

			_, err := i.Install(context.Background(), archive, mustVersion(t, "2.0.0"))
			require.ErrorIs(t, err, tt.want)
			assertManagedBinaries(t, l, "1.0.0")
			assert.Equal(t, []string{"0.9.0"}, l.loadState().Previous)
			assert.FileExists(t, filepath.Join(l.versionBinDir("0.9.0"), clientBinaryName))
			data, err := os.ReadFile(filepath.Join(l.versionBinDir("2.0.0"), clientBinaryName))
			require.NoError(t, err)
			assert.Equal(t, fakeBinary(clientBinaryName, "2.0.0"), data, "probe failure must not clear an inactive directory")
		})
	}
}

func TestManagedRollback_LegacyClientSurvivesPrune(t *testing.T) {
	l, i := newManagedInstall(t, "1.0.0", 2)
	require.NoError(t, os.Remove(filepath.Join(l.versionBinDir("1.0.0"), clientBinaryName)))
	_, err := i.Install(context.Background(), releaseArchive(t, "2.0.0", nodeBinaryName, launcherBinaryName, clientBinaryName), mustVersion(t, "2.0.0"))
	require.NoError(t, err)
	i.SetCurrentExecutable(filepath.Join(l.versionBinDir("2.0.0"), nodeBinaryName))
	_, err = i.Rollback()
	require.NoError(t, err)

	assert.NoFileExists(t, filepath.Join(l.versionBinDir("1.0.0"), clientBinaryName), "rollback must not modify the legacy version")
	data, err := os.ReadFile(filepath.Join(l.binDir(), clientBinaryName))
	require.NoError(t, err)
	assert.Equal(t, fakeBinary(clientBinaryName, "2.0.0"), data)
	l.prune("1.0.0", l.loadState(), 0)
	assert.FileExists(t, filepath.Join(l.versionBinDir("2.0.0"), clientBinaryName))

	i.SetCurrentExecutable(filepath.Join(l.versionBinDir("1.0.0"), nodeBinaryName))
	_, err = i.Install(context.Background(), releaseArchive(t, "2.0.0", nodeBinaryName, clientBinaryName), mustVersion(t, "2.0.0"))
	require.ErrorIs(t, err, ErrVersionInUse)
	data, err = os.ReadFile(filepath.Join(l.binDir(), clientBinaryName))
	require.NoError(t, err)
	assert.Equal(t, fakeBinary(clientBinaryName, "2.0.0"), data)
	active, err := l.activeVersion()
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", active)
	assert.Empty(t, l.loadState().Previous)

	i.keepPreviousVersions = 0
	_, err = i.Install(context.Background(), releaseArchive(t, "3.0.0", nodeBinaryName, launcherBinaryName, clientBinaryName), mustVersion(t, "3.0.0"))
	require.NoError(t, err)
	assertManagedBinaries(t, l, "3.0.0")
	assert.NoDirExists(t, filepath.Dir(l.versionBinDir("2.0.0")), "a complete update releases the old client pin")
}

func TestManagedPrune_CompanionPinDoesNotConsumeRollbackBudget(t *testing.T) {
	l, _ := newManagedInstall(t, "3.0.0", 1)
	seedVersion(t, l, "1.0.0")
	seedVersion(t, l, "0.9.0")
	require.NoError(t, os.Remove(filepath.Join(l.versionBinDir("1.0.0"), clientBinaryName)))
	require.NoError(t, l.activate("1.0.0"))
	l.prune("1.0.0", &layoutState{Previous: []string{"0.9.0"}}, 1)
	for _, ver := range []string{"0.9.0", "1.0.0", "3.0.0"} {
		assert.DirExists(t, l.versionBinDir(ver))
	}
}

func TestManagedInstall_LegacyNodeOnlyTree(t *testing.T) {
	requireSymlinks(t)
	l := &layout{root: t.TempDir()}
	require.NoError(t, os.MkdirAll(l.versionBinDir("1.0.0"), 0755))
	node := filepath.Join(l.versionBinDir("1.0.0"), nodeBinaryName)
	require.NoError(t, os.WriteFile(node, fakeBinary(nodeBinaryName, "1.0.0"), 0755))
	require.NoError(t, l.activate("1.0.0"))
	i := NewInstaller(filepath.Join(l.root, "backups"), 1)
	i.SetCurrentExecutable(node)
	_, err := i.Install(context.Background(), releaseArchive(t, "2.0.0", nodeBinaryName, launcherBinaryName, clientBinaryName), mustVersion(t, "2.0.0"))
	require.NoError(t, err)
	assertManagedBinaries(t, l, "2.0.0")
}

func TestManagedRollback_ActivationFailurePreservesLinksAndHistory(t *testing.T) {
	for _, failure := range []string{"late rename", "invalid client", "broken retained client", "retained directory"} {
		t.Run(failure, func(t *testing.T) {
			l, i := newManagedInstall(t, "1.0.0", 1)
			_, err := i.Install(context.Background(), releaseArchive(t, "2.0.0", nodeBinaryName, launcherBinaryName, clientBinaryName), mustVersion(t, "2.0.0"))
			require.NoError(t, err)
			i.SetCurrentExecutable(filepath.Join(l.versionBinDir("2.0.0"), nodeBinaryName))
			switch failure {
			case "late rename":
				obstruction := filepath.Join(l.binDir(), nodeBinaryName) + ".tmp"
				require.NoError(t, os.Mkdir(obstruction, 0755))
				require.NoError(t, os.WriteFile(filepath.Join(obstruction, "keep"), []byte("obstruct rename"), 0600))
			case "invalid client":
				client := filepath.Join(l.versionBinDir("1.0.0"), clientBinaryName)
				require.NoError(t, os.Remove(client))
				require.NoError(t, os.Mkdir(client, 0755))
			case "broken retained client", "retained directory":
				for _, ver := range []string{"1.0.0", "2.0.0"} {
					require.NoError(t, os.Remove(filepath.Join(l.versionBinDir(ver), clientBinaryName)))
				}
				if failure == "retained directory" {
					require.NoError(t, os.Mkdir(filepath.Join(l.versionBinDir("2.0.0"), clientBinaryName), 0755))
				}
			}
			_, err = i.Rollback()
			require.Error(t, err)
			for _, name := range []string{nodeBinaryName, launcherBinaryName, clientBinaryName} {
				target, err := os.Readlink(filepath.Join(l.binDir(), name))
				require.NoError(t, err)
				assert.Equal(t, filepath.Join("..", "versions", "2.0.0", "bin", name), target)
			}
			assert.Equal(t, []string{"1.0.0"}, l.loadState().Previous)
		})
	}
}

func TestEnsureManagedLayout_RelocatesAllThree(t *testing.T) {
	requireSymlinks(t)
	source := t.TempDir()
	for _, name := range []string{nodeBinaryName, launcherBinaryName, clientBinaryName} {
		require.NoError(t, os.WriteFile(filepath.Join(source, name), fakeBinary(name, "1.0.0"), 0755))
	}
	l := &layout{root: t.TempDir()}
	nodePath, moved, err := ensureManagedLayout(l, filepath.Join(source, nodeBinaryName), source, "1.0.0")
	require.NoError(t, err)
	assert.True(t, moved)
	assert.Equal(t, filepath.Join(l.binDir(), nodeBinaryName), nodePath)
	assertManagedBinaries(t, l, "1.0.0")
	for _, name := range []string{nodeBinaryName, launcherBinaryName, clientBinaryName} {
		target, err := os.Readlink(filepath.Join(source, name))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(l.binDir(), name), target)
	}
}

func TestEnsureManagedLayout_RepairsLegacyPublicClientAfterUpdate(t *testing.T) {
	for _, externalLink := range []bool{false, true} {
		t.Run(map[bool]string{false: "real file", true: "external symlink"}[externalLink], func(t *testing.T) {
			l, i := newManagedInstall(t, "1.0.0", 1)
			require.NoError(t, os.Remove(filepath.Join(l.versionBinDir("1.0.0"), clientBinaryName)))
			require.NoError(t, os.Remove(filepath.Join(l.binDir(), clientBinaryName)))
			publicDir := t.TempDir()
			client := filepath.Join(publicDir, clientBinaryName)
			if externalLink {
				external := filepath.Join(t.TempDir(), clientBinaryName)
				require.NoError(t, os.WriteFile(external, fakeBinary(clientBinaryName, "0.1.0"), 0755))
				require.NoError(t, os.Symlink(external, client))
			} else {
				require.NoError(t, os.WriteFile(client, fakeBinary(clientBinaryName, "0.1.0"), 0755))
			}
			oldExe := filepath.Join(l.versionBinDir("1.0.0"), nodeBinaryName)
			_, moved, err := ensureManagedLayout(l, oldExe, publicDir, "1.0.0")
			require.NoError(t, err)
			assert.False(t, moved)
			data, err := os.ReadFile(client)
			require.NoError(t, err)
			assert.Equal(t, fakeBinary(clientBinaryName, "0.1.0"), data)

			_, err = i.Install(context.Background(), releaseArchive(t, "2.0.0", nodeBinaryName, launcherBinaryName, clientBinaryName), mustVersion(t, "2.0.0"))
			require.NoError(t, err)
			for range 2 {
				// Invoking an older executable must repair the active version.
				_, moved, err := ensureManagedLayout(l, oldExe, publicDir, "1.0.0")
				require.NoError(t, err)
				assert.False(t, moved)
				data, err := os.ReadFile(client)
				require.NoError(t, err)
				assert.Equal(t, fakeBinary(clientBinaryName, "2.0.0"), data)
				assertManagedBinaries(t, l, "2.0.0")
			}
			assert.Equal(t, []string{"1.0.0"}, l.loadState().Previous)
			assert.NoFileExists(t, filepath.Join(l.versionBinDir("1.0.0"), clientBinaryName))
			require.NoError(t, smokeTest(context.Background(), client, mustVersion(t, "2.0.0")))
		})
	}
}

func TestEnsureManagedLayout_RefusesExistingVersionDirectory(t *testing.T) {
	requireSymlinks(t)
	l := &layout{root: t.TempDir()}
	seedVersion(t, l, "1.0.0")
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, nodeBinaryName), fakeBinary(nodeBinaryName, "9.9.9"), 0755))
	_, _, err := ensureManagedLayout(l, filepath.Join(source, nodeBinaryName), source, "1.0.0")
	require.ErrorIs(t, err, ErrNotRelocatable)
	data, err := os.ReadFile(filepath.Join(l.versionBinDir("1.0.0"), nodeBinaryName))
	require.NoError(t, err)
	assert.Equal(t, fakeBinary(nodeBinaryName, "1.0.0"), data)
}

func TestEnsureManagedLayout_ResumesPublishedRelocation(t *testing.T) {
	for _, changed := range []string{"", nodeBinaryName, launcherBinaryName, clientBinaryName, "legacy node only"} {
		t.Run("changed="+changed, func(t *testing.T) {
			requireSymlinks(t)
			l := &layout{root: t.TempDir()}
			source := t.TempDir()
			seedVersion(t, l, "1.0.0")
			published := make(map[string]os.FileInfo)
			for _, name := range []string{nodeBinaryName, launcherBinaryName, clientBinaryName} {
				if changed == "legacy node only" && name != nodeBinaryName {
					require.NoError(t, os.Remove(filepath.Join(l.versionBinDir("1.0.0"), name)))
					continue
				}
				body := fakeBinary(name, "1.0.0")
				if name == changed {
					body = fakeBinary(name, "9.9.9")
				}
				require.NoError(t, os.WriteFile(filepath.Join(source, name), body, 0755))
				info, err := os.Stat(filepath.Join(l.versionBinDir("1.0.0"), name))
				require.NoError(t, err)
				published[name] = info
			}
			// The version is published, but activation and public linking never ran.
			nodePath, moved, err := ensureManagedLayout(l, filepath.Join(source, nodeBinaryName), source, "1.0.0")
			if changed == "" || changed == "legacy node only" {
				require.NoError(t, err)
				assert.True(t, moved)
				assert.Equal(t, filepath.Join(l.binDir(), nodeBinaryName), nodePath)
				for name := range published {
					target, err := os.Readlink(filepath.Join(source, name))
					require.NoError(t, err)
					assert.Equal(t, filepath.Join(l.binDir(), name), target)
					resolved, err := filepath.EvalSymlinks(filepath.Join(l.binDir(), name))
					require.NoError(t, err)
					want, err := filepath.EvalSymlinks(filepath.Join(l.versionBinDir("1.0.0"), name))
					require.NoError(t, err)
					assert.Equal(t, want, resolved)
				}
			} else {
				require.ErrorIs(t, err, ErrNotRelocatable)
				assert.False(t, moved)
				assert.NoDirExists(t, l.binDir())
			}
			for name, original := range published {
				path := filepath.Join(l.versionBinDir("1.0.0"), name)
				info, err := os.Stat(path)
				require.NoError(t, err)
				assert.True(t, os.SameFile(original, info), "published binary must not be replaced")
				assert.Equal(t, original.ModTime(), info.ModTime(), "published binary must not be rewritten")
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, fakeBinary(name, "1.0.0"), body)
			}
		})
	}
}

func TestEnsureManagedLayout_PublicRepairFailureIsReported(t *testing.T) {
	l, _ := newManagedInstall(t, "1.0.0", 1)
	publicDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(publicDir, clientBinaryName), 0755))
	_, _, err := ensureManagedLayout(l, filepath.Join(l.versionBinDir("1.0.0"), nodeBinaryName), publicDir, "1.0.0")
	require.ErrorIs(t, err, ErrNotRelocatable)
	assertManagedBinaries(t, l, "1.0.0")
}

func TestEnsureManagedLayout_StagingFailureCanBeRetried(t *testing.T) {
	requireSymlinks(t)
	l := &layout{root: t.TempDir()}
	source := t.TempDir()
	node := filepath.Join(source, nodeBinaryName)
	client := filepath.Join(source, clientBinaryName)
	require.NoError(t, os.WriteFile(node, fakeBinary(nodeBinaryName, "1.0.0"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(source, launcherBinaryName), fakeBinary(launcherBinaryName, "1.0.0"), 0755))
	require.NoError(t, os.Mkdir(client, 0755))
	_, _, err := ensureManagedLayout(l, node, source, "1.0.0")
	require.ErrorIs(t, err, ErrNotRelocatable)
	assert.NoDirExists(t, filepath.Dir(l.versionBinDir("1.0.0")))
	data, err := os.ReadFile(node)
	require.NoError(t, err)
	assert.Equal(t, fakeBinary(nodeBinaryName, "1.0.0"), data)
	require.NoError(t, os.Remove(client))
	require.NoError(t, os.WriteFile(client, fakeBinary(clientBinaryName, "1.0.0"), 0755))
	_, moved, err := ensureManagedLayout(l, node, source, "1.0.0")
	require.NoError(t, err)
	assert.True(t, moved)
	assertManagedBinaries(t, l, "1.0.0")
}

func TestEnsureManagedLayout_ActivationFailureCanBeRetried(t *testing.T) {
	requireSymlinks(t)
	l := &layout{root: t.TempDir()}
	source := t.TempDir()
	for _, name := range []string{nodeBinaryName, launcherBinaryName, clientBinaryName} {
		require.NoError(t, os.WriteFile(filepath.Join(source, name), fakeBinary(name, "1.0.0"), 0755))
	}
	obstruction := filepath.Join(l.binDir(), nodeBinaryName) + ".tmp"
	require.NoError(t, os.MkdirAll(obstruction, 0755))
	marker := filepath.Join(obstruction, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("obstruct rename"), 0600))
	_, _, err := ensureManagedLayout(l, filepath.Join(source, nodeBinaryName), source, "1.0.0")
	require.ErrorIs(t, err, ErrNotRelocatable)
	assert.NoDirExists(t, filepath.Dir(l.versionBinDir("1.0.0")))
	for _, name := range []string{nodeBinaryName, launcherBinaryName, clientBinaryName} {
		assert.NoFileExists(t, filepath.Join(l.binDir(), name))
		data, err := os.ReadFile(filepath.Join(source, name))
		require.NoError(t, err)
		assert.Equal(t, fakeBinary(name, "1.0.0"), data)
	}
	require.NoError(t, os.Remove(marker))
	require.NoError(t, os.Remove(obstruction))
	_, moved, err := ensureManagedLayout(l, filepath.Join(source, nodeBinaryName), source, "1.0.0")
	require.NoError(t, err)
	assert.True(t, moved)
	assertManagedBinaries(t, l, "1.0.0")
}

func TestEnsureManagedLayout_PublicFailureRecoversThroughManagedNode(t *testing.T) {
	requireSymlinks(t)
	l := &layout{root: t.TempDir()}
	source := t.TempDir()
	for _, name := range []string{nodeBinaryName, launcherBinaryName, clientBinaryName} {
		require.NoError(t, os.WriteFile(filepath.Join(source, name), fakeBinary(name, "1.0.0"), 0755))
	}
	obstruction := filepath.Join(source, clientBinaryName) + ".tmp"
	require.NoError(t, os.Mkdir(obstruction, 0755))
	marker := filepath.Join(obstruction, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("obstruct public link"), 0600))
	_, _, err := ensureManagedLayout(l, filepath.Join(source, nodeBinaryName), source, "1.0.0")
	require.ErrorIs(t, err, ErrNotRelocatable)
	assertManagedBinaries(t, l, "1.0.0")
	require.NoError(t, os.Remove(marker))
	require.NoError(t, os.Remove(obstruction))
	_, moved, err := ensureManagedLayout(l, filepath.Join(l.versionBinDir("1.0.0"), nodeBinaryName), source, "1.0.0")
	require.NoError(t, err)
	assert.False(t, moved)
	for _, name := range []string{nodeBinaryName, launcherBinaryName, clientBinaryName} {
		target, err := os.Readlink(filepath.Join(source, name))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(l.binDir(), name), target)
	}
	assertManagedBinaries(t, l, "1.0.0")
}
