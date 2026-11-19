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
	"testing"

	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/recovery"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
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
	require.Contains(t, out, "Staged only. Keep the node stopped and publish it with zzrouter-node recovery activate "+staging+", after a --dry-run.")
	var manifest recovery.Manifest
	data, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &manifest))
	generation := manifest.Staged.Generation
	replaced := "the access store (its database and store marker)"

	out, err = runRecovery(t, dir, "activate", staging, "--dry-run")
	require.NoError(t, err)
	require.Contains(t, out, "Dry run: activating would publish generation "+generation+" of access store "+meta.StoreID+" and replace "+replaced+", kept in rollback "+generation+". Nothing was changed.")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = recovery.Activate(sqlitedb.WithStep(ctx, func(step string) {
		if step == "rollback record" {
			cancel() // an interrupt
		}
	}), dir, staging, recovery.ActivateOptions{})
	require.ErrorIs(t, err, context.Canceled)
	var refusal *recovery.Refusal
	require.ErrorAs(t, err, &refusal, "the interrupted command says the activation is incomplete")
	require.Equal(t, recovery.ActivationIncomplete, refusal.Reason)
	installation := treeOf(t, dir)
	err = runNormalStart(NewStartCmd(), dir, &startServiceManager{})
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, recovery.ActivationIncomplete, refusal.Reason)
	require.Contains(t, err.Error(), "run zzrouter-node recovery activate "+staging+" --dir "+dir+" to finish it")
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
	require.Contains(t, out, "Dry run: rolling back activation "+generation+" would restore "+replaced+". Nothing was changed.")
	out, err = runRecovery(t, dir, "activate", staging, "--rollback")
	require.NoError(t, err)
	require.Contains(t, out, "Rolled back activation "+generation+": restored "+replaced+" as it was before. The staging directory is kept.")
	require.NoError(t, recovery.PendingActivation(dir))

	out, err = runRecovery(t, dir, "activate", staging)
	require.NoError(t, err)
	require.Contains(t, out, "Activated generation "+generation+" of access store "+meta.StoreID+" (schema version 1, revision 0, sequence 1), replacing "+replaced+".")
	require.Contains(t, out, "Rollback "+generation+" keeps what was replaced, credential hashes included, under rollback/"+generation)
	require.Contains(t, out, "Next: start the node. Every session and delegated access issued before this restore is invalid")
	require.NotContains(t, out, dir, "no installation path is printed")
	require.NoDirExists(t, staging)

	_, err = runRecovery(t, dir, "activate", staging, "--rollback")
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, recovery.NoActivation, refusal.Reason, "D5: a finished activation is not rolled back")
}
