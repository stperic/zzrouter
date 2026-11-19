package sqlitedb_test

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/*
var fixtures embed.FS

func testSchema(t *testing.T) sqlitedb.Schema {
	t.Helper()
	f, err := fs.Sub(fixtures, "testdata")
	require.NoError(t, err)
	return sqlitedb.Schema{AppID: 0x1234abcd, FS: f}
}

func scriptsSchema(scripts ...string) sqlitedb.Schema {
	f := make(fstest.MapFS)
	var sums strings.Builder
	for i, script := range scripts {
		name := fmt.Sprintf("%04d_test.sql", i+1)
		f[name] = &fstest.MapFile{Data: []byte(script)}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256([]byte(script)), name)
	}
	f["SUMS"] = &fstest.MapFile{Data: []byte(sums.String())}
	return sqlitedb.Schema{AppID: 0x1234abcd, FS: f}
}

func schemaPrefix(t *testing.T, n int) sqlitedb.Schema {
	t.Helper()
	f := testSchema(t).FS
	names, err := fs.Glob(f, "*.sql")
	require.NoError(t, err)
	var scripts []string
	for _, name := range names[:n] {
		data, err := fs.ReadFile(f, name)
		require.NoError(t, err)
		scripts = append(scripts, string(data))
	}
	return scriptsSchema(scripts...)
}

func TestSchemaVerify(t *testing.T) {
	s := testSchema(t)
	require.NoError(t, s.Verify())
	latest, err := s.Latest()
	require.NoError(t, err)
	require.Equal(t, 3, latest)
	for _, name := range []string{"gap", "duplicate", "missing_sum", "extra_sum", "edited_file", "duplicate_sum", "invalid_name"} {
		t.Run(name, func(t *testing.T) {
			s := scriptsSchema("SELECT 1;", "SELECT 2;", "SELECT 3;")
			f := s.FS.(fstest.MapFS)
			sums := string(f["SUMS"].Data)
			switch name {
			case "gap":
				delete(f, "0002_test.sql")
			case "duplicate":
				f["0001_duplicate.sql"] = f["0001_test.sql"]
			case "missing_sum":
				f["SUMS"].Data = []byte(strings.Join(strings.Split(sums, "\n")[1:], "\n"))
			case "extra_sum":
				f["SUMS"].Data = []byte(sums + fmt.Sprintf("%x  0004_absent.sql\n", sha256.Sum256(nil)))
			case "edited_file":
				f["0001_test.sql"].Data = []byte("SELECT 99;")
			case "duplicate_sum":
				f["SUMS"].Data = []byte(sums + sums)
			case "invalid_name":
				f["migration.sql"] = &fstest.MapFile{Data: []byte("SELECT 1;")}
			}
			require.Error(t, s.Verify())
			if name == "gap" || name == "duplicate" || name == "invalid_name" {
				_, err := s.Latest()
				require.Error(t, err)
			}
		})
	}
}
