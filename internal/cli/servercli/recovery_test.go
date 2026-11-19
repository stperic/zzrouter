package servercli

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/recovery"
	"github.com/stretchr/testify/require"
)

// recoveryInstallation is a stopped coordinator installation that started
// once: its recorded layout, its installation lock and an access store.
func recoveryInstallation(t *testing.T) (string, sqlstore.Meta) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)
	t.Setenv("ZZROUTER_CONFIG_DIR", root)
	previous := fsroot.ProviderRootDir()
	fsroot.SetProviderRootOverride(filepath.Join(root, "installed-providers"))
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previous) })
	_, err := templates.PrepareProviders(filepath.Join(root, "providers"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "node.yaml"), []byte("node:\n  name: recovery-cli\n  port: 9090\n"), 0o600))
	cfg, err := pkgConfig.ReadExistingNodeConfig(root)
	require.NoError(t, err)
	apps, err := pkgConfig.LoadAppsConfig(filepath.Join(root, "providers"))
	require.NoError(t, err)
	inventory, err := recovery.Capture(t.Context(), cfg, apps)
	require.NoError(t, err)
	require.NoError(t, recovery.Save(t.Context(), inventory))
	lock, err := pkgConfig.LockInstallation(root)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	store, err := sqlstore.Open(t.Context(), root, sqlstore.Options{})
	require.NoError(t, err)
	meta := store.Meta()
	require.NoError(t, store.Close())
	return root, meta
}

func runRecovery(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := NewRecoveryCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append(args, "--dir", dir))
	err := cmd.ExecuteContext(t.Context())
	t.Logf("zzrouter-node recovery %v:\n%s", args, out.String())
	return out.String(), err
}

// TestRecoveryCommandsNameTheProfileAndStore: backup, check and restore name
// the bundle's profile and its store's identity, restore names the prepared
// generation, a newer release's database is saved with a note, and a node
// without a store says it has none.
func TestRecoveryCommandsNameTheProfileAndStore(t *testing.T) {
	dir, meta := recoveryInstallation(t)
	identity := "Access store " + meta.StoreID + ": generation " + meta.Generation + ", schema version 1, revision 0, sequence 0."
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	out, err := runRecovery(t, dir, "backup", "--output", archive)
	require.NoError(t, err)
	require.Contains(t, out, "Saved and checked")
	require.Contains(t, out, "profile installation, for recovery-cli (")
	require.Contains(t, out, identity)
	out, err = runRecovery(t, dir, "check", archive)
	require.NoError(t, err)
	require.Contains(t, out, "profile installation")
	require.Contains(t, out, identity)
	require.NotContains(t, out, "Prepared")
	out, err = runRecovery(t, dir, "restore", archive, "--to", filepath.Join(t.TempDir(), "staged"))
	require.NoError(t, err)
	require.Contains(t, out, identity)
	require.Contains(t, out, "Prepared the staged access database as generation ")
	require.NotContains(t, out, dir, "no installation path is printed")

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "access", "access.sqlite")))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "PRAGMA user_version = 99")
	require.NoError(t, errors.Join(err, db.Close()))
	newer := filepath.Join(t.TempDir(), "newer.tar.gz")
	out, err = runRecovery(t, dir, "backup", "--output", newer)
	require.NoError(t, err, "a newer release's database is saved for safety")
	require.Contains(t, out, "schema version 99")
	require.Contains(t, out, "A newer release wrote the access database")
	_, err = runRecovery(t, dir, "check", newer)
	var refusal *sqlstore.Refusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, sqlstore.SchemaNewer, refusal.Reason)

	require.NoError(t, os.RemoveAll(filepath.Join(dir, "access")))
	require.NoError(t, os.Remove(filepath.Join(dir, sqlstore.MarkerName)))
	out, err = runRecovery(t, dir, "backup", "--output", filepath.Join(t.TempDir(), "worker.tar.gz"))
	require.NoError(t, err)
	require.Contains(t, out, "No access store")
}
