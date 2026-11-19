package security

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGiveOwnerAsTheOwnerChangesNothing: a process that already runs as the
// reference's owner leaves the file as it is; a missing reference is an error.
// Changing the owner needs another account, so a privileged run is not proven
// here.
func TestGiveOwnerAsTheOwnerChangesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	before, err := os.Lstat(path)
	require.NoError(t, err)
	require.NoError(t, GiveOwner(path, dir))
	after, err := os.Lstat(path)
	require.NoError(t, err)
	require.Equal(t, before.Sys(), after.Sys(), "owner, group and change time are as they were")
	require.ErrorIs(t, GiveOwner(path, filepath.Join(dir, "absent")), fs.ErrNotExist)
}
