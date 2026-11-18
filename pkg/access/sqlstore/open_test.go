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
	assert.Equal(t, "access store cannot open: "+string(reason), err.Error())
	assert.Regexp(t, "^Stop the service, then ", refusal.NextAction())
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

func TestOpenRefusesPeopleWithoutValidators(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]Options{
		"no team validator":   {People: true, ValidateExternalIssuer: contractOptions.ValidateExternalIssuer},
		"no issuer validator": {People: true, ValidateTeam: contractOptions.ValidateTeam},
	} {
		dir := t.TempDir()
		_, err := Open(t.Context(), dir, opts)
		require.Error(t, err, name)
		assert.NoDirExists(t, filepath.Join(dir, "access"), "%s: refused before anything is created", name)
	}
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
