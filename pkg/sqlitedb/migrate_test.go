package sqlitedb_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrate(t *testing.T) {
	s := testSchema(t)
	t.Run("fresh", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fresh.sqlite")
		raw := rawDB(t, path)
		execSQL(t, raw, fmt.Sprintf("PRAGMA application_id=%d", s.AppID))
		require.NoError(t, raw.Close())
		db := openDB(t, path, s)
		from, to, err := db.Migrate(t.Context())
		require.NoError(t, err)
		require.Equal(t, 0, from)
		require.Equal(t, 3, to)
		require.EqualValues(t, 3, integer(t, db.SQL(), "PRAGMA user_version"))
		execSQL(t, db.SQL(), "INSERT INTO parents VALUES(1,'ready'); INSERT INTO children VALUES(1,1)")
		require.EqualValues(t, 1, integer(t, db.SQL(), "SELECT count(*) FROM sqlite_schema WHERE name='children_parent'"))
		require.NoError(t, db.Check(t.Context(), true))
	})
	t.Run("partial", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "partial.sqlite")
		db := createDB(t, path, schemaPrefix(t, 1))
		execSQL(t, db.SQL(), "INSERT INTO parents VALUES(7, 'preserved')")
		require.NoError(t, db.Close())
		db = openDB(t, path, s)
		require.EqualValues(t, 1, integer(t, db.SQL(), "PRAGMA user_version"))
		from, to, err := db.Migrate(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, from)
		require.Equal(t, 3, to)
		execSQL(t, db.SQL(), "INSERT INTO children VALUES(1,7)")
		require.EqualValues(t, 1, integer(t, db.SQL(), "SELECT count(*) FROM parents WHERE id=7 AND value='preserved'"))
		require.NoError(t, db.Check(t.Context(), true))
	})
	t.Run("already_latest", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "latest.sqlite")
		db := createDB(t, path, s)
		execSQL(t, db.SQL(), "INSERT INTO parents VALUES(1, 'unchanged')")
		before, err := os.ReadFile(path)
		require.NoError(t, err)
		from, to, err := db.Migrate(t.Context())
		require.NoError(t, err)
		require.Equal(t, 3, from)
		require.Equal(t, 3, to)
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, before, after)
	})
}

func TestMigrateRollback(t *testing.T) {
	t.Run("failing_second", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rollback.sqlite")
		s := scriptsSchema(
			"CREATE TABLE first(id); INSERT INTO first VALUES(1); UPDATE existing SET value='changed';",
			"CREATE TABLE second(id); INSERT INTO missing VALUES(1);",
		)
		raw := rawDB(t, path)
		execSQL(t, raw, fmt.Sprintf("PRAGMA application_id=%d; CREATE TABLE existing(value); INSERT INTO existing VALUES('original');", s.AppID))
		require.NoError(t, raw.Close())
		db := openDB(t, path, s)
		before, err := os.ReadFile(path)
		require.NoError(t, err)
		from, to, err := db.Migrate(t.Context())
		require.Error(t, err)
		require.Equal(t, 0, from)
		require.Equal(t, 0, to)
		require.EqualValues(t, 0, integer(t, db.SQL(), "PRAGMA user_version"))
		require.EqualValues(t, 0, integer(t, db.SQL(), "SELECT count(*) FROM sqlite_schema WHERE name IN ('first','second')"))
		require.EqualValues(t, 1, integer(t, db.SQL(), "SELECT count(*) FROM existing WHERE value='original'"))
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, before, after)
	})
	t.Run("partial_pending_failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "partial-failure.sqlite")
		db := createDB(t, path, schemaPrefix(t, 1))
		execSQL(t, db.SQL(), "INSERT INTO parents VALUES(1,'original')")
		require.NoError(t, db.Close())
		s := scriptsSchema(
			"CREATE TABLE parents (id INTEGER PRIMARY KEY, value TEXT NOT NULL);",
			"UPDATE parents SET value='changed'; CREATE TABLE second(id);",
			"INSERT INTO missing VALUES(1);",
		)
		db = openDB(t, path, s)
		from, to, err := db.Migrate(t.Context())
		require.Error(t, err)
		require.Equal(t, 1, from)
		require.Equal(t, 1, to)
		require.EqualValues(t, 1, integer(t, db.SQL(), "PRAGMA user_version"))
		require.EqualValues(t, 1, integer(t, db.SQL(), "SELECT count(*) FROM parents WHERE value='original'"))
		require.EqualValues(t, 0, integer(t, db.SQL(), "SELECT count(*) FROM sqlite_schema WHERE name='second'"))
	})
}
