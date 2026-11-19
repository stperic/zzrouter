package sqlitedb_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestObjects: a database its schema created lists the objects the schema's
// migrations create, and one with another object, or with only the whitespace
// of an object's SQL changed, does not.
func TestObjects(t *testing.T) {
	s := testSchema(t)
	want, err := s.Objects(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, want)
	db := createDB(t, filepath.Join(t.TempDir(), "objects.sqlite"), s)
	got, err := db.Objects(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, got)
	execSQL(t, db.SQL(), "PRAGMA writable_schema = ON; UPDATE sqlite_schema SET sql = replace(sql, 'TABLE parents', 'TABLE\n  parents') WHERE name = 'parents'; PRAGMA writable_schema = OFF")
	got, err = db.Objects(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, want, got, "the SQL text is compared exactly")
	require.NoError(t, db.Close())

	db = createDB(t, filepath.Join(t.TempDir(), "indexed.sqlite"), s)
	execSQL(t, db.SQL(), "CREATE INDEX planted ON parents(value)")
	got, err = db.Objects(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, want, got)
	require.NoError(t, db.Close())
}
