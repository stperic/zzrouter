package servercli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/host"
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

// TestRecoveryInventoryReportsTheStore: recovery inventory prints the
// inventory as before and a store section that holds only the closed fields:
// its state, the store's identity, a refusal's reason and next action, and the
// archives, with no path and no secret; with the node running it is in_use.
func TestRecoveryInventoryReportsTheStore(t *testing.T) {
	dir, meta := recoveryInstallation(t)
	temp := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, temp)
	}
	// Exclusions follow the providers' map order.
	sorted := func(inventory recovery.Inventory) recovery.Inventory {
		for _, source := range inventory.Sources {
			slices.Sort(source.Exclude)
		}
		return inventory
	}
	inventory, err := recovery.Read(t.Context(), dir)
	require.NoError(t, err)
	inspect := func() map[string]any {
		t.Helper()
		out, err := runRecovery(t, dir, "inventory")
		require.NoError(t, err)
		var printed struct {
			Inventory recovery.Inventory `json:"inventory"`
			Store     map[string]any     `json:"store"`
		}
		decoder := json.NewDecoder(strings.NewReader(out))
		decoder.DisallowUnknownFields()
		require.NoError(t, decoder.Decode(&printed))
		require.Equal(t, sorted(inventory), sorted(printed.Inventory), "the inventory is printed as before")
		require.NotContains(t, out, temp, "the copy's path is not printed")
		return printed.Store
	}
	require.Equal(t, map[string]any{"state": "setup_required", "store_id": meta.StoreID, "generation": meta.Generation,
		"schema_version": 1.0, "revision": 0.0, "sequence": 0.0, "archives": []any{}}, inspect())

	lock, err := pkgConfig.LockInstallation(dir)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"state": "in_use", "next_action": "A node is running: read its access store's state from GET /zzrouter/v1/system/state.", "archives": []any{}}, inspect())
	require.NoError(t, lock.Close())

	require.NoError(t, os.Remove(filepath.Join(dir, "access", "access.sqlite")))
	require.Equal(t, map[string]any{"state": "refused", "reason": "missing", "next_action": (&sqlstore.Refusal{Reason: sqlstore.Missing}).NextAction(), "archives": []any{}}, inspect())
}

// rewriteImage rewrites archive with its database image replaced by what edit
// makes of it, and the image's digest and size recomputed, as a forger would.
func rewriteImage(t *testing.T, archive string, edit func(t *testing.T, image string)) string {
	t.Helper()
	in, err := os.Open(archive)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	compressed, err := gzip.NewReader(in)
	require.NoError(t, err)
	reader := tar.NewReader(compressed)
	var names []string
	entries := make(map[string][]byte)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		entries[header.Name], err = io.ReadAll(reader)
		require.NoError(t, err)
		names = append(names, header.Name)
	}
	image := filepath.Join(t.TempDir(), "image.sqlite")
	require.NoError(t, os.WriteFile(image, entries["store/image.sqlite"], 0o600))
	edit(t, image)
	data, err := os.ReadFile(image)
	require.NoError(t, err)
	entries["store/image.sqlite"] = data
	var manifest recovery.Manifest
	require.NoError(t, json.Unmarshal(entries["manifest.json"], &manifest))
	for i, file := range manifest.Files {
		if file.Name == "store/image.sqlite" {
			sum := sha256.Sum256(data)
			manifest.Files[i].Size, manifest.Files[i].SHA256 = int64(len(data)), hex.EncodeToString(sum[:])
		}
	}
	entries["manifest.json"], err = json.Marshal(manifest)
	require.NoError(t, err)
	forged := filepath.Join(t.TempDir(), "forged.tar.gz")
	out, err := os.Create(forged)
	require.NoError(t, err)
	gz := gzip.NewWriter(out)
	writer := tar.NewWriter(gz)
	for _, name := range names {
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(entries[name]))}))
		_, err := writer.Write(entries[name])
		require.NoError(t, err)
	}
	require.NoError(t, errors.Join(writer.Close(), gz.Close(), out.Close()))
	return forged
}

func execImage(query string) func(t *testing.T, image string) {
	return func(t *testing.T, image string) {
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(image))
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), query)
		require.NoError(t, errors.Join(err, db.Close()))
	}
}

// TestRecoveryCommandsGiveTheArchiveItsOwnAdvice: check and restore of an
// archive whose image is refused print the reason with advice about the
// archive, never the live store's next action.
func TestRecoveryCommandsGiveTheArchiveItsOwnAdvice(t *testing.T) {
	dir, _ := recoveryInstallation(t)
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	_, err := runRecovery(t, dir, "backup", "--output", archive)
	require.NoError(t, err)
	for reason, edit := range map[sqlstore.Reason]func(t *testing.T, image string){
		sqlstore.IntegrityFailed: execImage("CREATE TRIGGER planted AFTER INSERT ON teams BEGIN SELECT 1; END"),
		sqlstore.SchemaNewer:     execImage("PRAGMA user_version = 99"),
		sqlstore.Unreadable: func(t *testing.T, image string) {
			require.NoError(t, os.WriteFile(image, []byte("not a database, just text long enough to have a header"), 0o600))
		},
	} {
		t.Run(string(reason), func(t *testing.T) {
			forged := rewriteImage(t, archive, edit)
			for _, args := range [][]string{{"check", forged}, {"restore", forged, "--to", filepath.Join(t.TempDir(), "staged")}} {
				out, err := runRecovery(t, dir, args...)
				require.Error(t, err)
				printed := out + err.Error()
				require.Contains(t, printed, "the archive's access database image is refused: "+string(reason))
				require.NotContains(t, printed, "remove the access directory")
				require.NotContains(t, printed, "Stop the service")
			}
		})
	}
}

const recoveryChildArgs = "ZZROUTER_RECOVERY_CHILD_ARGS"

// TestRecoveryInterruptChild runs the recovery command its parent names in
// recoveryChildArgs; it is TestRecoveryCommandsStopOnInterrupt's child.
func TestRecoveryInterruptChild(t *testing.T) {
	args := os.Getenv(recoveryChildArgs)
	if args == "" {
		return
	}
	cmd := NewRecoveryCmd()
	cmd.SetArgs(strings.Split(args, "\n"))
	require.NoError(t, cmd.Execute())
}

// TestRecoveryCommandsStopOnInterrupt interrupts a running recovery check: it
// stops with an error and removes its temporary directory.
func TestRecoveryCommandsStopOnInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot send an interrupt to another process")
	}
	dir, _ := recoveryInstallation(t)
	// A large compressible file keeps the check busy after its temporary exists.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "large.bin"), make([]byte, 64<<20), 0o600))
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	_, err := runRecovery(t, dir, "backup", "--output", archive)
	require.NoError(t, err)
	temp := t.TempDir()
	// The child inherits these.
	t.Setenv("TMPDIR", temp)
	t.Setenv(recoveryChildArgs, strings.Join([]string{"check", archive, "--dir", dir}, "\n"))
	child := host.Command(os.Args[0], "-test.run=^TestRecoveryInterruptChild$", "-test.count=1")
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill() })
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(temp)
		return err == nil && len(entries) > 0
	}, time.Minute, time.Millisecond, "the check created its temporary directory")
	require.NoError(t, child.Process.Signal(os.Interrupt))
	require.Error(t, child.Wait(), "an interrupted check fails")
	entries, err := os.ReadDir(temp)
	require.NoError(t, err)
	require.Empty(t, entries, "the interrupted check removed its temporary directory")
}
