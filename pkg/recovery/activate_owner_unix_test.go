//go:build !windows

package recovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRequireOwner: the installation's owner passes; another account, here
// one that does not own the root directory, is refused with the command to
// repeat as that owner; and an installation named by a link is refused.
func TestRequireOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns the directory this test needs another account to own")
	}
	dir := t.TempDir()
	require.NoError(t, RequireOwner(dir, "zzrouter-node recovery activate /staged"))
	refusal := requireRefusal(t, RequireOwner("/", "zzrouter-node recovery activate /staged --dir /"), NotOwner)
	require.Equal(t, "Run it as the installation's owner: sudo -u root zzrouter-node recovery activate /staged --dir /", refusal.NextAction)
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	require.Contains(t, requireRefusal(t, RequireOwner(link, "zzrouter-node recovery activate /staged"), NotOwner).Detail, "is a link")
}
