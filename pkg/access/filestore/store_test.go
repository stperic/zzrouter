package filestore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/common/atomicfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/accesstest"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

func TestStoreContract(t *testing.T) {
	accesstest.Run(t, func(t *testing.T) accesstest.Harness {
		dir := t.TempDir()
		var fail atomic.Bool
		sink := &recordedEvents{}
		open := func() access.Store {
			opened, err := Open(t.Context(), dir, contractOptions)
			require.NoError(t, err)
			t.Cleanup(func() { _ = opened.Close() })
			s := opened.(*store)
			s.SetAuditSink(sink)
			s.write = func(path string, data []byte, mode os.FileMode) error {
				if fail.CompareAndSwap(true, false) {
					return io.ErrClosedPipe
				}
				return atomicfile.WriteFile(path, data, mode)
			}
			return s
		}
		current := open()
		return accesstest.Harness{
			Store:          current,
			FailNextCommit: func() { fail.Store(true) },
			Events:         sink.all,
			Reopen: func() access.Store {
				require.NoError(t, current.Close())
				current = open()
				return current
			},
		}
	})
}

// contractOptions opens people the way accesstest expects.
var contractOptions = Options{People: true, ValidateExternalIssuer: accesstest.ValidateExternalIssuer, ValidateTeam: accesstest.ValidateTeam}

func openTestStore(t *testing.T, dir string) *store {
	t.Helper()
	s, err := Open(t.Context(), dir, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s.(*store)
}

func readKeys(t *testing.T, dir string) map[string]*keys.VirtualKey {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, keysFileName))
	require.NoError(t, err)
	records, err := DecodeKeys(data)
	require.NoError(t, err)
	return records
}

func TestOpenReadsMissingFilesAsEmptyAndWritesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openTestStore(t, dir)
	assert.Nil(t, s.Snapshot().Key("any"))
	assert.Nil(t, s.Snapshot().Team("any"))
	assert.Nil(t, s.People(), "a store opened without people has none")
	assert.False(t, s.Snapshot().Initialized())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestOpenRefusesUnreadableFiles(t *testing.T) {
	t.Parallel()
	for name, file := range map[string]string{"load keys.yaml": keysFileName, "load teams.yaml": teamsFileName} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte("keys: [not, a, map]\nteams: [not, a, map]\n"), 0o600))
		_, err := Open(t.Context(), dir, Options{})
		require.ErrorContains(t, err, name, "an unreadable file stops the start instead of being overwritten later")
	}
}

func TestOperationWritesOnlyItsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openTestStore(t, dir)
	_, _, err := s.CreateKey(t.Context(), "reader", keys.VirtualKey{Name: "Reader", TeamID: "team", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, teamsFileName))
	require.ErrorIs(t, err, os.ErrNotExist, "a key operation writes only keys.yaml")
	require.NoError(t, os.Remove(filepath.Join(dir, keysFileName)))
	_, err = s.CreateTeam(t.Context(), "eng", teams.Team{Name: "Engineering", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, keysFileName))
	require.ErrorIs(t, err, os.ErrNotExist, "a team operation writes only teams.yaml")
}

func TestDescriptionIsWrittenToKeysYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openTestStore(t, dir)
	_, _, err := s.CreateKey(t.Context(), "reader", keys.VirtualKey{Name: "Reader", Description: "agent: research-bot", TeamID: "team", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, keysFileName))
	require.NoError(t, err)
	assert.Contains(t, string(data), "description:")
	assert.Equal(t, "agent: research-bot", readKeys(t, dir)["reader"].Description)
}

const envKeysYAML = `version: "1"
keys:
  ci:
    name: CI
    key_env: ZZROUTER_TEST_FILESTORE_ENV_KEY
    role: user
    team_id: team
    team_role: member
`

func TestKeyEnvRecordsSurviveAFileWrite(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_FILESTORE_ENV_KEY", "ci-secret-from-environment")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, keysFileName), []byte(envKeysYAML), 0o600))
	s := openTestStore(t, dir)
	_, _, err := s.CreateKey(t.Context(), "reader", keys.VirtualKey{Name: "Reader", TeamID: "team", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	_, err = s.UpdateKey(t.Context(), "ci", func(k *keys.VirtualKey) error { k.Name = "CI renamed"; return nil }, audit.Event{})
	require.NoError(t, err)

	persisted := readKeys(t, dir)["ci"]
	assert.Equal(t, "ZZROUTER_TEST_FILESTORE_ENV_KEY", persisted.KeyEnv)
	assert.Empty(t, persisted.HashedKey)
	assert.Equal(t, "CI renamed", persisted.Name)
	id, _ := openTestStore(t, dir).Snapshot().ValidateRawKey("ci-secret-from-environment")
	assert.Equal(t, "ci", id)
}

func TestRotatingAnEnvKeyMakesItHashed(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_FILESTORE_ENV_KEY", "ci-secret-from-environment")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, keysFileName), []byte(envKeysYAML), 0o600))
	s := openTestStore(t, dir)
	rotated, raw, err := s.RotateKey(t.Context(), "ci", audit.Event{})
	require.NoError(t, err)
	assert.Empty(t, rotated.KeyEnv)
	assert.NotEmpty(t, rotated.HashedKey)
	for _, snap := range []*access.Snapshot{s.Snapshot(), openTestStore(t, dir).Snapshot()} {
		id, _ := snap.ValidateRawKey("ci-secret-from-environment")
		assert.Empty(t, id, "the environment value stops authenticating")
		id, _ = snap.ValidateRawKey(raw)
		assert.Equal(t, "ci", id)
	}
}

// failWrite fails before the rename, or after it with only directory durability unknown.
func failWrite(committed bool) func(string, []byte, os.FileMode) error {
	return func(path string, data []byte, mode os.FileMode) error {
		if !committed {
			return io.ErrClosedPipe
		}
		if err := atomicfile.WriteFile(path, data, mode); err != nil {
			return err
		}
		return errors.Join(atomicfile.ErrPostRenameDurability, io.ErrClosedPipe)
	}
}

func TestKeyRotationAcknowledgesCommittedReplacement(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "before-rename"
		if committed {
			name = "after-rename"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			_, oldKey, err := s.CreateKey(t.Context(), "reader", keys.VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"}, audit.Event{})
			require.NoError(t, err)
			s.write = failWrite(committed)
			_, newKey, err := s.RotateKey(t.Context(), "reader", audit.Event{})
			want := oldKey
			if committed {
				require.NoError(t, err, "a saved rotation must deliver its one-time key instead of asking for a retry")
				want = newKey
			} else {
				require.ErrorIs(t, err, io.ErrClosedPipe)
			}
			for _, snap := range []*access.Snapshot{s.Snapshot(), openTestStore(t, dir).Snapshot()} {
				id, _ := snap.ValidateRawKey(want)
				require.Equal(t, "reader", id)
				if committed {
					id, _ = snap.ValidateRawKey(oldKey)
					require.Empty(t, id)
				}
			}
		})
	}
}

func TestTeamSaveAcknowledgesCommittedPolicy(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "before-rename"
		if committed {
			name = "after-rename"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			_, err := s.CreateTeam(t.Context(), "engineering", teams.Team{Name: "Engineering", Kind: teams.KindShared}, audit.Event{})
			require.NoError(t, err)
			s.write = failWrite(committed)
			_, err = s.UpdateTeam(t.Context(), "engineering", func(team *teams.Team) error { team.Suspended = true; return nil }, audit.Event{})
			if committed {
				require.NoError(t, err, "committed policy must continue the caller's publication path")
			} else {
				require.ErrorIs(t, err, io.ErrClosedPipe)
			}
			require.Equal(t, committed, s.Snapshot().Team("engineering").Suspended)
			require.Equal(t, committed, openTestStore(t, dir).Snapshot().Team("engineering").Suspended)
		})
	}
}

func TestPeopleOpenWithTheStoreAndReadItsTeams(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var checked []string
	opened, err := Open(t.Context(), dir, Options{
		People:                 true,
		ValidateExternalIssuer: func(context.Context, string) error { return nil },
		ValidateTeam: func(snap *access.Snapshot, id string) error {
			checked = append(checked, id)
			if snap.Team(id) == nil {
				return people.ErrInvalidGrant
			}
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = opened.Close() })
	require.NotNil(t, opened.People())
	request := people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: people.Issuer{Reference: "installation"}}
	_, err = opened.People().Issue(t.Context(), request)
	require.ErrorIs(t, err, people.ErrInvalidGrant, "people check teams against the store's current snapshot")
	_, err = opened.CreateTeam(t.Context(), "workspace", teams.Team{Name: "Workspace", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	_, err = opened.People().Issue(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, []string{"workspace", "workspace"}, checked)
	assert.False(t, opened.Snapshot().Initialized())

	validators := Options{
		People:                 true,
		ValidateExternalIssuer: func(context.Context, string) error { return nil },
		ValidateTeam:           func(*access.Snapshot, string) error { return nil },
	}
	_, err = Open(t.Context(), dir, validators)
	require.ErrorIs(t, err, atomicfile.ErrLocked, "one store owns people.json")
	require.NoError(t, opened.Close())
	reopened, err := Open(t.Context(), dir, validators)
	require.NoError(t, err, "Close releases people.json")
	require.NoError(t, reopened.Close())
}

func TestOpenRefusesPeopleWithoutValidators(t *testing.T) {
	t.Parallel()
	issuer := func(context.Context, string) error { return nil }
	team := func(*access.Snapshot, string) error { return nil }
	for name, opts := range map[string]Options{
		"no team validator":   {People: true, ValidateExternalIssuer: issuer},
		"no issuer validator": {People: true, ValidateTeam: team},
	} {
		dir := t.TempDir()
		_, err := Open(t.Context(), dir, opts)
		require.Error(t, err, name)
		_, err = os.Stat(filepath.Join(dir, "access"))
		require.ErrorIs(t, err, os.ErrNotExist, "%s: refused before people is opened", name)
	}
}

// TestStoreLockAndCorruptLoadRelease moved here with people's persistence.
func TestPeopleLockAndCorruptLoadRelease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opened, err := Open(t.Context(), dir, contractOptions)
	require.NoError(t, err)
	_, err = Open(t.Context(), dir, contractOptions)
	require.ErrorIs(t, err, atomicfile.ErrLocked)
	require.NoError(t, opened.Close())
	path := filepath.Join(dir, peopleFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"initialized":false,"people":{},"grants":{},"unexpected":true}`), 0o600))
	_, err = Open(t.Context(), dir, contractOptions)
	require.Error(t, err)
	require.NoError(t, os.Remove(path))
	reopened, err := Open(t.Context(), dir, contractOptions)
	require.NoError(t, err, "a refused load releases the lock")
	require.NoError(t, reopened.Close())
}

// recordedEvents is an audit sink a test reads after the fact.
type recordedEvents struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordedEvents) Emit(_ context.Context, ev audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordedEvents) all() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func TestPostRenameWarningPublishesCommittedClaim(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opened, err := Open(t.Context(), dir, contractOptions)
	require.NoError(t, err)
	s := opened.(*store)
	_, err = s.CreateTeam(t.Context(), "workspace", teams.Team{Name: "Workspace", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	grant, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: people.Issuer{Reference: "installation"}})
	require.NoError(t, err)
	bound, err := s.People().Exchange(t.Context(), grant.Token, "browser proof")
	require.NoError(t, err)
	sink := &recordedEvents{}
	s.SetAuditSink(sink)
	s.write = failWrite(true)
	ctx := audit.WithIdentity(t.Context(), "claimer", "claim-request")
	person, err := s.People().ClaimOwner(ctx, bound.Token, "browser proof", people.CreatePerson{Username: "alice", Name: "Alice", Password: "a long test password"})
	require.NoError(t, err)
	assert.Equal(t, person, s.Snapshot().Owner())
	events := sink.all()
	require.Len(t, events, 1, "committed state is audited despite uncertain directory durability")
	assert.Equal(t, audit.ActionPersonCreated, events[0].Action)
	assert.Equal(t, "claim-request", events[0].RequestID)
	require.NoError(t, s.Close())
	reopened, err := Open(t.Context(), dir, contractOptions)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	assert.Equal(t, person, reopened.Snapshot().Owner())
	_, err = reopened.People().Inspect(t.Context(), bound.Token)
	require.ErrorIs(t, err, people.ErrInvalidGrant)
}

// People reads are lock-free, so the team validator may use them mid-commit.
func TestPeopleTeamValidatorMayReadPeople(t *testing.T) {
	t.Parallel()
	opened, err := Open(t.Context(), t.TempDir(), Options{
		People:                 true,
		ValidateExternalIssuer: accesstest.ValidateExternalIssuer,
		ValidateTeam: func(snap *access.Snapshot, id string) error {
			if snap.Initialized() || len(snap.People()) > 0 || snap.TeamImpact(id).People > 0 || snap.OwnerTeam(id) {
				return people.ErrInvalidGrant
			}
			return accesstest.ValidateTeam(snap, id)
		},
	})
	require.NoError(t, err)
	_, err = opened.CreateTeam(t.Context(), "workspace", teams.Team{Name: "Workspace", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	issued := make(chan error, 1)
	go func() {
		_, err := opened.People().Issue(context.Background(), people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: people.Issuer{Reference: "installation"}})
		issued <- err
	}()
	select {
	case err := <-issued:
		require.NoError(t, err)
		require.NoError(t, opened.Close())
	case <-time.After(10 * time.Second):
		t.Fatal("a people read inside the team validator blocked the commit")
	}
}
