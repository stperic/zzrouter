package sqlitedb_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestObjects: a database its schema created lists the objects the schema's
// migrations create, and one with another object does not.
func TestObjects(t *testing.T) {
	s := testSchema(t)
	want, err := s.Objects(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, want)
	db := createDB(t, filepath.Join(t.TempDir(), "objects.sqlite"), s)
	got, err := db.Objects(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, got)
	execSQL(t, db.SQL(), "CREATE INDEX planted ON parents(value)")
	got, err = db.Objects(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, want, got)
	require.NoError(t, db.Close())
}
