package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	testPassword = "a long sqlstore password" //nolint:gosec // test fixture, not a credential
	testProof    = "a sqlstore browser proof"
)

var installationIssuer = people.Issuer{Reference: "installation"}

// readerKey is a key with its personal team.
func readerKey() keys.VirtualKey { return keys.VirtualKey{Name: "Reader", Role: keys.RoleUser} }

func engineering() teams.Team { return teams.Team{Name: "Engineering", Kind: teams.KindShared} }

func versions(s *Store) (uint64, uint64) {
	m := s.Meta()
	return m.Revision, m.Sequence
}

func auditCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.SQL().QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events").Scan(&n))
	return n
}

// bound issues a grant and binds it to testProof.
func bound(t *testing.T, s *Store, req people.GrantRequest) *people.IssuedGrant {
	t.Helper()
	if req.Issuer == (people.Issuer{}) {
		req.Issuer = installationIssuer
	}
	grant, err := s.People().Issue(t.Context(), req)
	require.NoError(t, err)
	exchanged, err := s.People().Exchange(t.Context(), grant.Token, testProof)
	require.NoError(t, err)
	return exchanged
}

func claim(t *testing.T, s *Store) *people.Person {
	t.Helper()
	_, err := s.CreateTeam(t.Context(), "workspace", engineering(), audit.Event{})
	require.NoError(t, err)
	grant := bound(t, s, people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin"})
	owner, err := s.People().ClaimOwner(t.Context(), grant.Token, testProof, people.CreatePerson{Username: "owner", Name: "Owner", Password: testPassword})
	require.NoError(t, err)
	return owner
}

func TestRevisionAndSequence(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), contractOptions)
	owner := claim(t, s)
	step := func(name string, revises bool, run func()) {
		t.Helper()
		revision, sequence := versions(s)
		run()
		gotRevision, gotSequence := versions(s)
		assert.Equal(t, sequence+1, gotSequence, "%s advances the sequence", name)
		want := revision
		if revises {
			want++
		}
		assert.Equal(t, want, gotRevision, "%s revision", name)
	}
	ok := func(err error) { require.NoError(t, err) }
	step("key create", true, func() { _, _, err := s.CreateKey(t.Context(), "reader", readerKey(), audit.Event{}); ok(err) })
	step("team update", true, func() {
		_, err := s.UpdateTeam(t.Context(), "workspace", func(team *teams.Team) error { team.Name = "Renamed"; return nil }, audit.Event{})
		ok(err)
	})
	step("person update", true, func() {
		_, err := s.People().Update(t.Context(), owner.ID, people.UpdatePerson{Name: new("Renamed")})
		ok(err)
	})
	var invitation *people.IssuedGrant
	step("invitation issue", true, func() {
		var err error
		invitation, err = s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "workspace", Role: "user", Issuer: installationIssuer})
		ok(err)
	})
	step("audit record", false, func() { s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "reader"}) })
	var session *people.IssuedGrant
	step("session issue", false, func() {
		var err error
		session, err = s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSession, Issuer: installationIssuer})
		ok(err)
	})
	step("session exchange", false, func() {
		var err error
		session, err = s.People().Exchange(t.Context(), session.Token, testProof)
		ok(err)
	})
	step("session consume", false, func() { _, err := s.People().ConsumeSession(t.Context(), session.Token, testProof); ok(err) })
	step("progress", false, func() { ok(s.People().UpdateProgress(t.Context(), people.ProgressUpdate{Step: new("model")})) })
	step("milestone", false, func() { ok(s.People().MarkAppConnected(t.Context())) })
	var recovery *people.IssuedGrant
	step("recovery issue", true, func() {
		var err error
		recovery, err = s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeRecovery, TeamID: "workspace", PersonID: owner.ID, Issuer: installationIssuer})
		ok(err)
	})
	step("recovery revoke", true, func() { ok(s.People().RevokeGrant(t.Context(), recovery.ID)) })
	step("session issue that prunes the revoked recovery grant", false, func() {
		before := len(s.Snapshot().Invitations())
		_, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSession, Issuer: installationIssuer})
		ok(err)
		require.Len(t, s.Snapshot().Invitations(), before-1, "the revoked recovery grant was pruned")
	})

	revision, sequence := versions(s)
	rows := auditCount(t, s)
	require.NoError(t, s.People().RevokeGrant(t.Context(), invitation.ID))
	require.NoError(t, s.People().RevokeGrant(t.Context(), invitation.ID), "revoking a revoked grant changes nothing")
	gotRevision, gotSequence := versions(s)
	assert.Equal(t, revision+1, gotRevision)
	assert.Equal(t, sequence+1, gotSequence, "a no-op commit advances neither")
	assert.Equal(t, rows+1, auditCount(t, s), "a no-op commit writes no row")
	s.Record(t.Context(), audit.Event{})
	assert.Equal(t, rows+1, auditCount(t, s), "a zero event writes nothing")
	_, gotSequence = versions(s)
	assert.Equal(t, sequence+1, gotSequence)
}

func TestAuditRowCommitsWithItsChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	ctx := audit.WithIdentity(t.Context(), "operator", "request-1")
	s.beforeCommit = func(*sql.Tx) error { return errInjected }
	_, _, err := s.CreateKey(ctx, "lost", readerKey(), audit.Event{Action: audit.ActionKeyCreated, Subject: "lost"})
	require.ErrorIs(t, err, errInjected)
	require.Empty(t, storedEvents(t, dir), "the failed commit left no audit row")
	s.beforeCommit = nil
	_, _, err = s.CreateKey(ctx, "kept", readerKey(), audit.Event{Action: audit.ActionKeyCreated, Subject: "kept"})
	require.NoError(t, err)
	require.NoError(t, s.Close())

	reopened := openStore(t, dir, Options{})
	assert.Nil(t, reopened.Snapshot().Key("lost"))
	assert.NotNil(t, reopened.Snapshot().Key("kept"))
	events := storedEvents(t, dir)
	require.Equal(t, []string{"personal-kept", "kept"}, subjects(events), "the personal team's row, then the key's, in one commit")
	for _, ev := range events {
		assert.Equal(t, "operator", ev.Actor, "the actor comes from the context")
		assert.Equal(t, "request-1", ev.RequestID)
	}
	sequence := reopened.Meta().Sequence
	assert.EqualValues(t, 1, sequence)
}

func TestUncertainCommit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		hook      func(*sql.Tx) error
		committed bool
	}{
		"committed":   {hook: (*sql.Tx).Commit, committed: true},
		"rolled back": {hook: (*sql.Tx).Rollback},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := openStore(t, dir, Options{})
			_, _, err := s.CreateKey(t.Context(), "reader", readerKey(), audit.Event{})
			require.NoError(t, err)
			before := s.Snapshot()
			sequence := s.Meta().Sequence
			// The hook ends the transaction itself, so the real COMMIT fails either way.
			s.beforeCommit = tc.hook
			_, err = s.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error { k.Name = "Renamed"; return nil }, audit.Event{})
			s.beforeCommit = nil
			got := s.Meta().Sequence
			if tc.committed {
				require.NoError(t, err, "a commit that happened is acknowledged")
				assert.Equal(t, "Renamed", s.Snapshot().Key("reader").Name)
				assert.Equal(t, sequence+1, got)
			} else {
				require.ErrorIs(t, err, sql.ErrTxDone)
				assert.Same(t, before, s.Snapshot(), "nothing is published")
				assert.Equal(t, sequence, got)
			}
			_, err = s.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error { k.Description = "later"; return nil }, audit.Event{})
			require.NoError(t, err, "the store keeps accepting writes")
		})
	}
	// A store that cannot read itself back fails closed.
	for name, broken := range map[string]func(s *Store, tx *sql.Tx) error{
		"unreadable after": func(s *Store, tx *sql.Tx) error {
			if err := tx.Rollback(); err != nil {
				return err
			}
			_, err := s.db.SQL().ExecContext(context.Background(), "ALTER TABLE grants RENAME TO lost")
			return err
		},
		// The driver could not end the transaction: an autocommit read would see its writes.
		"left open": func(s *Store, tx *sql.Tx) error {
			if err := tx.Rollback(); err != nil {
				return err
			}
			sequence := s.Meta().Sequence
			for _, query := range []string{"BEGIN", "UPDATE keys SET name = 'Renamed'", fmt.Sprintf("UPDATE meta SET value = '%d' WHERE key = 'sequence'", sequence+1)} {
				if _, err := s.db.SQL().ExecContext(context.Background(), query); err != nil {
					return err
				}
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var fatal []error
			dir := t.TempDir()
			opts := contractOptions
			opts.Fatal = func(err error) { fatal = append(fatal, err) }
			s := openStore(t, dir, opts)
			claim(t, s)
			_, _, err := s.CreateKey(t.Context(), "reader", readerKey(), audit.Event{})
			require.NoError(t, err)
			s.beforeCommit = func(tx *sql.Tx) error { return broken(s, tx) }
			_, err = s.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error { k.Name = "Renamed"; return nil }, audit.Event{})
			require.ErrorIs(t, err, ErrFailed)
			snap := s.Snapshot()
			assert.Nil(t, snap.Key("reader"), "the snapshot admits no one")
			assert.Nil(t, snap.Team("workspace"))
			assert.Empty(t, snap.People())
			assert.True(t, snap.HasGatedTeam(), "keyless callers stay refused")
			assert.True(t, snap.Initialized(), "setup cannot start")
			assert.Nil(t, snap.Owner())
			_, err = s.People().Authenticate(t.Context(), "owner", testPassword)
			require.ErrorIs(t, err, people.ErrInvalidCredentials)
			_, err = s.CreateTeam(t.Context(), "eng", engineering(), audit.Event{})
			require.ErrorIs(t, err, ErrFailed, "every later write is refused")
			s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "reader"})
			_, _, err = s.AuditPage(t.Context(), audit.Filter{}, "", 1)
			require.ErrorIs(t, err, ErrFailed)
			require.Len(t, fatal, 1, "Fatal is called once")
			require.ErrorIs(t, fatal[0], ErrFailed)
			require.NoError(t, s.Close())
			if name == "left open" {
				assert.Equal(t, "Reader", openStore(t, dir, Options{}).Snapshot().Key("reader").Name, "the acknowledged-looking write was never committed")
			}
		})
	}
}

// TestBusyDatabaseRefusesTheWrite: a lock another process holds past the busy
// timeout refuses the write with ErrBusy, whether the write cannot begin, a
// statement inside it needs the lock to spill its pages, or its COMMIT cannot
// finish, and changes nothing.
func TestBusyDatabaseRefusesTheWrite(t *testing.T) {
	t.Parallel()
	reader := "BEGIN; SELECT count(*) FROM keys"
	rename := func(k *keys.VirtualKey) error { k.Name = "Renamed"; return nil }
	spill := func(k *keys.VirtualKey) error {
		k.Metadata = map[string]string{"blob": strings.Repeat("x", 1<<20)}
		return nil
	}
	for name, tc := range map[string]struct {
		lock, cache string
		apply       func(*keys.VirtualKey) error
	}{
		"a reader at COMMIT":    {reader, "", rename},
		"a writer at BEGIN":     {"BEGIN IMMEDIATE", "", rename},
		"a reader during spill": {reader, "PRAGMA cache_size = 10", spill},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := openStore(t, dir, Options{})
			_, _, err := s.CreateKey(t.Context(), "reader", readerKey(), audit.Event{})
			require.NoError(t, err)
			// The store's one connection keeps these settings.
			for _, pragma := range []string{"PRAGMA busy_timeout = 50", tc.cache} {
				if pragma != "" {
					_, err = s.db.SQL().ExecContext(t.Context(), pragma)
					require.NoError(t, err)
				}
			}
			before := s.Snapshot()
			sequence := s.Meta().Sequence
			other := rawDB(t, dir)
			_, err = other.ExecContext(t.Context(), tc.lock)
			require.NoError(t, err)
			_, err = s.UpdateKey(t.Context(), "reader", tc.apply, audit.Event{Action: audit.ActionKeyUpdated, Subject: "refused"})
			require.ErrorIs(t, err, ErrBusy)
			assert.Equal(t, ErrBusy.Error(), err.Error(), "no database text")
			assert.Same(t, before, s.Snapshot(), "nothing is published")
			got := s.Meta().Sequence
			assert.Equal(t, sequence, got)
			_, err = other.ExecContext(t.Context(), "ROLLBACK")
			require.NoError(t, err)
			_, err = s.UpdateKey(t.Context(), "reader", rename, audit.Event{Action: audit.ActionKeyUpdated, Subject: "kept"})
			require.NoError(t, err, "the store keeps accepting writes")
			require.NoError(t, s.Close())
			assert.Equal(t, []string{"kept"}, subjects(storedEvents(t, dir)), "the refused write recorded nothing")
		})
	}
}

// TestConcurrentWritersAndReaders mixes key, team and people writers with lock-free readers.
func TestConcurrentWritersAndReaders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, contractOptions)
	owner := claim(t, s)
	_, err := s.CreateTeam(t.Context(), "eng", engineering(), audit.Event{})
	require.NoError(t, err)
	start := s.Meta().Sequence
	const writers = 6
	var wg sync.WaitGroup
	done := make(chan struct{})
	for range 3 {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				snap := s.Snapshot()
				for _, key := range snap.Keys() {
					_ = key.Name
				}
				_ = snap.Team("eng")
				_ = snap.People()
				_ = snap.Progress()
			}
		})
	}
	var writersWG sync.WaitGroup
	for i := range writers {
		writersWG.Go(func() {
			member := readerKey()
			member.TeamID, member.TeamRole = "eng", string(teams.RoleMember)
			_, _, err := s.CreateKey(context.Background(), fmt.Sprint("key-", i), member, audit.Event{Action: audit.ActionKeyCreated, Subject: fmt.Sprint(i)})
			assert.NoError(t, err)
		})
		writersWG.Go(func() {
			_, err := s.UpdateTeam(context.Background(), "eng", func(team *teams.Team) error { team.RPMLimit++; return nil }, audit.Event{})
			assert.NoError(t, err)
		})
		writersWG.Go(func() {
			_, err := s.People().Update(context.Background(), owner.ID, people.UpdatePerson{Name: new(fmt.Sprint("Owner ", i))})
			assert.NoError(t, err)
		})
	}
	writersWG.Wait()
	close(done)
	wg.Wait()
	assert.Len(t, keyIDs(s), writers)
	assert.Equal(t, writers, s.Snapshot().Team("eng").RPMLimit)
	end := s.Meta().Sequence
	assert.Equal(t, start+3*writers, end, "every commit took exactly one sequence number")
	require.NoError(t, s.Close())
	assert.Len(t, storedEvents(t, dir), 2+2*writers, "the claim's two events, then one per key and per rename")
}

func keyIDs(s *Store) []string {
	var ids []string
	for id := range s.Snapshot().Keys() {
		ids = append(ids, id)
	}
	return ids
}

// storedView is everything a snapshot holds, for comparison without tolerance.
type storedView struct {
	Keys   map[string]*keys.VirtualKey
	Teams  map[string]*teams.Team
	People people.Records
}

func viewOf(snap *access.Snapshot) storedView {
	return storedView{Keys: maps.Collect(snap.Keys()), Teams: maps.Collect(snap.Teams()), People: snap.PeopleState().Records()}
}

func copyStore(t *testing.T, from string) string {
	t.Helper()
	to := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(to, "access"), 0o700))
	for _, name := range []string{filepath.Join("access", fileName), MarkerName} {
		data, err := os.ReadFile(filepath.Join(from, name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(to, name), data, 0o600))
	}
	return to
}

// TestPublishedStateIsStoredState publishes exactly what a reopen reads, on
// the normal path and on an acknowledged uncertain commit. Not parallel: it
// fixes the clock.
func TestPublishedStateIsStoredState(t *testing.T) {
	// Every time the store sets has nanoseconds, which storage truncates.
	restore := utils.SetClock(utils.FixedClock(time.Date(2031, 2, 3, 4, 5, 6, 123456789, time.UTC)))
	defer restore()
	dir := t.TempDir()
	s := openStore(t, dir, contractOptions)
	owner := claim(t, s)
	behind := time.Date(2029, 5, 6, 7, 8, 9, 987654321, time.FixedZone("behind", -5*3600))
	_, err := s.CreateTeam(t.Context(), "eng", teams.Team{
		Name: "Engineering", Kind: teams.KindShared, AllowedModels: []string{}, SuspendedAt: &behind, Metadata: map[string]string{},
		RouteAccess: teams.RouteAccess{Mode: "none", Routes: []string{}}, ProviderAccess: teams.ProviderAccess{Providers: []string{}},
	}, audit.Event{})
	require.NoError(t, err)
	key := readerKey()
	expires := time.Date(2030, 1, 2, 3, 4, 5, 123456789, time.FixedZone("ahead", 2*3600))
	key.ExpiresAt, key.SuspendedAt, key.Metadata = &expires, &behind, map[string]string{}
	_, _, err = s.CreateKey(t.Context(), "reader", key, audit.Event{})
	require.NoError(t, err)
	invitation := bound(t, s, people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "eng", Role: "user"})
	member, err := s.People().AcceptInvitation(t.Context(), invitation.Token, testProof, people.CreatePerson{Username: "member", Name: "Member", Password: testPassword})
	require.NoError(t, err)
	assert.Equal(t, owner, s.Snapshot().Owner(), "people answer in stored form")
	assert.Equal(t, member, s.Snapshot().Person(member.ID))
	bound(t, s, people.GrantRequest{Purpose: people.PurposeSession})
	recovery, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeRecovery, TeamID: "eng", PersonID: member.ID, Issuer: installationIssuer})
	require.NoError(t, err)
	for _, grant := range s.Snapshot().Invitations() {
		if grant.ID == recovery.ID {
			assert.Equal(t, recovery.Grant, grant)
		}
	}
	require.NoError(t, s.People().UpdateProgress(t.Context(), people.ProgressUpdate{Dismissed: new(true)}))
	require.NoError(t, s.People().MarkResponse(t.Context(), owner.ID))
	published := viewOf(s.Snapshot())
	require.NoError(t, s.Close())
	assert.Equal(t, published, viewOf(openStore(t, dir, contractOptions).Snapshot()), "the published state is the stored state")

	// The same change on two copies of one store, one acknowledged after a failed COMMIT.
	later := time.Date(2032, 3, 4, 5, 6, 7, 891011121, time.FixedZone("ahead", 3600))
	suspend := func(team *teams.Team) error { team.SuspendedAt = &later; return nil }
	other := copyStore(t, dir)
	normal := openStore(t, dir, contractOptions)
	_, err = normal.UpdateTeam(t.Context(), "eng", suspend, audit.Event{})
	require.NoError(t, err)
	uncertain := openStore(t, other, contractOptions)
	uncertain.beforeCommit = (*sql.Tx).Commit
	_, err = uncertain.UpdateTeam(t.Context(), "eng", suspend, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, viewOf(normal.Snapshot()), viewOf(uncertain.Snapshot()), "an acknowledged uncertain commit publishes what the normal path does")
	want := viewOf(normal.Snapshot())
	require.NoError(t, normal.Close())
	assert.Equal(t, want, viewOf(openStore(t, dir, contractOptions).Snapshot()))
}

// TestRefusedEventChangesNothing refuses metadata JSON would alter or cannot
// encode before any transaction opens.
func TestRefusedEventChangesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	_, _, err := s.CreateKey(t.Context(), "reader", readerKey(), audit.Event{})
	require.NoError(t, err)
	for name, metadata := range map[string]map[string]any{"invalid UTF-8": {"bad\xffname": 1}, "NaN": {"n": math.NaN()}} {
		_, err := s.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error { k.Name = "Renamed"; return nil },
			audit.Event{Action: audit.ActionKeyUpdated, Subject: "reader", Metadata: metadata})
		require.Error(t, err, name)
		assert.NotContains(t, err.Error(), "sql:", "%s: refused by validation, not inside a transaction", name)
		assert.Equal(t, "Reader", s.Snapshot().Key("reader").Name, name)
		s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "reader", Metadata: metadata})
	}
	sequence := s.Meta().Sequence
	assert.EqualValues(t, 1, sequence, "a refused event commits nothing")
	assert.Empty(t, storedEvents(t, dir))
}

// TestCallerMetadataIsNotRewritten leaves the caller's event metadata as it
// was, whether the commit succeeds, is refused or only records an event.
func TestCallerMetadataIsNotRewritten(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), Options{})
	metadata := func() map[string]any {
		return map[string]any{"count": 3, "fields": map[string]string{"name": "changed"}, "purpose": people.PurposeInvitation}
	}
	shared := metadata()
	event := audit.Event{Action: audit.ActionKeyCreated, Subject: "reader", Metadata: shared}
	_, _, err := s.CreateKey(t.Context(), "reader", readerKey(), event)
	require.NoError(t, err)
	assert.Equal(t, metadata(), shared, "after a commit")
	s.beforeCommit = func(*sql.Tx) error { return errInjected }
	_, err = s.UpdateKey(t.Context(), "reader", func(*keys.VirtualKey) error { return nil }, event)
	require.ErrorIs(t, err, errInjected)
	s.beforeCommit = nil
	assert.Equal(t, metadata(), shared, "after a refused commit")
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			s.Record(context.Background(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "reader", Metadata: shared})
		})
	}
	wg.Wait()
	assert.Equal(t, metadata(), shared, "after concurrent records")
}
