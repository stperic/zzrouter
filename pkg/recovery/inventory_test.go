package recovery

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

// execSQL runs query on the database at path.
func execSQL(t *testing.T, path, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), query)
	require.NoError(t, errors.Join(err, db.Close()))
}

// chmod sets path's mode until the test ends.
func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.Chmod(path, mode))
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
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
	require.Equal(t, StoreSection{State: StoreInUse, NextAction: inUseAction, Archives: []Archive{}}, section)
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
	damages[sqlstore.SchemaNewer] = func(t *testing.T, database string) { execSQL(t, database, "PRAGMA user_version = 99") }
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
			require.Equal(t, string(reason), section.Evidence, "the refusal's summary")
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

// TestInspectStoreListsArchives lists the online backup archives newest first,
// then by ID, from their manifests: a database archive this release reads with its
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
	tie := backupArchive(t, dir, garbage, now.Add(-2*time.Hour))
	ties := []Archive{unreadable, tie}
	slices.SortFunc(ties, func(a, b Archive) int { return strings.Compare(a.ID, b.ID) })
	installation := backupArchive(t, dir, writeTestBundle(t, map[string][]byte{"config/node.yaml": []byte("node: {port: 9090}")}), now.Add(-3*time.Hour))
	backups := filepath.Join(dir, "access", "backups")
	id := uuid.NewString()
	for _, leftover := range []string{id + ".tar.gz.tmp", id + ".image.tmp"} {
		require.NoError(t, os.WriteFile(filepath.Join(backups, leftover), readBytes(t, filepath.Join(backups, readable.ID+".tar.gz")), 0o600))
	}

	section, err := InspectStore(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, StoreAbsent, section.State)
	require.Equal(t, []Archive{newer, readable, ties[0], ties[1], installation}, section.Archives, "newest first, then by ID")
}

// TestInspectStoreTellsAlteredSchemaFromDamage: a live store holding schema
// objects this release does not create, an added index or the statistics
// table ANALYZE adds, is refused integrity_failed with the cause phrase.
func TestInspectStoreTellsAlteredSchemaFromDamage(t *testing.T) {
	for name, query := range map[string]string{"an added index": "CREATE INDEX planted ON teams (name)", "ANALYZE": "ANALYZE"} {
		t.Run(name, func(t *testing.T) {
			dir, _, _ := recoveryFixture(t)
			execSQL(t, filepath.Join(dir, "access", "access.sqlite"), query)
			section, err := InspectStore(t.Context(), dir)
			require.NoError(t, err)
			require.Equal(t, StoreRefused, section.State)
			require.Equal(t, sqlstore.IntegrityFailed, section.Reason)
			require.Equal(t, "integrity_failed (its schema objects differ from this release's)", section.Evidence)
			require.Equal(t, (&sqlstore.Refusal{Reason: sqlstore.IntegrityFailed}).NextAction(), section.NextAction)
		})
	}
}

// TestInspectStoreUnknown: an inspection that cannot finish is unknown, its
// error the evidence, and never another state: a lock file the account cannot
// open or cannot write is not a node holding the lock.
func TestInspectStoreUnknown(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions that bind the test's account")
	}
	for name, tc := range map[string]struct {
		breakIt  func(t *testing.T, dir string)
		evidence string
	}{
		"a lock file it cannot open":            {func(t *testing.T, dir string) { chmod(t, filepath.Join(dir, "installation.lock"), 0) }, "permission denied"},
		"a lock file it cannot write":           {func(t *testing.T, dir string) { chmod(t, filepath.Join(dir, "installation.lock"), 0o400) }, "lock the installation"},
		"a temporary directory it cannot write": {func(t *testing.T, _ string) { chmod(t, privateTemp(t), 0o500) }, "permission denied"},
		"a read-only access directory with a crash's journal": {func(t *testing.T, dir string) {
			crashMidCommit(t, filepath.Join(dir, "access", "access.sqlite"))
			chmod(t, filepath.Join(dir, "access"), 0o500)
		}, "access.sqlite"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _, _ := recoveryFixture(t)
			privateTemp(t)
			tc.breakIt(t, dir)
			section, err := InspectStore(t.Context(), dir)
			require.Error(t, err)
			require.Equal(t, StoreSection{State: StoreUnknown, Evidence: err.Error(), NextAction: unknownAction, Archives: []Archive{}}, section)
			require.Contains(t, section.Evidence, tc.evidence)
			require.NotContains(t, section.Evidence, "in use")
		})
	}
}

// TestInspectStoreNeverFollowsLinks: a linked backup directory is an error
// that lists nothing; a linked entry, a directory and a FIFO named like an
// archive are not listed, and the FIFO is never opened.
func TestInspectStoreNeverFollowsLinks(t *testing.T) {
	image, meta := capturedImage(t)
	now := utils.NowUTC().Truncate(time.Second)
	archive := databaseBundle(t, image, meta, func(m *Manifest) { m.Created = now })
	outside := filepath.Join(t.TempDir(), uuid.NewString()+".tar.gz")
	require.NoError(t, os.WriteFile(outside, readBytes(t, archive), 0o600))

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "access"), 0o700))
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "access", "backups")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	section, err := InspectStore(t.Context(), dir)
	require.ErrorContains(t, err, "must not contain links")
	require.Equal(t, StoreUnknown, section.State)
	require.Empty(t, section.Archives)

	dir = t.TempDir()
	listed := backupArchive(t, dir, archive, now)
	listed.StoreMeta, listed.Readable = &meta, true
	backups := filepath.Join(dir, "access", "backups")
	require.NoError(t, os.Symlink(outside, filepath.Join(backups, uuid.NewString()+".tar.gz")))
	require.NoError(t, os.Mkdir(filepath.Join(backups, uuid.NewString()+".tar.gz"), 0o700))
	if err := mkfifo(filepath.Join(backups, uuid.NewString()+".tar.gz")); !errors.Is(err, errors.ErrUnsupported) {
		require.NoError(t, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		section, err = InspectStore(t.Context(), dir)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("the inspection blocked on a FIFO")
	}
	require.NoError(t, err)
	require.Equal(t, []Archive{listed}, section.Archives, "regular files only")
}
