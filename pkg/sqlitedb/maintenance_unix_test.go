//go:build unix

package sqlitedb_test

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stretchr/testify/require"
)

// TestCopyToWriteLimit copies with a file-size limit smaller than one page, so
// SQLite's own write fails inside VACUUM INTO after it created the copy's
// journal, as a full disk does. It runs in a child process: the limit binds
// the whole process.
func TestCopyToWriteLimit(t *testing.T) {
	src := os.Getenv("ZZ_SQLITEDB_LIMITED_COPY")
	if src == "" {
		return
	}
	limit, err := strconv.ParseUint(os.Getenv("ZZ_SQLITEDB_COPY_LIMIT"), 10, 64)
	require.NoError(t, err)
	db := openDB(t, src, testSchema(t))
	signal.Ignore(syscall.SIGXFSZ) // the failing write reports EFBIG instead
	var rlimit syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &rlimit))
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: limit, Max: rlimit.Max}))
	require.Error(t, db.CopyTo(t.Context(), src+".copy"), "the write limit fails the copy")
}

// TestCopyToFailingWriteLeavesNothing: a copy whose writes fail part way, with
// its journal present, leaves neither the copy nor the journal.
func TestCopyToFailingWriteLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.sqlite")
	db := createDB(t, src, testSchema(t))
	// Larger than SQLite's default page cache, so VACUUM INTO spills the copy's
	// pages and its journal is still there when the write fails.
	execSQL(t, db.SQL(), "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 4096) "+
		"INSERT INTO parents SELECT i, ? FROM n", strings.Repeat("x", 1024))
	require.NoError(t, db.Close())
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	child := host.CommandContext(ctx, os.Args[0], "-test.run=^TestCopyToWriteLimit$", "-test.count=1")
	child.Env = append(os.Environ(), "ZZ_SQLITEDB_LIMITED_COPY="+src, "ZZ_SQLITEDB_COPY_LIMIT=2000")
	output, err := child.CombinedOutput()
	require.NoError(t, err, "%s", output)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Equal(t, []string{"source.sqlite"}, names, "no copy and no journal")
	require.Equal(t, filepath.Join(dir, "source.sqlite-journal"), sqlitedb.Journal(src))
}
