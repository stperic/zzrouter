package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/audit"
	"modernc.org/sqlite"
)

func status(t *testing.T, s *Store) Status {
	t.Helper()
	st, err := s.Status()
	require.NoError(t, err)
	return st
}

// fill creates keys k0…k(n-1) whose descriptions span overflow pages, so
// deleting one frees pages.
func fill(t *testing.T, s *Store, n int) {
	t.Helper()
	key := readerKey()
	key.Description = strings.Repeat("x", 16<<10)
	for i := range n {
		_, _, err := s.CreateKey(t.Context(), fmt.Sprint("k", i), key, audit.Event{})
		require.NoError(t, err)
	}
}

// freelist is PRAGMA freelist_count times page_size, read on s's connection.
func freelist(t *testing.T, s *Store) int64 {
	t.Helper()
	var count, size int64
	require.NoError(t, s.db.SQL().QueryRowContext(t.Context(), "PRAGMA freelist_count").Scan(&count))
	require.NoError(t, s.db.SQL().QueryRowContext(t.Context(), "PRAGMA page_size").Scan(&size))
	return count * size
}

// TestStatusFollowsCommits: the status carries the published meta, turns
// ready once an owner exists, and reports the check Open ran and the files.
func TestStatusFollowsCommits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, contractOptions)
	opened := status(t, s)
	assert.Equal(t, s.Meta(), opened.Meta)
	assert.False(t, opened.Initialized, "a fresh store waits for owner setup")
	assert.Equal(t, "quick_check", opened.Integrity.Check)
	assert.False(t, opened.Integrity.CheckedAt.IsZero())
	require.NotNil(t, opened.DatabaseBytes)
	info, err := os.Stat(dbPath(dir))
	require.NoError(t, err)
	assert.Equal(t, info.Size(), *opened.DatabaseBytes)
	require.NotNil(t, opened.JournalBytes)
	assert.Zero(t, *opened.JournalBytes, "no journal outside a transaction")
	require.NotNil(t, opened.FreePageBytes, "the free pages are read at open")

	claim(t, s)
	claimed := status(t, s)
	assert.True(t, claimed.Initialized, "the owner claim makes the store ready")
	assert.Greater(t, claimed.Revision, opened.Revision)
	assert.Greater(t, claimed.Sequence, opened.Sequence)
	assert.Equal(t, opened.Integrity, claimed.Integrity, "commits keep the check Open ran")

	require.NoError(t, s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "reader"}))
	recorded := status(t, s)
	assert.Equal(t, claimed.Revision, recorded.Revision, "an event alone revises nothing")
	assert.Equal(t, claimed.Sequence+1, recorded.Sequence)
}

// TestStatusFreeBytesWithoutTheGate: deletes grow the free pages read after
// each commit, and the status answers while a commit holds the writer gate and
// the one connection, so it neither takes the gate nor queries.
func TestStatusFreeBytesWithoutTheGate(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), Options{})
	fill(t, s, 16)
	filled := status(t, s)
	for i := range 16 {
		require.NoError(t, s.DeleteKey(t.Context(), fmt.Sprint("k", i), audit.Event{}))
	}
	emptied := status(t, s)
	require.NotNil(t, filled.FreePageBytes)
	require.NotNil(t, emptied.FreePageBytes)
	assert.Greater(t, *emptied.FreePageBytes, *filled.FreePageBytes, "deleted rows leave free pages")
	assert.Equal(t, freelist(t, s), *emptied.FreePageBytes, "the commit's freelist, read before its COMMIT")
	*emptied.FreePageBytes = -1
	assert.Equal(t, freelist(t, s), *status(t, s).FreePageBytes, "a caller's copy is its own")

	entered, release := make(chan struct{}), make(chan struct{})
	s.beforeCommit = func(*sql.Tx) error {
		close(entered)
		<-release
		return nil
	}
	committed := make(chan error, 1)
	go func() {
		_, err := s.CreateTeam(context.Background(), "eng", engineering(), audit.Event{})
		committed <- err
	}()
	<-entered
	answered := make(chan Status, 1)
	go func() {
		st, _ := s.Status()
		answered <- st
	}()
	select {
	case held := <-answered:
		assert.Equal(t, emptied.Meta, held.Meta, "the commit in flight is not published yet")
		require.NotNil(t, held.JournalBytes)
		assert.Positive(t, *held.JournalBytes, "a write transaction keeps a journal")
	case <-time.After(10 * time.Second):
		t.Error("Status waited for the commit that holds the gate")
	}
	close(release)
	require.NoError(t, <-committed)
	s.beforeCommit = nil
	assert.Equal(t, emptied.Sequence+1, status(t, s).Sequence)
}

// TestCommitPublishesBeforeAnotherLock: another program that locks the database
// the moment a commit releases it cannot hold back that commit's publication.
// The lock is taken when the store's connection prepares its first statement
// after the COMMIT, the last moment before that statement would wait for it.
func TestCommitPublishesBeforeAnotherLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	other := rawDB(t, dir)
	_, err := other.ExecContext(t.Context(), "PRAGMA busy_timeout = 0")
	require.NoError(t, err)
	var committed, locked bool
	var after []sqlite.AuthorizerActionCode
	hooks := func(commit sqlite.CommitHookFn, authorize sqlite.AuthorizerFn) {
		conn, err := s.db.SQL().Conn(t.Context())
		require.NoError(t, err)
		require.NoError(t, conn.Raw(func(driverConn any) error {
			driverConn.(sqlite.HookRegisterer).RegisterCommitHook(commit)
			return driverConn.(sqlite.AuthorizerRegisterer).RegisterAuthorizer(authorize)
		}))
		require.NoError(t, conn.Close())
	}
	hooks(func() int32 { committed = true; return 0 }, func(action sqlite.AuthorizerActionCode, _, _, _, _ string) sqlite.AuthorizerReturnCode {
		if committed {
			after = append(after, action)
			if !locked {
				_, err := other.ExecContext(context.Background(), "BEGIN EXCLUSIVE")
				locked = err == nil
			}
		}
		return sqlite.AuthorizerOK
	})
	sequence := s.Meta().Sequence
	start := time.Now()
	_, err = s.CreateTeam(t.Context(), "eng", engineering(), audit.Event{})
	elapsed := time.Since(start)
	hooks(nil, nil)
	if locked {
		_, rollback := other.ExecContext(t.Context(), "ROLLBACK")
		require.NoError(t, rollback)
	}
	require.NoError(t, err)
	require.True(t, committed)
	assert.Equal(t, sequence+1, status(t, s).Sequence, "the commit is published")
	assert.Empty(t, after, "the store prepared a statement between its COMMIT and the publication")
	assert.Less(t, elapsed, 2*time.Second, "the commit waited on a lock taken after it")
}

// TestStatusUnderAnotherProgramsLock: the status answers at once while another
// program holds an exclusive lock on the database file.
func TestStatusUnderAnotherProgramsLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	other := rawDB(t, dir)
	_, err := other.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	start := time.Now()
	st := status(t, s)
	elapsed := time.Since(start)
	_, err = other.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(t, err)
	assert.Less(t, elapsed, time.Second, "Status waited for another program's lock")
	assert.Equal(t, s.Meta(), st.Meta)
}

// TestStatusAfterAnUncertainCommit: a COMMIT that failed but is found to have
// committed publishes the free pages that commit left.
func TestStatusAfterAnUncertainCommit(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), Options{})
	fill(t, s, 4)
	before := status(t, s)
	// The hook commits, so the real COMMIT fails and the store reads back what happened.
	s.beforeCommit = (*sql.Tx).Commit
	err := s.DeleteKey(t.Context(), "k0", audit.Event{})
	s.beforeCommit = nil
	require.NoError(t, err, "a commit that happened is acknowledged")
	after := status(t, s)
	assert.Equal(t, before.Sequence+1, after.Sequence)
	require.NotNil(t, before.FreePageBytes)
	require.NotNil(t, after.FreePageBytes)
	assert.Greater(t, *after.FreePageBytes, *before.FreePageBytes, "the deleted key's pages are free")
	assert.Equal(t, freelist(t, s), *after.FreePageBytes)
}

// TestStatusOfAFailedStore: a store that can no longer vouch for its records
// reports ErrFailed, not the meta it last published.
func TestStatusOfAFailedStore(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), Options{})
	s.beforeCommit = func(tx *sql.Tx) error {
		if err := tx.Rollback(); err != nil {
			return err
		}
		_, err := s.db.SQL().ExecContext(context.Background(), "ALTER TABLE grants RENAME TO lost")
		return err
	}
	_, err := s.CreateTeam(t.Context(), "eng", engineering(), audit.Event{})
	require.ErrorIs(t, err, ErrFailed)
	_, err = s.Status()
	require.ErrorIs(t, err, ErrFailed)
}

// TestStatusUnavailableSizes: a file that cannot be measured is nil; an
// absent journal is empty.
func TestStatusUnavailableSizes(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), Options{})
	live := s.path
	defer func() { s.path = live }()

	s.path = filepath.Join(t.TempDir(), "gone.sqlite")
	gone := status(t, s)
	assert.Nil(t, gone.DatabaseBytes)
	require.NotNil(t, gone.JournalBytes)
	assert.Zero(t, *gone.JournalBytes)

	// A name longer than any filesystem allows fails to stat on every
	// platform, unlike a path under a file, which Windows reports as absent.
	s.path = filepath.Join(t.TempDir(), strings.Repeat("x", 300)+".sqlite")
	unreadable := status(t, s)
	assert.Nil(t, unreadable.DatabaseBytes)
	assert.Nil(t, unreadable.JournalBytes, "only an absent journal reads as empty")
}
