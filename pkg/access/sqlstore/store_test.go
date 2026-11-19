package sqlstore

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/accesstest"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
)

// contractOptions opens people the way accesstest expects.
var contractOptions = Options{People: true, ValidateExternalIssuer: accesstest.ValidateExternalIssuer}

var errInjected = errors.New("injected commit failure")

func TestStoreContract(t *testing.T) {
	accesstest.Run(t, func(t *testing.T) accesstest.Harness {
		dir := t.TempDir()
		var fail atomic.Bool
		open := func() access.Store {
			s := openStore(t, dir, contractOptions)
			s.beforeCommit = func(*sql.Tx) error {
				if fail.CompareAndSwap(true, false) {
					return errInjected
				}
				return nil
			}
			return s
		}
		current := open()
		return accesstest.Harness{
			Store:          current,
			FailNextCommit: func() { fail.Store(true) },
			Reopen: func() access.Store {
				require.NoError(t, current.Close())
				current = open()
				return current
			},
		}
	})
}

func openStore(t *testing.T, dir string, opts Options) *Store {
	t.Helper()
	s, err := Open(t.Context(), dir, opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func dbPath(dir string) string { return filepath.Join(dir, "access", fileName) }

// rawDB opens dir's database beside any open store, as an operator's tool would.
func rawDB(t *testing.T, dir string) *sql.DB { return rawFile(t, dbPath(dir)) }

func rawFile(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(t.Context(), path, schema)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db.SQL()
}

// storedEvents reads the audit table, oldest first.
func storedEvents(t *testing.T, dir string) []audit.Event { return fileEvents(t, dbPath(dir)) }

func fileEvents(t *testing.T, path string) []audit.Event {
	t.Helper()
	db, err := sqlitedb.Open(t.Context(), path, schema)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	var events []audit.Event
	require.NoError(t, scanAll(t.Context(), db.SQL(), auditCols, func(r *auditRow) { events = append(events, r.Event) }, auditTable.selectAll+" ORDER BY seq"))
	return events
}
