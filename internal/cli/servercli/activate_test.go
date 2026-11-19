package servercli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/recovery"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

// databaseArchive writes a database bundle of dir's store as it is now: the
// image alone, as an online backup writes one.
func databaseArchive(t *testing.T, dir string) string {
	t.Helper()
	store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	image := filepath.Join(t.TempDir(), "image.sqlite")
	meta, err := store.Capture(t.Context(), image)
	require.NoError(t, errors.Join(err, store.Close()))
	data, err := os.ReadFile(image)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	described := recovery.StoreMeta(meta)
	manifest, err := json.Marshal(recovery.Manifest{Version: 1, Profile: recovery.ProfileDatabase, Created: utils.NowUTC(), Store: &described,
		Files: []recovery.File{{Name: "store/image.sqlite", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}}})
	require.NoError(t, err)
	archive := filepath.Join(t.TempDir(), "database.tar.gz")
	out, err := os.Create(archive)
	require.NoError(t, err)
	gz := gzip.NewWriter(out)
	writer := tar.NewWriter(gz)
	for _, entry := range []struct {
		name string
		data []byte
	}{{"manifest.json", manifest}, {"store/image.sqlite", data}} {
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o600, Size: int64(len(entry.data))}))
		_, err := writer.Write(entry.data)
		require.NoError(t, err)
	}
	require.NoError(t, errors.Join(writer.Close(), gz.Close(), out.Close()))
	return archive
}

// treeOf maps every file under root to its content.
func treeOf(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	}))
	return files
}

// TestRecoveryActivateCommand: a dry run, then an activation interrupted after
// its marker, which says it is incomplete and which the node's start refuses
// before writing anything into the installation; a rollback, then the
// activation: each names what it replaced, the generation and the rollback, and
// the activation the next step.
func TestRecoveryActivateCommand(t *testing.T) {
	dir, meta := recoveryInstallation(t)
	archive := databaseArchive(t, dir)
	staging := filepath.Join(t.TempDir(), "staged")
	out, err := runRecovery(t, dir, "restore", archive, "--to", staging)
	require.NoError(t, err)
	require.Contains(t, out, "Staged only. Keep the node stopped and publish it with zzrouter-node recovery activate "+staging+" --dir "+dir+", after a --dry-run.")
	var manifest recovery.Manifest
	data, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &manifest))
	generation := manifest.Staged.Generation
	rollback := filepath.Join(dir, "rollback", generation)

	out, err = runRecovery(t, dir, "activate", staging, "--dry-run")
	require.NoError(t, err)
	require.Contains(t, out, "Dry run: activating would publish generation "+generation+" of access store "+meta.StoreID+".\nIt would replace this installation's access store, keeping its database and store marker in "+rollback+".\nA dry run publishes nothing, replaces nothing and creates no rollback directory; reading the database may have let SQLite finish recovering a journal")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = recovery.Activate(sqlitedb.WithStep(ctx, func(step string) {
		if step == "rollback record" {
			cancel() // an interrupt
		}
	}), dir, staging, recovery.ActivateOptions{})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, strings.HasPrefix(err.Error(), "activation_incomplete: "), "the closed reason comes first: %v", err)
	var refusal *recovery.Refusal
	require.ErrorAs(t, err, &refusal, "the interrupted command says the activation is incomplete")
	require.Equal(t, recovery.ActivationIncomplete, refusal.Reason)
	installation := treeOf(t, dir)
	err = runNormalStart(NewStartCmd(), dir, &startServiceManager{})
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, recovery.ActivationIncomplete, refusal.Reason)
	require.Contains(t, err.Error(), "Stop the service, then run zzrouter-node recovery activate "+staging+" --dir "+dir+" to finish it")
	require.Equal(t, installation, treeOf(t, dir), "the refused start wrote nothing")
	out, err = runRecovery(t, dir, "inventory")
	require.NoError(t, err)
	var printed struct {
		Store recovery.StoreSection `json:"store"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &printed))
	require.Equal(t, recovery.StoreActivationIncomplete, printed.Store.State)
	require.Contains(t, printed.Store.NextAction, "zzrouter-node recovery activate "+staging)
	require.Equal(t, generation, printed.Store.Rollbacks[0].ID)

	out, err = runRecovery(t, dir, "activate", staging, "--rollback", "--dry-run")
	require.NoError(t, err)
	require.Contains(t, out, "Dry run: rolling back activation "+generation+" would put back the access store's database and store marker kept in "+rollback+". A dry run changes nothing.")
	out, err = runRecovery(t, dir, "activate", staging, "--rollback")
	require.NoError(t, err)
	require.Contains(t, out, "Rolled back activation "+generation+": the access store's database and store marker are as they were before. The staging directory is kept.")
	require.NoError(t, recovery.PendingActivation(dir))

	out, err = runRecovery(t, dir, "activate", staging)
	require.NoError(t, err)
	require.Contains(t, out, "Activated generation "+generation+" of access store "+meta.StoreID+" (schema version 1, revision 0, sequence 1).")
	require.Contains(t, out, "The previous database and store marker, credential hashes included, are kept in "+rollback+" until you delete it.")
	require.Contains(t, out, "Next: start the node. Every session and delegated access issued before this restore is invalid")
	require.NoDirExists(t, staging)

	_, err = runRecovery(t, dir, "activate", staging, "--rollback")
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, recovery.NoActivation, refusal.Reason, "D5: a finished activation is not rolled back")
}

// TestRecoveryActivateOnAFreshInstallation: with no access store to replace,
// each message says so in its own words.
func TestRecoveryActivateOnAFreshInstallation(t *testing.T) {
	source, meta := recoveryInstallation(t)
	staging := filepath.Join(t.TempDir(), "staged")
	_, err := runRecovery(t, source, "restore", databaseArchive(t, source), "--to", staging)
	require.NoError(t, err)
	fresh := t.TempDir()
	lock, err := pkgConfig.LockInstallation(fresh)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	out, err := runRecovery(t, fresh, "activate", staging, "--dry-run")
	require.NoError(t, err)
	require.Contains(t, out, "This installation has no access store, so nothing would be replaced.")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = recovery.Activate(sqlitedb.WithStep(ctx, func(step string) {
		if step == "rollback record" {
			cancel()
		}
	}), fresh, staging, recovery.ActivateOptions{})
	require.ErrorIs(t, err, context.Canceled)
	out, err = runRecovery(t, fresh, "activate", staging, "--rollback")
	require.NoError(t, err)
	require.Contains(t, out, "the access store it published is removed, since this installation had none before.")
	out, err = runRecovery(t, fresh, "activate", staging)
	require.NoError(t, err)
	require.Contains(t, out, "of access store "+meta.StoreID+" (")
	require.Contains(t, out, "This installation had no access store, so nothing was replaced; "+filepath.Join(fresh, "rollback")+string(filepath.Separator))
	require.NotContains(t, out, "credential hashes")
}

// crashAt runs an activation, or its rollback, that ends at step as a crash
// would: no later step runs. In-process, so nothing is forked.
func crashAt(t *testing.T, dir, staging, step string, o recovery.ActivateOptions) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = recovery.Activate(sqlitedb.WithStep(t.Context(), func(reached string) {
			if reached == step {
				runtime.Goexit()
			}
		}), dir, staging, o)
	}()
	<-done
}

// TestRecoveryActivateTellsWhatItKept: a staging directory inside the
// installation, also through a link, is refused at restore time with nothing
// written; a rollback
// whose staging directory is gone does not claim to keep it; and an activation
// finished after its pending rollback directory was deleted says the previous
// store was not kept.
func TestRecoveryActivateTellsWhatItKept(t *testing.T) {
	dir, _ := recoveryInstallation(t)
	archive := databaseArchive(t, dir)
	installation := treeOf(t, dir)
	_, err := runRecovery(t, dir, "restore", archive, "--to", filepath.Join(dir, "staged"))
	var refusal *recovery.Refusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, recovery.StagingInvalid, refusal.Reason)
	require.Equal(t, installation, treeOf(t, dir), "nothing written")
	require.NoDirExists(t, filepath.Join(dir, "staged"))
	if runtime.GOOS != "windows" {
		// A link outside the installation to a directory inside it, as the
		// staging directory's parent.
		link := filepath.Join(t.TempDir(), "link")
		require.NoError(t, os.Symlink(filepath.Join(dir, "providers"), link))
		_, err = runRecovery(t, dir, "restore", archive, "--to", filepath.Join(link, "staged"))
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, recovery.StagingInvalid, refusal.Reason)
		require.Equal(t, installation, treeOf(t, dir), "nothing written through the link")
	}

	staging := filepath.Join(t.TempDir(), "staged")
	_, err = runRecovery(t, dir, "restore", archive, "--to", staging)
	require.NoError(t, err)
	crashAt(t, dir, staging, "rollback record", recovery.ActivateOptions{})
	moved := filepath.Join(t.TempDir(), "moved")
	require.NoError(t, os.Rename(staging, moved))
	out, err := runRecovery(t, dir, "activate", staging, "--rollback")
	require.NoError(t, err)
	require.Contains(t, out, "are as they were before.")
	require.NotContains(t, out, "The staging directory is kept.")

	require.NoError(t, os.Rename(moved, staging))
	crashAt(t, dir, staging, "renamed", recovery.ActivateOptions{})
	var manifest recovery.Manifest
	data, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &manifest))
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "rollback", manifest.Staged.Generation)))
	out, err = runRecovery(t, dir, "activate", staging)
	require.NoError(t, err)
	require.Contains(t, out, "The previous database and store marker were not kept: the rollback directory was deleted while the activation was incomplete")
	require.NotContains(t, out, "are kept in")
}

// TestUpdateRunRefusesDuringAnIncompleteActivation: the privileged update run
// refuses with activation_incomplete before it installs anything, and
// publishes the refusal as its failed run.
func TestUpdateRunRefusesDuringAnIncompleteActivation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", dir)
	t.Setenv("ZZROUTER_CONFIG_DIR", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "recovery-activation.json"), []byte(`{"version":1,"profile":"database","staging":"/staged","generation":"g"}`), 0o600))
	h := update.NewHandoff(t.TempDir(), t.TempDir())
	err := executeUpdateRequest(t.Context(), h, &update.Request{Action: update.ActionApply, TargetVersion: "9.9.9", RequestedBy: "cli"}, false,
		func() (*pkgConfig.NodeConfig, error) { return &pkgConfig.NodeConfig{}, nil })
	var refusal *recovery.Refusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, recovery.ActivationIncomplete, refusal.Reason)
	status, err := h.LastRun()
	require.NoError(t, err)
	require.Equal(t, update.StateFailed, status.State)
	require.Contains(t, status.Error, "activation_incomplete")
}

// TestRecoveryActivateHandoffCommand: on a destination that holds its own
// store, activation of a staged handoff refuses without --source-stopped, then
// without --replace; a dry run names what it would replace and the node names
// to review; an interrupted activation is rolled back with its own words; and
// the activation names what it replaced, the node names and the next steps.
func TestRecoveryActivateHandoffCommand(t *testing.T) {
	source, _ := recoveryInstallation(t)
	groups := modelgroup.NewGroupStore()
	require.NoError(t, groups.Set("assistant", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "local", Model: "qwen", App: "llamacpp"}, {Name: "old", Model: "qwen", App: "llamacpp", Node: "recovery-cli"}}}))
	require.NoError(t, groups.SaveToFile(filepath.Join(source, "model_groups.yaml")))
	archive := filepath.Join(t.TempDir(), "handoff.tar.gz")
	_, err := runRecovery(t, source, "backup", "--profile", "handoff", "--output", archive)
	require.NoError(t, err)
	dir, _ := recoveryInstallation(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), []byte("node:\n  name: recovery-dest\n  port: 9090\n"), 0o600))
	staging := filepath.Join(t.TempDir(), "staged")
	_, err = runRecovery(t, dir, "restore", archive, "--to", staging)
	require.NoError(t, err)
	var manifest recovery.Manifest
	data, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &manifest))
	generation, rollback := manifest.Staged.Generation, filepath.Join(dir, "rollback", manifest.Staged.Generation)
	references := "Node names the handoff's configuration holds, unchanged; review each once the node runs:\n" +
		`  route assistant, replica local (pinned nowhere: it served on the source, recovery-cli): zz_api {"method": "PATCH", "path": "/zzrouter/v1/model-groups/assistant/replicas/local", "body": {"node": "<node>"}}` + "\n" +
		`  route assistant, replica old (the source): zz_api {"method": "PATCH", "path": "/zzrouter/v1/model-groups/assistant/replicas/old", "body": {"node": "<node>"}}` + "\n"

	var refusal *recovery.Refusal
	_, err = runRecovery(t, dir, "activate", staging, "--dry-run")
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, recovery.SourceNotStopped, refusal.Reason)
	_, err = runRecovery(t, dir, "activate", staging, "--dry-run", "--source-stopped")
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, recovery.ReplaceRequired, refusal.Reason)
	out, err := runRecovery(t, dir, "activate", staging, "--dry-run", "--source-stopped", "--replace")
	require.NoError(t, err)
	require.Contains(t, out, "Dry run: activating would publish generation "+generation+" of access store "+manifest.Store.StoreID+" and the handoff's coordinator configuration.\nIt would replace, keeping each in "+rollback+": store.\n"+references+"A dry run publishes nothing")

	crashAt(t, dir, staging, "published model_groups.yaml", recovery.ActivateOptions{SourceStopped: true, Replace: true})
	out, err = runRecovery(t, dir, "activate", staging, "--rollback", "--dry-run")
	require.NoError(t, err)
	require.Contains(t, out, "Dry run: rolling back activation "+generation+" would put back what it replaced from "+rollback+" and remove what it created. A dry run changes nothing.")
	out, err = runRecovery(t, dir, "activate", staging, "--rollback")
	require.NoError(t, err)
	require.Contains(t, out, "Rolled back activation "+generation+": the access store and configuration files it replaced are as they were before, and what it created is removed. The staging directory is kept.")
	require.NoFileExists(t, filepath.Join(dir, "model_groups.yaml"))

	out, err = runRecovery(t, dir, "activate", staging, "--source-stopped", "--replace")
	require.NoError(t, err)
	require.Contains(t, out, "Activated generation "+generation+" of access store "+manifest.Store.StoreID+" (schema version 1, revision 0, sequence 1) and the handoff's coordinator configuration.\nReplaced from this installation and kept in "+rollback+" until you delete it, credential hashes and connections included: store.\n"+references+"The staging directory is removed. Next: start the node")
	require.NotContains(t, out, "—")
	require.FileExists(t, filepath.Join(dir, "model_groups.yaml"))
}

// TestTemplateWritersRefuseDuringAnIncompleteActivation: init and config reset,
// which write provider templates without the installation lock, refuse with
// activation_incomplete and write nothing while an activation is incomplete.
func TestTemplateWritersRefuseDuringAnIncompleteActivation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", dir)
	t.Setenv("ZZROUTER_CONFIG_DIR", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "recovery-activation.json"), []byte(`{"version":1,"profile":"handoff","staging":"/staged","generation":"g"}`), 0o600))
	installation := treeOf(t, dir)
	for name, run := range map[string]func() error{"init": func() error { return runNodeInit(true) }, "config reset": func() error { return runNodeConfigReset(true) }} {
		var refusal *recovery.Refusal
		require.ErrorAs(t, run(), &refusal, name)
		require.Equal(t, recovery.ActivationIncomplete, refusal.Reason, name)
		require.Equal(t, installation, treeOf(t, dir), "%s wrote nothing", name)
	}
}
