package sqlitedb_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestCopyToPurgesDeletedContent(t *testing.T) {
	s := testSchema(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.sqlite")
	db := createDB(t, path, s)
	execSQL(t, db.SQL(), "PRAGMA secure_delete=OFF")
	secret := "DELETED_SECRET_" + strings.Repeat("purge-this-value-", 4096)
	execSQL(t, db.SQL(), "INSERT INTO parents VALUES(1, ?), (2, 'retained')", secret)
	execSQL(t, db.SQL(), "INSERT INTO children VALUES(1, 2)")
	execSQL(t, db.SQL(), "DELETE FROM parents WHERE id=1")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	marker := []byte(strings.Repeat("purge-this-value-", 16))
	require.True(t, bytes.Contains(data, marker), "source must still contain deleted bytes for this test to prove purging")
	dst := filepath.Join(dir, "absent"+uriSpecial()+".sqlite")
	require.NoError(t, db.CopyTo(t.Context(), dst))
	ownerOnly(t, dst)
	copy := openDB(t, dst, s)
	require.NoError(t, copy.Check(t.Context(), true))
	require.EqualValues(t, 1, integer(t, copy.SQL(), "SELECT count(*) FROM parents"))
	require.EqualValues(t, 1, integer(t, copy.SQL(), "SELECT count(*) FROM parents WHERE id=2 AND value='retained'"))
	require.EqualValues(t, 1, integer(t, copy.SQL(), "SELECT count(*) FROM children WHERE parent_id=2"))
	data, err = os.ReadFile(dst)
	require.NoError(t, err)
	require.False(t, bytes.Contains(data, marker))
	// An existing destination, even an empty one, is refused and left as it was.
	for name, content := range map[string]string{"empty": "", "occupied": "preserve"} {
		dst := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(dst, []byte(content), 0o644))
		require.ErrorIs(t, db.CopyTo(t.Context(), dst), os.ErrExist, name)
		data, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, content, string(data), name)
	}
}

// TestCopyFile copies a file byte for byte, owner-only, into an absent
// destination, and leaves no destination when it fails.
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.sqlite")
	db := createDB(t, path, testSchema(t))
	execSQL(t, db.SQL(), "INSERT INTO parents VALUES(1, 'kept')")
	require.NoError(t, db.Close())
	dst := filepath.Join(dir, "copy.sqlite")
	sum, err := sqlitedb.CopyFile(t.Context(), path, dst)
	require.NoError(t, err)
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	digest := sha256.Sum256(source)
	require.Equal(t, hex.EncodeToString(digest[:]), sum, "the digest of the bytes written")
	copied, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, source, copied)
	ownerOnly(t, dst)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "occupied"), []byte("preserve"), 0o600))
	require.ErrorIs(t, second(sqlitedb.CopyFile(t.Context(), path, filepath.Join(dir, "occupied"))), os.ErrExist)
	data, err := os.ReadFile(filepath.Join(dir, "occupied"))
	require.NoError(t, err)
	require.Equal(t, "preserve", string(data))
	failed := filepath.Join(dir, "failed.sqlite")
	require.ErrorIs(t, second(sqlitedb.CopyFile(t.Context(), filepath.Join(dir, "absent.sqlite"), failed)), os.ErrNotExist)
	require.NoFileExists(t, failed)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, second(sqlitedb.CopyFile(cancelled, path, failed)), context.Canceled)
	require.NoFileExists(t, failed, "a cancelled copy leaves no file")

	// Cancelled after its first read of a file larger than one.
	large := filepath.Join(dir, "large.sqlite")
	require.NoError(t, os.WriteFile(large, bytes.Repeat([]byte("page"), 1<<18), 0o600))
	midway := &cancelOnSecondCheck{}
	midway.Context, midway.cancel = context.WithCancel(t.Context())
	defer midway.cancel()
	require.ErrorIs(t, second(sqlitedb.CopyFile(midway, large, failed)), context.Canceled)
	require.Equal(t, int32(2), midway.checks.Load(), "it read once before the cancellation")
	require.NoFileExists(t, failed, "a copy cancelled midway leaves no file")
}

// cancelOnSecondCheck cancels its context when Err is checked the second time.
type cancelOnSecondCheck struct {
	context.Context
	cancel context.CancelFunc
	checks atomic.Int32
}

func (c *cancelOnSecondCheck) Err() error {
	if c.checks.Add(1) == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCheckCorruptPage(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := map[bool]string{false: "quick_check", true: "integrity_check"}[full]
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "corrupt.sqlite")
			s := testSchema(t)
			db := createDB(t, path, s)
			execSQL(t, db.SQL(), "INSERT INTO parents VALUES(1, 'intact')")
			require.NoError(t, db.Check(t.Context(), full))
			page := integer(t, db.SQL(), "SELECT rootpage FROM sqlite_schema WHERE name='parents'")
			size := integer(t, db.SQL(), "PRAGMA page_size")
			require.NoError(t, db.Close())
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(t, err)
			_, err = f.WriteAt([]byte{0xff}, (page-1)*size)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			db = openDB(t, path, s)
			var failure *sqlitedb.CheckError
			require.ErrorAs(t, db.Check(t.Context(), full), &failure)
			require.Equal(t, name, failure.Check)
			require.NotEmpty(t, failure.Detail)
			require.NotContains(t, failure.Detail, "<nil>")
		})
	}
}

func TestCheckCorruptQueryError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.sqlite")
	db := createDB(t, path, testSchema(t))
	// Drop the cached connection so the next query reads the damaged file.
	db.SQL().SetMaxIdleConns(0)
	require.NoError(t, os.Truncate(path, 256))
	for _, full := range []bool{false, true} {
		name := map[bool]string{false: "quick_check", true: "integrity_check"}[full]
		t.Run(name, func(t *testing.T) {
			// Prove this fixture fails the query, rather than returning a check result row.
			rows, err := db.SQL().QueryContext(t.Context(), "PRAGMA "+name)
			if rows != nil {
				require.NoError(t, rows.Close())
			}
			var corrupt *sqlite.Error
			require.ErrorAs(t, err, &corrupt)
			require.Equal(t, sqlite3.SQLITE_CORRUPT, corrupt.Code()&0xff)
			err = db.Check(t.Context(), full)
			var failure *sqlitedb.CheckError
			require.ErrorAs(t, err, &failure)
			require.IsType(t, &sqlitedb.CheckError{}, err)
			require.Equal(t, name, failure.Check)
			require.NotEmpty(t, failure.Detail)
		})
	}
}

func TestCheckForeignKeyViolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign-key.sqlite")
	s := testSchema(t)
	db := createDB(t, path, s)
	require.NoError(t, db.Close())
	raw := rawDB(t, path)
	execSQL(t, raw, "PRAGMA foreign_keys=OFF; INSERT INTO children VALUES(1,999)")
	require.NoError(t, raw.Close())
	db = openDB(t, path, s)
	for _, full := range []bool{false, true} {
		var failure *sqlitedb.CheckError
		require.ErrorAs(t, db.Check(t.Context(), full), &failure)
		require.Equal(t, "foreign_key_check", failure.Check)
		require.NotEmpty(t, failure.Detail)
		require.Contains(t, failure.Detail, "table children rowid 1")
		require.NotContains(t, failure.Detail, "<nil>")
	}
}

func TestCheckCancelledContext(t *testing.T) {
	db := createDB(t, filepath.Join(t.TempDir(), "cancelled.sqlite"), testSchema(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, full := range []bool{false, true} {
		err := db.Check(ctx, full)
		require.ErrorIs(t, err, context.Canceled)
		var failure *sqlitedb.CheckError
		require.False(t, errors.As(err, &failure))
	}
}

func TestCheckUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unreadable.sqlite")
	db := createDB(t, path, testSchema(t))
	db.SQL().SetMaxIdleConns(0)
	require.NoError(t, os.WriteFile(path, []byte("no longer a SQLite database"), 0o600))
	for _, full := range []bool{false, true} {
		var failure *sqlitedb.CheckError
		require.ErrorAs(t, db.Check(t.Context(), full), &failure)
		require.NotEmpty(t, failure.Detail)
		require.NotContains(t, failure.Detail, "<nil>")
	}
}

func TestCheckForeignKeyWithoutRowid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "without-rowid.sqlite")
	s := scriptsSchema(`CREATE TABLE parents(id PRIMARY KEY);
		CREATE TABLE children(id PRIMARY KEY, parent_id REFERENCES parents(id)) WITHOUT ROWID;`)
	db := createDB(t, path, s)
	require.NoError(t, db.Close())
	raw := rawDB(t, path)
	execSQL(t, raw, "PRAGMA foreign_keys=OFF; INSERT INTO children VALUES('child',999)")
	require.NoError(t, raw.Close())
	db = openDB(t, path, s)
	var failure *sqlitedb.CheckError
	require.ErrorAs(t, db.Check(t.Context(), true), &failure)
	require.Contains(t, failure.Detail, "table children rowid NULL")
	require.NotContains(t, failure.Detail, "<nil>")
}

func TestCopyToFailureCleansCreatedDestination(t *testing.T) {
	db := createDB(t, filepath.Join(t.TempDir(), "source.sqlite"), testSchema(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dst := filepath.Join(t.TempDir(), "copy.sqlite")
	require.ErrorIs(t, db.CopyTo(ctx, dst), context.Canceled)
	require.NoFileExists(t, dst)
}

// TestCopyToCorruptPage reports a corrupt page the copy reads as a
// *CheckError, as Check does, and leaves no destination.
func TestCopyToCorruptPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.sqlite")
	s := testSchema(t)
	db := createDB(t, path, s)
	execSQL(t, db.SQL(), "INSERT INTO parents VALUES(1, 'intact')")
	page := integer(t, db.SQL(), "SELECT rootpage FROM sqlite_schema WHERE name='parents'")
	size := integer(t, db.SQL(), "PRAGMA page_size")
	require.NoError(t, db.Close())
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0xff}, (page-1)*size)
	require.NoError(t, errors.Join(err, f.Close()))
	dst := filepath.Join(t.TempDir(), "copy.sqlite")
	err = openDB(t, path, s).CopyTo(t.Context(), dst)
	var failure *sqlitedb.CheckError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "copy", failure.Check)
	require.NotEmpty(t, failure.Detail)
	require.NoFileExists(t, dst)
}

func TestFreeBytes(t *testing.T) {
	db := createDB(t, filepath.Join(t.TempDir(), "freelist.sqlite"), testSchema(t))
	free, err := sqlitedb.FreeBytes(t.Context(), db.SQL())
	require.NoError(t, err)
	require.Zero(t, free)
	tx, err := db.SQL().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	for i := range 256 {
		_, err := tx.ExecContext(t.Context(), "INSERT INTO parents VALUES(?, ?)", i, strings.Repeat("x", 4096))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	before, err := sqlitedb.FreeBytes(t.Context(), db.SQL())
	require.NoError(t, err)
	execSQL(t, db.SQL(), "DELETE FROM parents")
	after, err := sqlitedb.FreeBytes(t.Context(), db.SQL())
	require.NoError(t, err)
	require.Greater(t, after, before)
	require.Equal(t, integer(t, db.SQL(), "PRAGMA freelist_count")*integer(t, db.SQL(), "PRAGMA page_size"), after)
}

func TestRenameMovesAClosedFile(t *testing.T) {
	s := testSchema(t)
	dir := t.TempDir()
	staged, final := filepath.Join(dir, "store.sqlite.staged"), filepath.Join(dir, "store.sqlite")
	db, err := sqlitedb.Create(t.Context(), staged, s, nil)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, sqlitedb.Rename(staged, final))
	_, err = os.Stat(staged)
	require.ErrorIs(t, err, os.ErrNotExist)
	ownerOnly(t, final)
	h, err := sqlitedb.Probe(t.Context(), final)
	require.NoError(t, err)
	require.Equal(t, sqlitedb.Header{AppID: s.AppID, Version: 3}, h)
	require.ErrorIs(t, sqlitedb.Rename(staged, final), os.ErrNotExist, "a missing source moves nothing")
}

func second[T any](_ T, err error) error { return err }

// TestMakeDirSyncsEveryLevelItCreates: each missing level is created, made
// owner-only and its parent synced, top down, before the next; an existing
// directory reports nothing, and a file on the way is refused. The step comes
// right after the level's parent was synced: this pins the order of the calls,
// not what reaches the disk, which no test without a power cut can see.
func TestMakeDirSyncsEveryLevelItCreates(t *testing.T) {
	root := t.TempDir()
	var steps []string
	ctx := sqlitedb.WithStep(t.Context(), func(step string) { steps = append(steps, step) })
	leaf := filepath.Join(root, "a", "b", "c")
	require.NoError(t, sqlitedb.MakeDir(ctx, leaf))
	require.Equal(t, []string{"made a", "made b", "made c"}, steps)
	for _, level := range []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), leaf} {
		if runtime.GOOS != "windows" {
			info, err := os.Stat(level)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), level)
		}
	}
	steps = nil
	require.NoError(t, sqlitedb.MakeDir(ctx, leaf))
	require.Empty(t, steps, "nothing created, nothing reported")
	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), nil, 0o600))
	require.Error(t, sqlitedb.MakeDir(ctx, filepath.Join(root, "file", "x")))
	require.ErrorContains(t, sqlitedb.MakeDir(ctx, filepath.Join(root, "file")), "is not a directory")
	require.Empty(t, steps)
}
