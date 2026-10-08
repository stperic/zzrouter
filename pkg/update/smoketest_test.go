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

// writeScript drops an executable stand-in binary and returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "candidate")
	require.NoError(t, os.WriteFile(path, []byte(body), 0750))
	return path
}

func TestSmokeTest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-ins are shell scripts")
	}

	v2 := &version.Version{Major: 2, Minor: 0, Patch: 0}

	t.Run("accepts a binary reporting the expected version", func(t *testing.T) {
		path := writeScript(t, "#!/bin/sh\necho \"zzrouter-node version 2.0.0\"\n")
		require.NoError(t, smokeTest(context.Background(), path, v2))
	})

	t.Run("accepts any version when none is expected", func(t *testing.T) {
		path := writeScript(t, "#!/bin/sh\necho \"zzrouter-node version 1.2.3\"\n")
		require.NoError(t, smokeTest(context.Background(), path, nil))
	})

	// The case the smoke test exists for: a checksum-clean download that
	// cannot run here at all, which without this check would be swapped
	// in and then exited into.
	t.Run("rejects a binary that will not execute", func(t *testing.T) {
		path := writeScript(t, "this is not a program")
		err := smokeTest(context.Background(), path, v2)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSmokeTestFailed)
	})

	t.Run("rejects a binary that exits non-zero", func(t *testing.T) {
		path := writeScript(t, "#!/bin/sh\necho 'cannot load library' >&2\nexit 1\n")
		err := smokeTest(context.Background(), path, v2)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSmokeTestFailed)
		assert.Contains(t, err.Error(), "cannot load library")
	})

	// Catches serving the wrong release behind a valid checksum.
	t.Run("rejects a binary reporting a different version", func(t *testing.T) {
		path := writeScript(t, "#!/bin/sh\necho \"zzrouter-node version 1.0.0\"\n")
		err := smokeTest(context.Background(), path, v2)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSmokeTestFailed)
		assert.Contains(t, err.Error(), "the release is 2.0.0")
	})

	t.Run("rejects a binary that prints nothing", func(t *testing.T) {
		path := writeScript(t, "#!/bin/sh\nexit 0\n")
		err := smokeTest(context.Background(), path, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSmokeTestFailed)
	})

	t.Run("rejects a missing binary", func(t *testing.T) {
		err := smokeTest(context.Background(), filepath.Join(t.TempDir(), "absent"), nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSmokeTestFailed)
	})
}

// TestInstall_SmokeTestFailureChangesNothing is the whole point: a
// candidate that cannot run must leave the node on the binary it is
// already running, with no backup churn and no half-swapped pair.
func TestInstall_SmokeTestFailureChangesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	currentNode := filepath.Join(tmpDir, "zzrouter-node")
	currentLauncher := filepath.Join(tmpDir, "zzrouter-launcher")
	require.NoError(t, os.WriteFile(currentNode, fakeBinary("zzrouter-node", "1.0.0"), 0750))
	require.NoError(t, os.WriteFile(currentLauncher, fakeBinary("zzrouter-launcher", "1.0.0"), 0750))

	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentNode)

	archivePath := filepath.Join(tmpDir, "update.tar.gz")
	createTestTarGz(t, archivePath, map[string][]byte{
		"zzrouter-launcher-linux-amd64": fakeBinary("zzrouter-launcher", "2.0.0"),
		"zzrouter-node-linux-amd64":     []byte("truncated download, valid checksum"),
	})

	_, err := i.Install(context.Background(), archivePath, &version.Version{Major: 2})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSmokeTestFailed)

	node, err := os.ReadFile(currentNode)
	require.NoError(t, err)
	assert.Equal(t, fakeBinary("zzrouter-node", "1.0.0"), node)

	launcher, err := os.ReadFile(currentLauncher)
	require.NoError(t, err)
	assert.Equal(t, fakeBinary("zzrouter-launcher", "1.0.0"), launcher,
		"the launcher must not be replaced for a node that was never installed")

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a failed smoke test should not have backed anything up")
}
