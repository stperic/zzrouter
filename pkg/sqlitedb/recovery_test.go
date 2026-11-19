package sqlitedb_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stretchr/testify/require"
)

func TestHotJournalWriter(t *testing.T) {
	path := os.Getenv("ZZ_SQLITEDB_CRASH_PATH")
	if path == "" {
		return
	}
	db := rawDB(t, path)
	execSQL(t, db, `PRAGMA journal_mode=DELETE; PRAGMA synchronous=EXTRA;
		PRAGMA cache_size=4; PRAGMA cache_spill=ON; BEGIN IMMEDIATE;
		PRAGMA user_version=777; UPDATE parents SET value=replace(value, 'committed', 'discarded');`)
	fmt.Fprintln(os.Stdout, "writer ready")
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
	t.Fatal("writer must be killed while its transaction is open")
}

func TestHotJournalRecovery(t *testing.T) {
	for _, operation := range []string{"user_version", "probe", "open"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "crashed.sqlite")
			s := testSchema(t)
			db := createDB(t, path, s)
			tx, err := db.SQL().BeginTx(t.Context(), nil)
			require.NoError(t, err)
			const rows = 128
			value := strings.Repeat("committed-", 1024)
			for i := range rows {
				_, err := tx.ExecContext(t.Context(), "INSERT INTO parents VALUES(?, ?)", i, value)
				require.NoError(t, err)
			}
			require.NoError(t, tx.Commit())
			require.NoError(t, db.Close())
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := host.CommandContext(ctx, os.Args[0], "-test.run=^TestHotJournalWriter$", "-test.count=1")
			cmd.Env = append(os.Environ(), "ZZ_SQLITEDB_CRASH_PATH="+path)
			stdin, err := cmd.StdinPipe()
			require.NoError(t, err)
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			require.NoError(t, cmd.Start())
			waited := false
			defer func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				waited = true
				t.Fatalf("writer failed: %s", stderr.String())
			}
			require.Equal(t, "writer ready", scanner.Text())
			require.NoError(t, cmd.Process.Kill())
			require.Error(t, cmd.Wait())
			waited = true
			require.NoError(t, ctx.Err(), "writer timed out: %s", stderr.String())
			journal, err := os.ReadFile(path + "-journal")
			require.NoError(t, err)
			require.Greater(t, len(journal), 512)
			require.Equal(t, []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}, journal[:8])
			ownerOnly(t, path+"-journal")
			spilled, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotEqual(t, before, spilled, "the writer must spill uncommitted pages")
			require.True(t, bytes.Contains(spilled, []byte(strings.Repeat("discarded-", 8))))
			switch operation {
			case "user_version":
				raw := rawDB(t, path)
				require.EqualValues(t, 3, integer(t, raw, "PRAGMA user_version"))
				require.NoError(t, raw.Close())
			case "probe":
				h, err := sqlitedb.Probe(t.Context(), path)
				require.NoError(t, err)
				require.Equal(t, sqlitedb.Header{AppID: s.AppID, Version: 3}, h)
			case "open":
				db = openDB(t, path, s)
				require.NoError(t, db.Close())
			}
			_, err = os.Stat(path + "-journal")
			require.ErrorIs(t, err, os.ErrNotExist)
			db = openDB(t, path, s)
			var count int
			require.NoError(t, db.SQL().QueryRowContext(t.Context(), "SELECT count(*) FROM parents WHERE value=?", value).Scan(&count))
			require.Equal(t, rows, count)
			require.EqualValues(t, 3, integer(t, db.SQL(), "PRAGMA user_version"))
			require.NoError(t, db.Check(t.Context(), true))
		})
	}
}
