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
	data, err := os.ReadFile(filepath.Join(dir, markerName))
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
	storeID, generation, revision, sequence := s.Meta()
	assert.NotEmpty(t, storeID)
	assert.Equal(t, storeID, generation, "the generation starts as the store ID")
	assert.Zero(t, revision)
	assert.Zero(t, sequence)
	assert.Equal(t, marker{Version: 1, StoreID: storeID}, readMarker(t, dir))
	assert.FileExists(t, dbPath(dir))
	assert.NoFileExists(t, dbPath(dir)+".staged")
	assert.Nil(t, s.People(), "a store opened without people offers none")
	require.NoError(t, s.Close())
	again := openStore(t, dir, Options{})
	reopenedID, _, _, _ := again.Meta()
	assert.Equal(t, storeID, reopenedID, "a reopen keeps the store")
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
	storeID, _, _, _ := s.Meta()
	require.NoError(t, s.Close())
	staged := dbPath(dir) + ".staged"
	marker := filepath.Join(dir, markerName)
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
	ownerOnly(filepath.Join(dir, markerName), 0o600)
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
// check or a record that breaks its rule; a failure to read is plain.
func TestOpenLabelsOnlyDamage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, openStore(t, dir, Options{}).Close())
	db, err := sqlitedb.Open(t.Context(), dbPath(dir), schema)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = check(t.Context(), db)
	require.Error(t, err, "a check that cannot run")
	var refusal *Refusal
	require.False(t, errors.As(err, &refusal), "a check that cannot run is not a failed check: %v", err)

	_, err = rawDB(t, dir).ExecContext(t.Context(), "ALTER TABLE grants RENAME TO lost")
	require.NoError(t, err)
	_, err = Open(t.Context(), dir, Options{})
	require.Error(t, err)
	require.False(t, errors.As(err, &refusal), "a load that cannot read is not a broken record: %v", err)
}

// TestSettle rolls back the journal a crash left mid-commit, so a copy of the
// file holds exactly the committed transactions, and refuses a file it cannot read.
func TestSettle(t *testing.T) {
	t.Parallel()
	require.NoError(t, Settle(t.Context(), t.TempDir()), "no database is nothing to settle")

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
	_, err = tx.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		INSERT INTO audit_events (seq, time, action, actor, subject, request_id) SELECT 1 + i, 0, 'x', 'a', hex(randomblob(512)), '' FROM n`)
	require.NoError(t, err)
	crashed := t.TempDir()
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

	require.NoError(t, Settle(t.Context(), crashed))
	assert.NoFileExists(t, dbPath(crashed)+"-journal")
	settled, err := os.ReadFile(dbPath(crashed))
	require.NoError(t, err)
	assert.Equal(t, committed, settled, "the file holds exactly the committed transactions")

	// Settle refuses what Open refuses, for the same reason.
	foreign, newer := copyStore(t, dir), copyStore(t, dir)
	_, err = rawDB(t, foreign).ExecContext(t.Context(), "PRAGMA application_id = 1")
	require.NoError(t, err)
	_, err = rawDB(t, newer).ExecContext(t.Context(), "PRAGMA user_version = 2")
	require.NoError(t, err)
	junk := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(junk, "access"), 0o700))
	require.NoError(t, os.WriteFile(dbPath(junk), []byte("not a database, just text long enough to have a header"), 0o600))
	for name, tc := range map[string]struct {
		dir    string
		reason Reason
	}{"junk": {junk, Unreadable}, "another application": {foreign, Unreadable}, "a newer schema": {newer, SchemaNewer}} {
		var refusal *Refusal
		require.ErrorAs(t, Settle(t.Context(), tc.dir), &refusal, name)
		assert.Equal(t, tc.reason, refusal.Reason, name)
		requireRefusal(t, tc.dir, tc.reason)
	}
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
		})
	}
}
