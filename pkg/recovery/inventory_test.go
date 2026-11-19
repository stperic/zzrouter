package recovery

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

// tree maps every path under dir to its content's digest, "/" for a directory.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	require.NoError(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		content := "/"
		if !entry.IsDir() {
			content = digest(readBytes(t, path))
		}
		files[path] = content
		return nil
	}))
	return files
}

// backupArchive places the archive at path among dir's online backups under
// a new ID, modified at modified, and returns its entry as listed when readable.
func backupArchive(t *testing.T, dir, path string, modified time.Time) Archive {
	t.Helper()
	backups := filepath.Join(dir, "access", "backups")
	require.NoError(t, os.MkdirAll(backups, 0o700))
	id := uuid.NewString()
	placed := filepath.Join(backups, id+".tar.gz")
	require.NoError(t, os.WriteFile(placed, readBytes(t, path), 0o600))
	require.NoError(t, os.Chtimes(placed, modified, modified))
	return Archive{ID: id, CreatedAt: modified, SizeBytes: int64(len(readBytes(t, placed)))}
}

// TestInspectStoreInUseTouchesNothing: while a node holds the installation
// lock, the section is in_use and points at the running node's status, and
// neither the database nor its hot journal changes, nothing is copied and no
// archive is opened. Once the node stops, the same installation reports the
// committed state, and recovering the journal is the only change.
func TestInspectStoreInUseTouchesNothing(t *testing.T) {
	dir, _, _ := recoveryFixture(t)
	meta := issue(t, dir).meta
	image, imageMeta := capturedImage(t)
	backupArchive(t, dir, databaseBundle(t, image, imageMeta), utils.NowUTC())
	database := filepath.Join(dir, "access", "access.sqlite")
	committed := crashMidCommit(t, database)
	temp := privateTemp(t)
	before := tree(t, dir)
	lock, err := config.LockInstallation(dir)
	require.NoError(t, err)
	section, err := InspectStore(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, StoreSection{State: StoreInUse, NextAction: "A node is running: read its access store's state from GET /zzrouter/v1/system/state.", Archives: []Archive{}}, section)
	require.Equal(t, before, tree(t, dir), "the database and its journal are untouched")
	emptyDir(t, temp, "nothing is copied")
	require.NoError(t, lock.Close())

	section, err = InspectStore(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, StoreReady, section.State)
	require.Equal(t, StoreMeta(meta), *section.StoreMeta, "the committed state, not the crash's")
	require.Len(t, section.Archives, 1)
	require.NoFileExists(t, database+"-journal")
	require.Equal(t, committed, readBytes(t, database), "recovering the journal is the only change")
	emptyDir(t, temp, "the copy is removed")
}

// TestInspectStoreReportsEachRefusal: each reason a start refuses for is
// refused with the store's own next action, the store's identity only when it
// can be read, and no copy left behind.
func TestInspectStoreReportsEachRefusal(t *testing.T) {
	damages := storeDamages()
	damages[sqlstore.SchemaNewer] = func(t *testing.T, database string) {
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(database))
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), "PRAGMA user_version = 99")
		require.NoError(t, errors.Join(err, db.Close()))
	}
	for reason, damage := range damages {
		t.Run(string(reason), func(t *testing.T) {
			dir, _, _ := recoveryFixture(t)
			meta := issue(t, dir).meta
			temp := privateTemp(t)
			damage(t, filepath.Join(dir, "access", "access.sqlite"))
			section, err := InspectStore(t.Context(), dir)
			require.NoError(t, err)
			require.Equal(t, StoreRefused, section.State)
			require.Equal(t, reason, section.Reason)
			require.Equal(t, (&sqlstore.Refusal{Reason: reason}).NextAction(), section.NextAction)
			require.NotEmpty(t, section.NextAction)
			if reason == sqlstore.SchemaNewer {
				meta.SchemaVersion = 99
				require.Equal(t, StoreMeta(meta), *section.StoreMeta, "a newer release's store keeps the layout of its identity")
			} else {
				require.Nil(t, section.StoreMeta)
			}
			emptyDir(t, temp, "the copy is removed")
		})
	}
}

// TestInspectStoreStates: a store without an owner is setup_required with its
// identity, and inspecting it creates no lock file where none existed; a
// worker's installation is absent, and nothing is created in it.
func TestInspectStoreStates(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	meta := StoreMeta(store.Meta())
	require.NoError(t, store.Close())
	require.NoFileExists(t, filepath.Join(dir, "installation.lock"))
	section, err := InspectStore(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, StoreSection{State: StoreSetupRequired, StoreMeta: &meta, Archives: []Archive{}}, section)
	require.NoFileExists(t, filepath.Join(dir, "installation.lock"), "no lock file owned by the invoking account")

	worker := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(worker, "node.yaml"), []byte("node: {port: 9090}"), 0o600))
	lock, err := config.LockInstallation(worker)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	before := tree(t, worker)
	section, err = InspectStore(t.Context(), worker)
	require.NoError(t, err)
	require.Equal(t, StoreSection{State: StoreAbsent, Archives: []Archive{}}, section)
	require.Equal(t, before, tree(t, worker), "nothing is created on a worker")
}

// TestInspectStoreListsArchives lists the online backup archives newest first
// from their manifests: a database archive this release reads with its
// identity and creation time; one a newer release wrote with its identity but
// unreadable; a garbage file and an installation bundle unreadable, aged by
// their modification times. Temporary files are not listed.
func TestInspectStoreListsArchives(t *testing.T) {
	dir := t.TempDir()
	image, meta := capturedImage(t)
	now := utils.NowUTC().Truncate(time.Second)
	readable := backupArchive(t, dir, databaseBundle(t, image, meta, func(m *Manifest) { m.Created = now.Add(-time.Minute) }), now)
	readable.CreatedAt, readable.StoreMeta, readable.Readable = now.Add(-time.Minute), &meta, true
	newerMeta := meta
	newerMeta.SchemaVersion = 99
	newer := backupArchive(t, dir, databaseBundle(t, image, newerMeta, func(m *Manifest) { m.Created = now }), now.Add(-time.Hour))
	newer.CreatedAt, newer.StoreMeta = now, &newerMeta
	garbage := filepath.Join(t.TempDir(), "garbage.tar.gz")
	require.NoError(t, os.WriteFile(garbage, []byte("not an archive"), 0o600))
	unreadable := backupArchive(t, dir, garbage, now.Add(-2*time.Hour))
	installation := backupArchive(t, dir, writeTestBundle(t, map[string][]byte{"config/node.yaml": []byte("node: {port: 9090}")}), now.Add(-3*time.Hour))
	backups := filepath.Join(dir, "access", "backups")
	id := uuid.NewString()
	for _, leftover := range []string{id + ".tar.gz.tmp", id + ".image.tmp"} {
		require.NoError(t, os.WriteFile(filepath.Join(backups, leftover), readBytes(t, filepath.Join(backups, readable.ID+".tar.gz")), 0o600))
	}

	section, err := InspectStore(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, StoreAbsent, section.State)
	require.Equal(t, []Archive{newer, readable, unreadable, installation}, section.Archives)
}
