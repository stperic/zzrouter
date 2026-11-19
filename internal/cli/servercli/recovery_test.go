package servercli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
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

// inventoryOutput is what recovery inventory printed.
type inventoryOutput struct {
	Inventory *recovery.Inventory `json:"inventory"`
	Store     map[string]any      `json:"store"`
}

// runInventory runs recovery inventory on dir and decodes its standard
// output, which must be the two sections and nothing else, also on an error.
func runInventory(t *testing.T, dir string) (inventoryOutput, error) {
	t.Helper()
	cmd := NewRecoveryCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"inventory", "--dir", dir})
	err := cmd.ExecuteContext(t.Context())
	t.Logf("zzrouter-node recovery inventory:\n%s%s", stdout.String(), stderr.String())
	var printed inventoryOutput
	decoder := json.NewDecoder(&stdout)
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&printed))
	require.False(t, decoder.More(), "nothing follows the sections on standard output")
	return printed, err
}

// sortedExclusions sorts each source's exclusions, which follow the
// providers' map order.
func sortedExclusions(inventory recovery.Inventory) recovery.Inventory {
	for _, source := range inventory.Sources {
		slices.Sort(source.Exclude)
	}
	return inventory
}

// TestRecoveryInventoryReportsTheStore: recovery inventory prints the
// inventory as before and a store section that holds only the closed fields:
// its state, the store's identity, a refusal's reason, evidence and next
// action, and the archives, with no path and no secret; with the node running
// it is in_use.
func TestRecoveryInventoryReportsTheStore(t *testing.T) {
	dir, meta := recoveryInstallation(t)
	temp := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, temp)
	}
	inventory, err := recovery.Read(t.Context(), dir)
	require.NoError(t, err)
	inspect := func() map[string]any {
		t.Helper()
		printed, err := runInventory(t, dir)
		require.NoError(t, err)
		require.NotNil(t, printed.Inventory)
		require.Equal(t, sortedExclusions(inventory), sortedExclusions(*printed.Inventory), "the inventory is printed as before")
		encoded, err := json.Marshal(printed.Store)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), temp, "the copy's path is not printed")
		return printed.Store
	}
	require.Equal(t, map[string]any{"state": "setup_required", "store_id": meta.StoreID, "generation": meta.Generation,
		"schema_version": 1.0, "revision": 0.0, "sequence": 0.0, "archives": []any{}}, inspect())

	lock, err := pkgConfig.LockInstallation(dir)
	require.NoError(t, err)
	inUse := inspect()
	require.Equal(t, "in_use", inUse["state"])
	require.Contains(t, inUse["next_action"], "The installation is locked by a running node or another recovery command. On a coordinator, GET /zzrouter/v1/system/state reports its access store; a worker has none.")
	require.NoError(t, lock.Close())

	require.NoError(t, os.Remove(filepath.Join(dir, "access", "access.sqlite")))
	require.Equal(t, map[string]any{"state": "refused", "reason": "missing", "evidence": "missing",
		"next_action": (&sqlstore.Refusal{Reason: sqlstore.Missing}).NextAction(), "archives": []any{}}, inspect())
}

// TestRecoveryInventoryWithoutALayout: a store refused at the first start
// stops the node before it records its layout; the inventory still prints the
// store section, with a null installation section, and fails with the
// layout's error.
func TestRecoveryInventoryWithoutALayout(t *testing.T) {
	dir, _ := recoveryInstallation(t)
	require.NoError(t, os.Remove(filepath.Join(dir, recovery.InventoryFile)))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "access", "access.sqlite"), []byte("not a database, just text long enough to have a header"), 0o600))
	printed, err := runInventory(t, dir)
	require.ErrorContains(t, err, "read recovery inventory")
	require.Nil(t, printed.Inventory)
	require.Equal(t, map[string]any{"state": "refused", "reason": "unreadable", "evidence": "unreadable",
		"next_action": (&sqlstore.Refusal{Reason: sqlstore.Unreadable}).NextAction(), "archives": []any{}}, printed.Store)
}

// TestRecoveryInventoryUnknownPrintsBothSections: an inspection that cannot
// finish prints the installation section and an unknown store section whose
// evidence is the error, and fails; a lock file it cannot write is not a
// running node.
func TestRecoveryInventoryUnknownPrintsBothSections(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions that bind the test's account")
	}
	dir, _ := recoveryInstallation(t)
	lock := filepath.Join(dir, "installation.lock")
	require.NoError(t, os.Chmod(lock, 0o400))
	t.Cleanup(func() { _ = os.Chmod(lock, 0o600) })
	printed, err := runInventory(t, dir)
	require.Error(t, err)
	require.NotNil(t, printed.Inventory, "the installation section is printed")
	require.Equal(t, "unknown", printed.Store["state"])
	require.Equal(t, err.Error(), printed.Store["evidence"])
	require.NotContains(t, err.Error(), "in use")
	require.Contains(t, printed.Store["next_action"], "Resolve the cause in evidence")
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
	switch args {
	case "":
		return
	case "slow cleanup":
		// An operation whose cleanup outlasts the first interrupt.
		cmd := &cobra.Command{RunE: interruptible(func(cmd *cobra.Command, _ []string) error {
			fmt.Println("ready")
			<-cmd.Context().Done()
			fmt.Println("cancelled")
			time.Sleep(time.Minute)
			return nil
		})}
		cmd.SetArgs([]string{})
		require.NoError(t, cmd.Execute())
	default:
		cmd := NewRecoveryCmd()
		cmd.SetArgs(strings.Split(args, "\n"))
		require.NoError(t, cmd.Execute())
	}
}

// TestRecoveryCommandsEndOnASecondInterrupt: once an interrupt cancelled an
// operation, the next one ends the process at once, whatever its cleanup does.
func TestRecoveryCommandsEndOnASecondInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot send an interrupt to another process")
	}
	t.Setenv(recoveryChildArgs, "slow cleanup") // the child inherits it
	child := host.Command(os.Args[0], "-test.run=^TestRecoveryInterruptChild$", "-test.count=1")
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill() })
	lines := bufio.NewScanner(stdout)
	waitFor := func(want string) {
		t.Helper()
		for lines.Scan() {
			if lines.Text() == want {
				return
			}
		}
		t.Fatalf("the child never printed %q", want)
	}
	waitFor("ready")
	require.NoError(t, child.Process.Signal(os.Interrupt))
	waitFor("cancelled")
	exited := make(chan struct{})
	go func() { _ = child.Wait(); close(exited) }()
	// The handler is released just after the cancellation, so interrupt until
	// the process ends; a swallowed interrupt leaves it sleeping.
	deadline := time.After(30 * time.Second)
	for ended := false; !ended; {
		_ = child.Process.Signal(os.Interrupt)
		select {
		case <-exited:
			ended = true
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatal("a second interrupt did not end the process")
		}
	}
	require.Equal(t, -1, child.ProcessState.ExitCode(), "ended by the signal: %s", child.ProcessState)
}

// TestRecoveryBackupSaysWhenItsCheckWasInterrupted: an interrupt during the
// check that follows a backup fails the command and says the archive was
// written but not checked, and which command checks it.
func TestRecoveryBackupSaysWhenItsCheckWasInterrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot send an interrupt to another process")
	}
	dir, _ := recoveryInstallation(t)
	// A large compressible file keeps the check busy once it began.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "large.bin"), make([]byte, 64<<20), 0o600))
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	temp := t.TempDir()
	// The child inherits these.
	t.Setenv("TMPDIR", temp)
	t.Setenv(recoveryChildArgs, strings.Join([]string{"backup", "--output", archive, "--dir", dir}, "\n"))
	child := host.Command(os.Args[0], "-test.run=^TestRecoveryInterruptChild$", "-test.count=1")
	var printed bytes.Buffer
	child.Stdout, child.Stderr = &printed, &printed
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill() })
	require.Eventually(t, func() bool {
		if _, err := os.Stat(archive); err != nil {
			return false
		}
		entries, err := os.ReadDir(temp)
		return err == nil && len(entries) > 0 && strings.HasPrefix(entries[0].Name(), "zzrouter-recovery-")
	}, time.Minute, time.Millisecond, "the backup was published and its check began")
	require.NoError(t, child.Process.Signal(os.Interrupt))
	require.Error(t, child.Wait(), "the command fails")
	require.Contains(t, printed.String(), "the backup was written, but its check was interrupted: run zzrouter-node recovery check "+archive+" before relying on it")
	require.NotContains(t, printed.String(), "do not use this archive")
	_, err := recovery.Check(t.Context(), archive, "")
	require.NoError(t, err, "the archive is complete")
	entries, err := os.ReadDir(temp)
	require.NoError(t, err)
	require.Empty(t, entries, "the interrupted check removed its temporary directory")
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
