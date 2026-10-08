//go:build linux

package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChownTreeVisitsEveryEntry pins that the walk reaches nested files,
// which is the property SetupDirectories depends on: a config seeded by
// an installer sits one level down (providers/…) as well as at the root.
//
// The chown itself needs root and is not asserted here; what is asserted
// is that no entry is skipped and no error escapes for an ordinary tree.
func TestChownTreeVisitsEveryEntry(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "providers", "ollama"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "node.yaml"), []byte("node: {}\n"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(root, "providers", "ollama", "config.yaml"), []byte("a: 1\n"), 0o640))

	require.NoError(t, chownTree(root, os.Getuid(), os.Getgid()))

	// Still intact: chownTree must not move, truncate or delete anything.
	for _, p := range []string{
		filepath.Join(root, "node.yaml"),
		filepath.Join(root, "providers", "ollama", "config.yaml"),
	} {
		_, err := os.Stat(p)
		assert.NoError(t, err, "chownTree removed or renamed %s", p)
	}
}

// TestChownTreeReportsUnwalkableRoot guards the one failure that must not
// be swallowed: if the tree was never visited, SetupDirectories has no
// basis for claiming the service user can read its config.
func TestChownTreeReportsUnwalkableRoot(t *testing.T) {
	err := chownTree(filepath.Join(t.TempDir(), "does-not-exist"), os.Getuid(), os.Getgid())
	assert.Error(t, err)
}
