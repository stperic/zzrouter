package sqlitedb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), OmitHost: true}
	db, err := sql.Open("sqlite", u.String())
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func createDB(t *testing.T, path string, s sqlitedb.Schema) *sqlitedb.DB {
	t.Helper()
	db, err := sqlitedb.Create(t.Context(), path, s, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func openDB(t *testing.T, path string, s sqlitedb.Schema) *sqlitedb.DB {
	t.Helper()
	db, err := sqlitedb.Open(t.Context(), path, s)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func execSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), query, args...)
	require.NoError(t, err)
}

func integer(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.QueryRowContext(t.Context(), query).Scan(&n))
	return n
}

func ownerOnly(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), path)
}

func TestConnectionSettingsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data ?#%' file.sqlite")
	s := testSchema(t)
	db := createDB(t, path, s)
	require.NoError(t, db.Close())
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod(path, 0o666))
	}
	db = openDB(t, path, s)
	ownerOnly(t, path)
	require.Equal(t, 1, db.SQL().Stats().MaxOpenConnections)
	check := func() {
		require.EqualValues(t, 1, integer(t, db.SQL(), "PRAGMA foreign_keys"))
		require.EqualValues(t, 3, integer(t, db.SQL(), "PRAGMA synchronous"))
		require.EqualValues(t, 5000, integer(t, db.SQL(), "PRAGMA busy_timeout"))
		var mode string
		require.NoError(t, db.SQL().QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode))
		require.Equal(t, "delete", mode)
		if runtime.GOOS == "darwin" {
			require.EqualValues(t, 1, integer(t, db.SQL(), "PRAGMA fullfsync"))
		}
	}
	check()
	require.Equal(t, 1, db.SQL().Stats().Idle)
	closed := db.SQL().Stats().MaxIdleClosed
	db.SQL().SetMaxIdleConns(0)
	check()
	require.Greater(t, db.SQL().Stats().MaxIdleClosed, closed)
	db.SQL().SetMaxIdleConns(1)
	tx, err := db.SQL().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	other := rawDB(t, path)
	execSQL(t, other, "PRAGMA busy_timeout=1")
	_, err = other.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	require.Error(t, err, "an immediate transaction must already hold the writer lock")
	_, err = tx.ExecContext(t.Context(), "INSERT INTO parents VALUES(1, 'owner only')")
	require.NoError(t, err)
	ownerOnly(t, path+"-journal")
	require.NoError(t, tx.Rollback())
	_, err = db.SQL().ExecContext(t.Context(), "INSERT INTO children VALUES(1, 999)")
	require.Error(t, err)
}

func TestOpenRefusals(t *testing.T) {
	s := testSchema(t)
	path := filepath.Join(t.TempDir(), "absent.sqlite")
	db, err := sqlitedb.Open(t.Context(), path, s)
	require.Nil(t, db)
	require.ErrorIs(t, err, sqlitedb.ErrNotExist)
	require.Equal(t, 1, strings.Count(err.Error(), path))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = sqlitedb.Probe(t.Context(), path)
	require.ErrorIs(t, err, sqlitedb.ErrNotExist)
	require.Equal(t, 1, strings.Count(err.Error(), path))
	for _, name := range []string{"foreign", "newer"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.sqlite")
			db := createDB(t, path, s)
			sentinel := sqlitedb.ErrForeign
			if name == "foreign" {
				execSQL(t, db.SQL(), "PRAGMA application_id=42")
			} else {
				execSQL(t, db.SQL(), "PRAGMA user_version=999")
				sentinel = sqlitedb.ErrNewer
			}
			require.NoError(t, db.Close())
			opened, err := sqlitedb.Open(t.Context(), path, s)
			require.Nil(t, opened)
			require.ErrorIs(t, err, sentinel)
		})
	}
}

func TestProbeUnparseableNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.sqlite")
	s := testSchema(t)
	db := createDB(t, path, s)
	require.NoError(t, db.Close())
	raw := rawDB(t, path)
	execSQL(t, raw, `PRAGMA writable_schema=ON;
		INSERT INTO sqlite_schema(type,name,tbl_name,rootpage,sql)
		VALUES('table','future','future',0,'CREATE TABLE future(x FUTURE SYNTAX !!!)');
		PRAGMA user_version=999; PRAGMA writable_schema=OFF;`)
	require.NoError(t, raw.Close())
	bad := rawDB(t, path)
	_, err := bad.QueryContext(t.Context(), "SELECT * FROM parents")
	require.Error(t, err)
	require.NoError(t, bad.Close())
	h, err := sqlitedb.Probe(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, sqlitedb.Header{AppID: s.AppID, Version: 999}, h)
	opened, err := sqlitedb.Open(t.Context(), path, s)
	require.Nil(t, opened)
	require.ErrorIs(t, err, sqlitedb.ErrNewer)
}

// TestOpenAny opens a file Open refuses for its version, for copying, returns
// its header for the caller to judge and leaves its permissions alone.
func TestOpenAny(t *testing.T) {
	s := testSchema(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "newer.sqlite")
	db := createDB(t, path, s)
	execSQL(t, db.SQL(), "INSERT INTO parents VALUES(1, 'kept'); PRAGMA user_version=999")
	require.NoError(t, db.Close())
	require.NoError(t, os.Chmod(path, 0o644))
	_, err := sqlitedb.Open(t.Context(), path, s)
	require.ErrorIs(t, err, sqlitedb.ErrNewer)

	opened, h, err := sqlitedb.OpenAny(t.Context(), path, s)
	require.NoError(t, err)
	require.Equal(t, sqlitedb.Header{AppID: s.AppID, Version: 999}, h)
	_, _, err = opened.Migrate(t.Context())
	require.ErrorIs(t, err, sqlitedb.ErrNewer, "a file opened whatever its version is never migrated")
	_, err = opened.SQL().ExecContext(t.Context(), "INSERT INTO parents VALUES(2, 'written')")
	var readOnly *sqlite.Error
	require.ErrorAs(t, err, &readOnly, "nothing is written through it")
	require.Equal(t, sqlite3.SQLITE_READONLY, readOnly.Code()&0xff, "%v", err)
	dst := filepath.Join(dir, "copy.sqlite")
	require.NoError(t, opened.CopyTo(t.Context(), dst))
	require.NoError(t, opened.Close())
	copied, err := sqlitedb.Probe(t.Context(), dst)
	require.NoError(t, err)
	require.Equal(t, h, copied)
	require.EqualValues(t, 1, integer(t, rawDB(t, dst), "SELECT count(*) FROM parents WHERE value='kept'"))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "the source's permissions are left alone")
	}

	_, _, err = sqlitedb.OpenAny(t.Context(), filepath.Join(dir, "absent.sqlite"), s)
	require.ErrorIs(t, err, sqlitedb.ErrNotExist)
	junk := filepath.Join(dir, "junk.sqlite")
	require.NoError(t, os.WriteFile(junk, []byte(strings.Repeat("not a database ", 16)), 0o600))
	_, _, err = sqlitedb.OpenAny(t.Context(), junk, s)
	require.ErrorIs(t, err, sqlitedb.ErrUnreadable)
}

func TestCreate(t *testing.T) {
	s := testSchema(t)
	t.Run("existing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "existing")
		require.NoError(t, os.WriteFile(path, []byte("keep me"), 0o600))
		db, err := sqlitedb.Create(t.Context(), path, s, nil)
		require.Nil(t, db)
		require.ErrorIs(t, err, os.ErrExist)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "keep me", string(data))
	})
	t.Run("init_failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "failed.sqlite")
		failure := errors.New("init failed")
		db, err := sqlitedb.Create(t.Context(), path, s, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), "INSERT INTO parents VALUES(1, 'discard')")
			require.NoError(t, err)
			return failure
		})
		require.Nil(t, db)
		require.ErrorIs(t, err, failure)
		files, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Empty(t, files)
	})
	t.Run("initialized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "new.sqlite")
		db, err := sqlitedb.Create(t.Context(), path, s, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), "INSERT INTO parents VALUES(1, 'saved'); INSERT INTO children VALUES(1,1)")
			return err
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		ownerOnly(t, path)
		require.EqualValues(t, 1, integer(t, db.SQL(), "SELECT count(*) FROM children"))
		h, err := sqlitedb.Probe(t.Context(), path)
		require.NoError(t, err)
		require.Equal(t, sqlitedb.Header{AppID: s.AppID, Version: 3}, h)
		require.NoError(t, db.Check(t.Context(), true))
	})
	t.Run("migration_failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "failed.sqlite")
		db, err := sqlitedb.Create(t.Context(), path, scriptsSchema("CREATE TABLE good(id);", "invalid SQL"), nil)
		require.Nil(t, db)
		require.Error(t, err)
		_, err = os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

func TestOpenCancelledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancelled.sqlite")
	db := createDB(t, path, testSchema(t))
	require.NoError(t, db.Close())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	opened, err := sqlitedb.Open(ctx, path, testSchema(t))
	require.Nil(t, opened)
	require.ErrorIs(t, err, context.Canceled)
}

func TestProbeAndOpenFileErrors(t *testing.T) {
	s := testSchema(t)
	operations := map[string]func(context.Context, string) error{
		"probe": func(ctx context.Context, path string) error {
			_, err := sqlitedb.Probe(ctx, path)
			return err
		},
		"open": func(ctx context.Context, path string) error {
			db, err := sqlitedb.Open(ctx, path, s)
			if db != nil {
				require.NoError(t, db.Close())
			}
			return err
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			t.Run("junk", func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "junk.sqlite")
				require.NoError(t, os.WriteFile(path, []byte("this is not SQLite"), 0o600))
				err := operation(t.Context(), path)
				require.ErrorIs(t, err, sqlitedb.ErrUnreadable)
				require.ErrorContains(t, err, path)
			})
			t.Run("truncated", func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "truncated.sqlite")
				db := createDB(t, path, s)
				require.NoError(t, db.Close())
				require.NoError(t, os.Truncate(path, 256))
				err := operation(t.Context(), path)
				require.ErrorIs(t, err, sqlitedb.ErrUnreadable)
				require.ErrorContains(t, err, path)
			})
			t.Run("directory", func(t *testing.T) {
				path := t.TempDir()
				err := operation(t.Context(), path)
				require.Error(t, err)
				require.NotErrorIs(t, err, sqlitedb.ErrUnreadable)
				require.ErrorContains(t, err, path)
				require.NotNil(t, errors.Unwrap(err), "retain the underlying open failure")
			})
		})
	}
	t.Run("open/unparseable_schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unparseable.sqlite")
		db := createDB(t, path, s)
		require.NoError(t, db.Close())
		raw := rawDB(t, path)
		execSQL(t, raw, `PRAGMA writable_schema=ON;
			INSERT INTO sqlite_schema(type,name,tbl_name,rootpage,sql)
			VALUES('table','future','future',0,'CREATE TABLE future(x FUTURE SYNTAX !!!)');
			PRAGMA writable_schema=OFF;`)
		require.NoError(t, raw.Close())
		h, err := sqlitedb.Probe(t.Context(), path)
		require.NoError(t, err)
		latest, err := s.Latest()
		require.NoError(t, err)
		require.Equal(t, sqlitedb.Header{AppID: s.AppID, Version: latest}, h)
		opened, err := sqlitedb.Open(t.Context(), path, s)
		if opened != nil {
			require.NoError(t, opened.Close())
		}
		require.Nil(t, opened)
		require.ErrorIs(t, err, sqlitedb.ErrUnreadable)
		require.ErrorContains(t, err, path)
	})
}

func TestOpenNegativeVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "negative.sqlite")
	s := testSchema(t)
	db := createDB(t, path, s)
	execSQL(t, db.SQL(), "PRAGMA user_version=-1")
	require.EqualValues(t, -1, integer(t, db.SQL(), "PRAGMA user_version"))
	require.NoError(t, db.Close())
	opened, err := sqlitedb.Open(t.Context(), path, s)
	if opened != nil {
		require.NoError(t, opened.Close())
	}
	require.ErrorIs(t, err, sqlitedb.ErrUnreadable)
	require.Nil(t, opened)
}

func TestCloseWaitsForStatement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "close.sqlite")
	db := createDB(t, path, testSchema(t))
	locker := rawDB(t, path)
	execSQL(t, locker, "BEGIN IMMEDIATE")
	t.Cleanup(func() { _, _ = locker.ExecContext(context.Background(), "ROLLBACK") })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	statement := make(chan error, 1)
	go func() {
		_, err := db.SQL().ExecContext(ctx, "INSERT INTO parents VALUES(1, 'finished')")
		statement <- err
	}()
	require.Eventually(t, func() bool { return db.SQL().Stats().InUse == 1 }, 5*time.Second, time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- db.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before the blocked statement finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	execSQL(t, locker, "ROLLBACK")
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("Close did not finish after the statement was unblocked")
	}
	require.Zero(t, db.SQL().Stats().OpenConnections)
	require.EqualValues(t, 1, integer(t, locker, "SELECT count(*) FROM parents WHERE value='finished'"))
	require.NoError(t, <-statement)
	require.NoError(t, db.Close(), "Close remains idempotent")
}

func TestOpenNeverRecreatesRemovedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "removed.sqlite")
	db := createDB(t, path, testSchema(t))
	require.NoError(t, db.Close())
	db = openDB(t, path, testSchema(t))
	db.SQL().SetMaxIdleConns(0)
	require.NoError(t, os.Remove(path))
	_, err := db.SQL().ExecContext(t.Context(), "PRAGMA user_version")
	require.Error(t, err)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestIsBusy names a lock another connection held past the busy timeout, and nothing else.
func TestIsBusy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "busy.sqlite")
	db := createDB(t, path, testSchema(t))
	locker := rawDB(t, path)
	execSQL(t, locker, "BEGIN IMMEDIATE")
	t.Cleanup(func() { _, _ = locker.ExecContext(context.Background(), "ROLLBACK") })
	execSQL(t, db.SQL(), "PRAGMA busy_timeout = 10")
	_, err := db.SQL().ExecContext(t.Context(), "INSERT INTO parents VALUES(1, 'blocked')")
	require.Error(t, err)
	require.True(t, sqlitedb.IsBusy(err), "a held write lock: %v", err)
	_, err = db.SQL().ExecContext(t.Context(), "SELECT * FROM missing")
	require.Error(t, err)
	require.False(t, sqlitedb.IsBusy(err), "a failed statement is not busy: %v", err)
	// A table dropped under the connection's own open cursor is SQLITE_LOCKED.
	execSQL(t, locker, "ROLLBACK")
	execSQL(t, db.SQL(), "INSERT INTO parents VALUES(1, 'row'), (2, 'row')")
	conn, err := db.SQL().Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	rows, err := conn.QueryContext(t.Context(), "SELECT * FROM parents")
	require.NoError(t, err)
	require.True(t, rows.Next())
	_, err = conn.ExecContext(t.Context(), "DROP TABLE parents")
	require.NoError(t, rows.Close())
	var locked *sqlite.Error
	require.ErrorAs(t, err, &locked)
	require.Equal(t, sqlite3.SQLITE_LOCKED, locked.Code()&0xff, "%v", err)
	require.False(t, sqlitedb.IsBusy(err), "a conflict inside one connection is not busy: %v", err)
	require.False(t, sqlitedb.IsBusy(errors.New("database is locked")), "text alone classifies nothing")
}

// TestIsFull names SQLite's "database or disk is full", here a database at its
// page limit, and nothing else.
func TestIsFull(t *testing.T) {
	t.Parallel()
	db := createDB(t, filepath.Join(t.TempDir(), "full.sqlite"), testSchema(t))
	var pages int
	require.NoError(t, db.SQL().QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pages))
	execSQL(t, db.SQL(), "PRAGMA max_page_count = "+strconv.Itoa(pages))
	_, err := db.SQL().ExecContext(t.Context(), "INSERT INTO parents VALUES(1, ?)", strings.Repeat("x", 1<<16))
	require.Error(t, err)
	require.True(t, sqlitedb.IsFull(err), "a database at its page limit: %v", err)
	require.True(t, sqlitedb.IsFull(fmt.Errorf("copy: %w", err)), "a wrapped one")
	_, err = db.SQL().ExecContext(t.Context(), "SELECT * FROM missing")
	require.Error(t, err)
	require.False(t, sqlitedb.IsFull(err), "a failed statement is not full: %v", err)
	require.False(t, sqlitedb.IsFull(errors.New("database or disk is full")), "text alone classifies nothing")
}
