package preflight

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommands(t *testing.T) {
	results := Commands("ls", "nonexistent-tool-xyz")
	require.Len(t, results, 2)
	assert.True(t, results[0].Passed, "ls should exist")
	assert.False(t, results[1].Passed, "nonexistent tool should not exist")
}

func TestWritePermission(t *testing.T) {
	// Writable directory should pass
	dir := t.TempDir()
	assert.NoError(t, WritePermission(dir))

	// Non-existent subdirectory of writable parent should pass
	assert.NoError(t, WritePermission(filepath.Join(dir, "sub", "deep")))

	// Read-only directory should fail (Unix only)
	if runtime.GOOS != "windows" {
		readonly := filepath.Join(dir, "readonly")
		require.NoError(t, os.MkdirAll(readonly, 0555))
		err := WritePermission(readonly)
		if os.Geteuid() == 0 {
			// Root bypasses Unix directory permission bits, including in Docker.
			assert.NoError(t, err)
		} else {
			assert.ErrorIs(t, err, ErrNoWritePermission)
		}
		// Cleanup: restore write permission so t.TempDir() cleanup works
		_ = os.Chmod(readonly, 0755)
	}
}
