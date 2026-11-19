package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
)

func readMarker(t *testing.T, dir string) marker {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, MarkerName))
	require.NoError(t, err)
	var m marker
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

// requireRefusal opens dir and requires reason, with a message free of paths
// and database text.
func requireRefusal(t *testing.T, dir string, reason Reason) {
	t.Helper()
	s, err := Open(t.Context(), dir, Options{})
	require.Nil(t, s)
	var refusal *Refusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, reason, refusal.Reason)
	assert.Regexp(t, "^Stop the service, then ", refusal.NextAction())
	assert.Contains(t, err.Error(), string(reason))
	assert.Contains(t, err.Error(), refusal.NextAction(), "the message carries the operator's next step")
	assert.NotContains(t, err.Error(), dir, "the message names no path")
	assert.NotContains(t, err.Error(), "sqlite", "the message carries no database text")
}

func TestOpenCreatesTheStoreAndItsMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	m := s.Meta()
	storeID := m.StoreID
	assert.NotEmpty(t, storeID)
	assert.Equal(t, storeID, m.Generation, "the generation starts as the store ID")
	assert.Equal(t, 1, m.SchemaVersion, "read from the file header")
	assert.Zero(t, m.Revision)
	assert.Zero(t, m.Sequence)
	assert.Equal(t, marker{Version: 1, StoreID: storeID}, readMarker(t, dir))
	assert.FileExists(t, dbPath(dir))
	assert.NoFileExists(t, dbPath(dir)+".staged")
	assert.Nil(t, s.People(), "a store opened without people offers none")
	require.NoError(t, s.Close())
	again := openStore(t, dir, Options{})
	assert.Equal(t, storeID, again.Meta().StoreID, "a reopen keeps the store")
}

func TestOpenRefusesPeopleWithoutAnIssuerValidator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Open(t.Context(), dir, Options{People: true})
	require.Error(t, err)
	assert.NoDirExists(t, filepath.Join(dir, "access"), "refused before anything is created")
}

func TestOpenRefusesAMarkedStoreWithoutItsDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	require.NoError(t, s.Close())
	require.NoError(t, os.Remove(dbPath(dir)))
	requireRefusal(t, dir, Missing)
	assert.NoFileExists(t, dbPath(dir), "a refusal creates nothing")
	assert.NoFileExists(t, dbPath(dir)+".staged")
}

func TestOpenRefusesAJunkFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "access"), 0o700))
	require.NoError(t, os.WriteFile(dbPath(dir), []byte("not a database, just text long enough to have a header"), 0o600))
	requireRefusal(t, dir, Unreadable)
}

func TestOpenRefusesANewerSchema(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, openStore(t, dir, Options{}).Close())
	_, err := rawDB(t, dir).ExecContext(t.Context(), "PRAGMA user_version = 2")
	require.NoError(t, err)
	requireRefusal(t, dir, SchemaNewer)
}

func TestOpenRefusesARecordItsRulesRefuse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	_, _, err := s.CreateKey(t.Context(), "reader", readerKey(), audit.Event{})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	// Year 10000 passes every SQL constraint but not VirtualKey.Validate.
	_, err = rawDB(t, dir).ExecContext(t.Context(), "UPDATE keys SET expires_at = 253402300800000000")
	require.NoError(t, err)
	requireRefusal(t, dir, IntegrityFailed)
}

func TestOpenTidiesStagingAndTheMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	storeID := s.Meta().StoreID
	require.NoError(t, s.Close())
	staged := dbPath(dir) + ".staged"
	marker := filepath.Join(dir, MarkerName)
	for name, prepare := range map[string]func(){
		"leftover staging": func() { require.NoError(t, os.WriteFile(staged, []byte("interrupted"), 0o600)) },
		"no marker":        func() { require.NoError(t, os.Remove(marker)) },
		"another store":    func() { require.NoError(t, os.WriteFile(marker, []byte(`{"version":1,"store_id":"other"}`), 0o600)) },
		"unreadable":       func() { require.NoError(t, os.WriteFile(marker, []byte("{"), 0o600)) },
	} {
		prepare()
		require.NoError(t, openStore(t, dir, Options{}).Close(), name)
		assert.NoFileExists(t, staged, name)
		assert.Equal(t, storeID, readMarker(t, dir).StoreID, name)
	}
}

// TestOpenDropsTheJournalOfALostDatabase starts over beside the journal of a
// database that was removed with its marker, which SQLite would otherwise
// replay into the new one.
func TestOpenDropsTheJournalOfALostDatabase(t *testing.T) {
	t.Parallel()
	old := t.TempDir()
	s := openStore(t, old, Options{})
	_, _, err := s.CreateKey(t.Context(), "reader", readerKey(), audit.Event{})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	// Without syncs SQLite writes a complete journal header at once: a hot journal.
	db := rawDB(t, old)
	_, err = db.ExecContext(t.Context(), "PRAGMA synchronous = OFF")
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), "UPDATE keys SET name = 'changed'; UPDATE meta SET value = 'changed'")
	require.NoError(t, err)
	journal, err := os.ReadFile(dbPath(old) + "-journal")
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "access"), 0o700))
	require.NoError(t, os.WriteFile(dbPath(dir)+"-journal", journal, 0o600))
	fresh := openStore(t, dir, Options{})
	assert.Nil(t, fresh.Snapshot().Key("reader"))
	assert.NoFileExists(t, dbPath(dir)+"-journal")
	require.NoError(t, fresh.Close())
	require.NoError(t, openStore(t, dir, Options{}).Close(), "the new store reopens")
}

func TestStoreFilesAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows restricts these files with ACLs, not mode bits")
	}
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, openStore(t, dir, Options{}).Close())
	require.NoError(t, os.Chmod(filepath.Join(dir, "access"), 0o755))
	require.NoError(t, os.Chmod(dbPath(dir), 0o644))
	s := openStore(t, dir, Options{})
	ownerOnly := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, want, info.Mode().Perm(), path)
	}
	ownerOnly(filepath.Join(dir, "access"), 0o700)
	ownerOnly(dbPath(dir), 0o600)
	ownerOnly(filepath.Join(dir, MarkerName), 0o600)
	journal := errors.New("no journal seen")
	s.beforeCommit = func(*sql.Tx) error {
		info, err := os.Stat(dbPath(dir) + "-journal")
		if err == nil {
			journal = nil
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the journal")
		}
		return err
	}
	_, _, err := s.CreateKey(t.Context(), "reader", readerKey(), audit.Event{})
	require.NoError(t, err)
	require.NoError(t, journal, "a commit writes through a journal")
	ownerOnly(dbPath(dir), 0o600)
	assert.NoFileExists(t, dbPath(dir)+"-journal", "a clean commit leaves no journal")
}

// TestOpenFailuresThatAreNotDamage are no refusal: the store may be intact.
func TestOpenFailuresThatAreNotDamage(t *testing.T) {
	t.Parallel()
	existing := t.TempDir()
	require.NoError(t, openStore(t, existing, Options{}).Close())
	for name, dir := range map[string]string{"fresh": t.TempDir(), "existing": existing} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		s, err := Open(ctx, dir, Options{})
		require.Nil(t, s, name)
		require.Equal(t, context.Canceled, err, "%s: a cancelled open returns the context's error unwrapped", name)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("mode bits do not stop this user from opening the file")
	}
	require.NoError(t, os.Chmod(dbPath(existing), 0))
	s, err := Open(t.Context(), existing, Options{})
	require.Nil(t, s)
	require.Error(t, err)
	var refusal *Refusal
	require.False(t, errors.As(err, &refusal), "a permission error is not an unreadable store: %v", err)
}

// TestOpenLabelsOnlyDamage refuses with integrity_failed only for a failed
// check, schema objects this release does not create or a record that breaks
// its rule; a failure to read is plain.
func TestOpenLabelsOnlyDamage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, openStore(t, dir, Options{}).Close())
	db, err := sqlitedb.Open(t.Context(), dbPath(dir), schema)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = check(t.Context(), db, false)
	require.Error(t, err, "a check that cannot run")
	var refusal *Refusal
	require.False(t, errors.As(err, &refusal), "a check that cannot run is not a failed check: %v", err)

	// A temporary view on the connection shadows a table the loader reads, so
	// every check passes and the load's query fails without a broken record.
	db, err = sqlitedb.Open(t.Context(), dbPath(dir), schema)
	require.NoError(t, err)
	_, err = db.SQL().ExecContext(t.Context(), "CREATE TEMP VIEW keys AS SELECT 1 AS unrelated")
	require.NoError(t, err)
	_, err = check(t.Context(), db, false)
	require.Error(t, err, "a load that cannot read")
	require.False(t, errors.As(err, &refusal), "a load that cannot read is not a broken record: %v", err)
	require.NoError(t, db.Close())
}

// TestOpenRefusesSchemaObjectsThisReleaseDoesNotCreate: a live database
// another tool altered, even by an index or the statistics ANALYZE adds, is
// integrity_failed at open, as recovery would refuse to back it up.
func TestOpenRefusesSchemaObjectsThisReleaseDoesNotCreate(t *testing.T) {
	t.Parallel()
	for name, alteration := range map[string]string{
		"an added index":   "CREATE INDEX extra ON keys(name)",
		"ANALYZE":          "ANALYZE",
		"a renamed table":  "ALTER TABLE grants RENAME TO lost",
		"an added trigger": "CREATE TRIGGER planted AFTER INSERT ON teams BEGIN SELECT 1; END",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := openStore(t, dir, contractOptions)
			claim(t, s)
			require.NoError(t, s.Close())
			_, err := rawDB(t, dir).ExecContext(t.Context(), alteration)
			require.NoError(t, err)
			requireRefusal(t, dir, IntegrityFailed)
			_, err = Open(t.Context(), dir, Options{})
			assert.Contains(t, err.Error(), "integrity_failed (its schema objects differ from this release's)", "an altered file is told apart from a damaged one")
		})
	}
}

// crashedMidCommit returns the directory of a store holding one committed
// audit row as a crash mid-commit leaves it (a hot journal beside a file that
// holds uncommitted pages), and the committed file's bytes.
func crashedMidCommit(t *testing.T) (crashed string, committed []byte) {
	t.Helper()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "committed"})
	require.NoError(t, s.Close())
	committed, err := os.ReadFile(dbPath(dir))
	require.NoError(t, err)

	// A transaction larger than the page cache writes into the file before
	// COMMIT; copying both files then leaves what a crash would.
	db := rawDB(t, dir)
	for _, pragma := range []string{"PRAGMA synchronous = OFF", "PRAGMA cache_size = 10"} {
		_, err = db.ExecContext(t.Context(), pragma)
		require.NoError(t, err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	// A committed row changes first, so its page is among those spilled.
	_, err = tx.ExecContext(t.Context(), `UPDATE meta SET value = '999' WHERE key = 'sequence';
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		INSERT INTO audit_events (seq, time, action, actor, subject, request_id) SELECT 1 + i, 0, 'x', 'a', hex(randomblob(512)), '' FROM n`)
	require.NoError(t, err)
	crashed = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(crashed, "access"), 0o700))
	for _, suffix := range []string{"", "-journal"} {
		data, err := os.ReadFile(dbPath(dir) + suffix)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dbPath(crashed)+suffix, data, 0o600))
	}
	require.NoError(t, tx.Rollback())
	inFlight, err := os.ReadFile(dbPath(crashed))
	require.NoError(t, err)
	require.NotEqual(t, committed, inFlight, "the copy holds uncommitted pages")
	return crashed, committed
}

// TestOpenRefusesRecordsThatBreakTheRulesBetweenThem damages how records
// relate through a raw connection, past every SQL constraint; each is
// integrity_failed at open.
func TestOpenRefusesRecordsThatBreakTheRulesBetweenThem(t *testing.T) {
	t.Parallel()
	for name, damage := range map[string]string{
		"a personal team without its key": "DELETE FROM keys WHERE id = 'solo'",
		"the owner's team suspended":      "UPDATE teams SET suspended = 1 WHERE id = 'workspace'",
		"a person in a personal team":     "UPDATE people SET team_id = 'personal-solo'",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := openStore(t, dir, contractOptions)
			claim(t, s)
			_, _, err := s.CreateKey(t.Context(), "solo", readerKey(), audit.Event{})
			require.NoError(t, err)
			require.NoError(t, s.Close())
			_, err = rawDB(t, dir).ExecContext(t.Context(), damage)
			require.NoError(t, err)
			requireRefusal(t, dir, IntegrityFailed)
			_, err = Open(t.Context(), dir, Options{})
			assert.NotContains(t, err.Error(), "schema objects", "damage is not called an altered schema")
		})
	}
}

// TestOpenWhileAnotherProgramHoldsTheDatabase: an open that waits out another
// program's lock is ErrBusy, not a Refusal (the file is intact), with no path
// or database text, and the store opens once the program lets go.
func TestOpenWhileAnotherProgramHoldsTheDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, openStore(t, dir, Options{}).Close())
	other := rawDB(t, dir)
	_, err := other.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	s, err := Open(t.Context(), dir, Options{})
	require.Nil(t, s)
	require.ErrorIs(t, err, ErrBusy)
	var refusal *Refusal
	assert.False(t, errors.As(err, &refusal), "a held file is not a damaged one")
	assert.Contains(t, err.Error(), "another program is holding the access database")
	assert.NotContains(t, err.Error(), dir, "the message names no path")
	assert.NotContains(t, err.Error(), "locked", "the message carries no database text")
	_, err = other.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(t, err)
	require.NoError(t, openStore(t, dir, Options{}).Close())
}
