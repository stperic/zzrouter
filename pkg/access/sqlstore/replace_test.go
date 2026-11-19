package sqlstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
)

// fixture is a closed store in a directory of its own, and an image of it
// captured before its last change and prepared as a restore prepares it: what
// Replace publishes over it, with its digest.
type fixture struct {
	dir, image, sha string
	meta            Meta
}

func installStore(t *testing.T) fixture {
	t.Helper()
	i := fixture{dir: t.TempDir(), image: filepath.Join(t.TempDir(), "image.sqlite")}
	s, err := Open(t.Context(), i.dir, Options{})
	require.NoError(t, err)
	_, err = s.Capture(t.Context(), i.image)
	require.NoError(t, err)
	_, err = s.CreateTeam(t.Context(), "after-capture", engineering(), audit.Event{})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	prepared, err := PrepareImage(t.Context(), i.image)
	require.NoError(t, err)
	i.meta, i.sha = prepared.Meta, digestOf(t, i.image)
	return i
}

func digestOf(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256(readFile(t, path))
	return hex.EncodeToString(sum[:])
}

// storeFiles reads dir's database, journal and marker, nil for an absent one.
func storeFiles(t *testing.T, dir string) [3][]byte {
	t.Helper()
	var files [3][]byte
	for i, path := range []string{dbPath(dir), dbPath(dir) + "-journal", filepath.Join(dir, MarkerName)} {
		data, err := os.ReadFile(path)
		if !errors.Is(err, fs.ErrNotExist) {
			require.NoError(t, err)
			files[i] = data
		}
	}
	return files
}

func keepIn(t *testing.T) string { return filepath.Join(t.TempDir(), "rollback", "store") }

// TestReplaceAndRevert: Replace publishes the image byte for byte and keeps
// the database and marker it replaced; a second call changes nothing; Revert
// restores both byte for byte, and a second Revert changes nothing.
func TestReplaceAndRevert(t *testing.T) {
	t.Parallel()
	i := installStore(t)
	before := storeFiles(t, i.dir)
	keep := keepIn(t)
	for range 2 {
		img, err := Replace(t.Context(), i.dir, i.image, i.sha, keep, false)
		require.NoError(t, err)
		require.Equal(t, i.meta, img.Meta)
		require.Equal(t, readFile(t, i.image), readFile(t, dbPath(i.dir)), "the image is published byte for byte")
		require.Equal(t, before[0], readFile(t, filepath.Join(keep, fileName)))
		require.Equal(t, before[2], readFile(t, filepath.Join(keep, MarkerName)))
		require.NoFileExists(t, dbPath(i.dir)+".staged")
	}
	s := openStore(t, i.dir, Options{})
	require.Nil(t, s.Snapshot().Team("after-capture"), "the store holds the image's records")
	require.NoError(t, s.Close())
	require.Equal(t, before[2], readFile(t, filepath.Join(i.dir, MarkerName)), "an open of the published image keeps the marker")
	for range 2 {
		require.NoError(t, Revert(t.Context(), i.dir, keep, i.meta.Generation))
		require.Equal(t, before, storeFiles(t, i.dir), "the database and marker are as they were")
	}
}

// TestReplaceSettlesAHotJournal: a journal a crash left beside a readable
// database is settled before the database is kept, and never lies beside the
// new one; Revert restores the committed database without it.
func TestReplaceSettlesAHotJournal(t *testing.T) {
	t.Parallel()
	i := installStore(t)
	committed := readFile(t, dbPath(i.dir))
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath(i.dir))+"?_pragma=synchronous(OFF)&_pragma=cache_size(10)")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		INSERT INTO audit_events (seq, time, action, actor, subject, request_id) SELECT 100000 + i, 0, 'x', 'a', hex(randomblob(512)), '' FROM n`)
	require.NoError(t, err)
	inFlight, journal := readFile(t, dbPath(i.dir)), readFile(t, dbPath(i.dir)+"-journal")
	require.NoError(t, errors.Join(tx.Rollback(), db.Close()))
	require.NoError(t, os.WriteFile(dbPath(i.dir), inFlight, 0o600))
	require.NoError(t, os.WriteFile(dbPath(i.dir)+"-journal", journal, 0o600))
	require.NotEqual(t, committed, inFlight, "the crash left uncommitted pages in the file")

	keep := keepIn(t)
	_, err = Replace(t.Context(), i.dir, i.image, i.sha, keep, false)
	require.NoError(t, err)
	require.NoFileExists(t, dbPath(i.dir)+"-journal", "no journal beside the new database")
	require.Equal(t, readFile(t, i.image), readFile(t, dbPath(i.dir)))
	require.Equal(t, committed, readFile(t, filepath.Join(keep, fileName)), "the kept database is the settled one")
	require.NoFileExists(t, filepath.Join(keep, fileName+"-journal"))
	require.NoError(t, Revert(t.Context(), i.dir, keep, i.meta.Generation))
	require.Equal(t, committed, readFile(t, dbPath(i.dir)))
	require.NoFileExists(t, dbPath(i.dir)+"-journal")
}

// TestReplaceKeepsAnUnreadableDatabase: an unreadable database is replaced,
// and it and the journal SQLite left beside it are kept, the journal gone from
// beside the new database, and both restored byte for byte. A hot journal
// takes another path even beside an unreadable database: SQLite plays it into
// the file, or deletes it, while it probes, before anything is copied. The
// journal left is one it does not read, whose first byte is zero.
func TestReplaceKeepsAnUnreadableDatabase(t *testing.T) {
	t.Parallel()
	i := installStore(t)
	require.NoError(t, os.WriteFile(dbPath(i.dir), []byte("not a database, just text long enough to have a header"), 0o600))
	require.NoError(t, os.WriteFile(dbPath(i.dir)+"-journal", []byte("\x00a journal SQLite does not read"), 0o600))
	before := storeFiles(t, i.dir)
	keep := keepIn(t)
	_, err := Replace(t.Context(), i.dir, i.image, i.sha, keep, false)
	require.NoError(t, err)
	require.Equal(t, readFile(t, i.image), readFile(t, dbPath(i.dir)))
	require.NoFileExists(t, dbPath(i.dir)+"-journal")
	require.Equal(t, before[0], readFile(t, filepath.Join(keep, fileName)))
	require.Equal(t, before[1], readFile(t, filepath.Join(keep, fileName+"-journal")))
	require.NoError(t, Revert(t.Context(), i.dir, keep, i.meta.Generation))
	require.Equal(t, before, storeFiles(t, i.dir))
}

// TestReplaceAdmitsOnlyTheInstallationsStore: a store marker or a database
// admits only its own store ID, refused with nothing changed, and a fresh
// installation admits any; foreign admits another store over one, and Revert
// undoes either, removing what did not exist.
func TestReplaceAdmitsOnlyTheInstallationsStore(t *testing.T) {
	t.Parallel()
	other := installStore(t)
	for name, prepare := range map[string]func(dir string){
		"a marker and a database": func(string) {},
		"a marker alone":          func(dir string) { require.NoError(t, os.Remove(dbPath(dir))) },
		"a database alone":        func(dir string) { require.NoError(t, os.Remove(filepath.Join(dir, MarkerName))) },
		"an unreadable database without a marker": func(dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, MarkerName)))
			require.NoError(t, os.WriteFile(dbPath(dir), []byte("not a database, just text long enough to have a header"), 0o600))
		},
	} {
		t.Run(name, func(t *testing.T) {
			i := installStore(t)
			prepare(i.dir)
			before := storeFiles(t, i.dir)
			keep := keepIn(t)
			_, err := AdmitStoreID(t.Context(), i.dir, other.meta.StoreID, false)
			require.ErrorIs(t, err, ErrStoreIdentity)
			_, err = Replace(t.Context(), i.dir, other.image, other.sha, keep, false)
			require.ErrorIs(t, err, ErrStoreIdentity)
			require.Equal(t, before, storeFiles(t, i.dir))
			require.NoFileExists(t, dbPath(i.dir)+".staged")
			require.NoDirExists(t, keep, "nothing was kept")
		})
	}
	t.Run("a fresh installation", func(t *testing.T) {
		dir, keep := t.TempDir(), keepIn(t)
		inst, err := AdmitStoreID(t.Context(), dir, other.meta.StoreID, false)
		require.NoError(t, err)
		require.Equal(t, Installed{}, inst)
		for range 2 { // the second is a rerun after a crash at the end: nothing existed still
			_, err = Replace(t.Context(), dir, other.image, other.sha, keep, false)
			require.NoError(t, err)
		}
		id, err := MarkerStoreID(readFile(t, filepath.Join(dir, MarkerName)))
		require.NoError(t, err)
		require.Equal(t, other.meta.StoreID, id)
		require.NoError(t, Revert(t.Context(), dir, keep, other.meta.Generation))
		require.Equal(t, [3][]byte{}, storeFiles(t, dir), "neither a database nor a marker")
	})
	t.Run("foreign", func(t *testing.T) {
		i := installStore(t)
		before := storeFiles(t, i.dir)
		keep := keepIn(t)
		_, err := Replace(t.Context(), i.dir, other.image, other.sha, keep, true)
		require.NoError(t, err)
		id, err := MarkerStoreID(readFile(t, filepath.Join(i.dir, MarkerName)))
		require.NoError(t, err)
		require.Equal(t, other.meta.StoreID, id)
		require.NoError(t, Revert(t.Context(), i.dir, keep, other.meta.Generation))
		require.Equal(t, before, storeFiles(t, i.dir))
	})
}

// TestRevertBeforeReplaceRecorded: before Replace records anything, the
// installation is unchanged; Revert removes only the staged copy.
func TestRevertBeforeReplaceRecorded(t *testing.T) {
	t.Parallel()
	i := installStore(t)
	before := storeFiles(t, i.dir)
	require.NoError(t, os.WriteFile(dbPath(i.dir)+".staged", readFile(t, i.image), 0o600))
	require.NoError(t, Revert(t.Context(), i.dir, keepIn(t), i.meta.Generation))
	require.Equal(t, before, storeFiles(t, i.dir))
	require.NoFileExists(t, dbPath(i.dir)+".staged")
}

// TestReplaceRefusesAnImageNotTheOneChecked: a copy whose digest is not the
// one the caller checked is refused before anything of the installation
// changes.
func TestReplaceRefusesAnImageNotTheOneChecked(t *testing.T) {
	t.Parallel()
	i := installStore(t)
	before := storeFiles(t, i.dir)
	keep := keepIn(t)
	_, err := Replace(t.Context(), i.dir, i.image, digestOf(t, dbPath(i.dir)), keep, false)
	require.ErrorContains(t, err, "the image is not the one checked")
	require.Equal(t, before, storeFiles(t, i.dir))
	require.NoFileExists(t, dbPath(i.dir)+".staged")
	require.NoDirExists(t, keep, "nothing was kept")
}

// TestRevertRefusesWhenWhatWasKeptIsLost: once the database is the published
// image, a keep that lost its record or a file it recorded refuses the revert
// with nothing changed.
func TestRevertRefusesWhenWhatWasKeptIsLost(t *testing.T) {
	t.Parallel()
	for name, lose := range map[string]func(keep string) error{
		"the whole keep":    os.RemoveAll,
		"the kept database": func(keep string) error { return os.Remove(filepath.Join(keep, fileName)) },
	} {
		t.Run(name, func(t *testing.T) {
			i := installStore(t)
			keep := keepIn(t)
			_, err := Replace(t.Context(), i.dir, i.image, i.sha, keep, false)
			require.NoError(t, err)
			require.NoError(t, lose(keep))
			published := storeFiles(t, i.dir)
			require.ErrorIs(t, Revert(t.Context(), i.dir, keep, i.meta.Generation), ErrKeptLost)
			require.Equal(t, published, storeFiles(t, i.dir), "nothing changed")
		})
	}
}

// TestReplaceGivesTheInstallationsOwner: Replace gives every file and
// directory it wrote the owner of the installation directory, Revert every
// file it restored, each in one step the observer sees.
func TestReplaceGivesTheInstallationsOwner(t *testing.T) {
	t.Parallel()
	i := installStore(t)
	keep := filepath.Join(i.dir, "rollback", "g", "store")
	var steps []string
	ctx := sqlitedb.WithStep(t.Context(), func(step string) {
		if strings.HasPrefix(step, "owned ") {
			steps = append(steps, step)
		}
	})
	_, err := Replace(ctx, i.dir, i.image, i.sha, keep, false)
	require.NoError(t, err)
	require.NoError(t, Revert(ctx, i.dir, keep, i.meta.Generation))
	require.Equal(t, []string{
		"owned access, access/access.sqlite, access-store.json, rollback/g/store, rollback/g/store/kept.json, rollback/g/store/access.sqlite, rollback/g/store/access-store.json",
		"owned access/access.sqlite, access-store.json",
	}, steps)
}
