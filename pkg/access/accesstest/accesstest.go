// Package accesstest is the contract every access.Store implementation passes.
package accesstest

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Harness is one empty store under test.
type Harness struct {
	Store          access.Store
	Reopen         func() access.Store // closes Store and opens its persisted state again
	FailNextCommit func()              // makes the next persist fail before anything is committed
}

// Run checks every rule that holds for any access.Store.
func Run(t *testing.T, newHarness func(*testing.T) Harness) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, Harness)
	}{
		{"KeyLifecycle", testKeyLifecycle},
		{"KeyStoreOwnedFields", testKeyStoreOwnedFields},
		{"KeyRotation", testKeyRotation},
		{"TeamLifecycle", testTeamLifecycle},
		{"EveryCommittedStateReopens", testInvalidRecords},
		{"ConcurrentUpdatesLoseNothing", testConcurrentUpdates},
		{"ApplyErrorIsReturnedUnwrapped", testApplyError},
		{"HasGatedTeam", testHasGatedTeam},
		{"HasGatedTeamDuringBlockedUpdate", testHasGatedTeamDuringBlockedUpdate},
		{"KeySnapshotsShareNoMutableState", testKeySnapshotIsolation},
		{"TeamSnapshotsShareNoMutableState", testTeamSnapshotIsolation},
		{"CommitNotifications", testCommitNotifications},
		{"SubscriberMayReenter", testSubscriberReentry},
		{"AuditEvents", testAuditEvents},
		{"PersistFailureChangesNothing", testPersistFailure},
		{"CanceledContextChangesNothing", testCanceledContext},
		{"ClosedStoreRefusesWrites", testClosedStore},
		{"ConcurrentCreatesOfOneKey", testConcurrentCreates},
		{"ReopenRoundTrip", testReopenRoundTrip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t, newHarness(t))
		})
	}
}

type recorder struct {
	mu      sync.Mutex
	events  []audit.Event
	commits []access.Commit
}

func (r *recorder) Emit(_ context.Context, ev audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recorder) commit(c access.Commit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commits = append(r.commits, c)
}

func (r *recorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events), len(r.commits)
}

func (r *recorder) lastEvent() audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[len(r.events)-1]
}

func (r *recorder) lastCommit() access.Commit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commits[len(r.commits)-1]
}

// observe records every audit event and commit of s from now on.
func observe(s access.Store) *recorder {
	r := &recorder{}
	s.SetAuditSink(r)
	s.OnCommit(r.commit)
	return r
}

func event(action audit.Action, subject string) audit.Event {
	return audit.Event{Action: action, Actor: "contract", Subject: subject, Metadata: map[string]any{"subject": subject}}
}

func newKey(teamID string) keys.VirtualKey {
	return keys.VirtualKey{Name: "Reader", Role: "user", TeamID: teamID, TeamRole: string(teams.RoleMember)}
}

func sharedTeam() teams.Team { return teams.Team{Name: "Engineering", Kind: teams.KindShared} }

func createKey(t *testing.T, s access.Store, id string) string {
	t.Helper()
	_, raw, err := s.CreateKey(t.Context(), id, newKey("team"), audit.Event{})
	require.NoError(t, err)
	return raw
}

func createTeam(t *testing.T, s access.Store, id string, team teams.Team) {
	t.Helper()
	_, err := s.CreateTeam(t.Context(), id, team, audit.Event{})
	require.NoError(t, err)
}

func keyIDs(snap *access.Snapshot, teamID string) []string {
	var ids []string
	for id := range snap.KeysByTeam(teamID) {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func testKeyLifecycle(t *testing.T, h Harness) {
	s := h.Store
	input := newKey("team")
	input.KeyEnv = "IGNORED_ON_CREATE"
	created, raw, err := s.CreateKey(t.Context(), "reader", input, audit.Event{})
	require.NoError(t, err)
	assert.Contains(t, raw, keys.KeyPrefix)
	assert.Equal(t, "reader", created.ID)
	assert.Equal(t, "Reader", created.Name)
	assert.NotEmpty(t, created.UUID)
	assert.NotEmpty(t, created.HashedKey)
	assert.Empty(t, created.KeyEnv, "a created key is never env-backed")
	assert.False(t, created.CreatedAt.IsZero())
	assert.Equal(t, created.CreatedAt, created.UpdatedAt)
	assert.Equal(t, created, s.Snapshot().Key("reader"))

	id, matched := s.Snapshot().ValidateRawKey(raw)
	assert.Equal(t, "reader", id)
	require.NotNil(t, matched)
	id, matched = s.Snapshot().ValidateRawKey("zzr_wrong")
	assert.Empty(t, id)
	assert.Nil(t, matched)
	createKey(t, s, "writer")
	_, _, err = s.CreateKey(t.Context(), "other-team", newKey("other"), audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, []string{"reader", "writer"}, keyIDs(s.Snapshot(), "team"))
	assert.Empty(t, keyIDs(s.Snapshot(), ""))

	_, _, err = s.CreateKey(t.Context(), "reader", newKey("team"), audit.Event{})
	require.ErrorIs(t, err, keys.ErrKeyExists)

	_, err = s.UpdateKey(t.Context(), "missing", func(*keys.VirtualKey) error { return nil }, audit.Event{})
	require.ErrorIs(t, err, keys.ErrKeyNotFound)
	_, _, err = s.RotateKey(t.Context(), "missing", audit.Event{})
	require.ErrorIs(t, err, keys.ErrKeyNotFound)
	require.ErrorIs(t, s.DeleteKey(t.Context(), "missing", audit.Event{}), keys.ErrKeyNotFound)

	require.NoError(t, s.DeleteKey(t.Context(), "reader", audit.Event{}))
	assert.Nil(t, s.Snapshot().Key("reader"))
	id, _ = s.Snapshot().ValidateRawKey(raw)
	assert.Empty(t, id, "a deleted key stops authenticating")
	assert.Equal(t, []string{"writer"}, keyIDs(s.Snapshot(), "team"))
}

func testKeyStoreOwnedFields(t *testing.T, h Harness) {
	s := h.Store
	input := newKey("team")
	input.UUID, input.CreatedBy = "caller-uuid", "creator"
	created, _, err := s.CreateKey(t.Context(), "reader", input, audit.Event{})
	require.NoError(t, err)
	assert.NotEmpty(t, created.UUID)
	assert.NotEqual(t, "caller-uuid", created.UUID, "the store assigns the UUID")

	updated, err := s.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error {
		k.ID, k.UUID, k.HashedKey, k.KeyEnv = "moved", "other-uuid", "forged-hash", "FORGED_ENV"
		k.TeamID, k.TeamRole = "elsewhere", string(teams.RoleOwner)
		k.CreatedAt, k.CreatedBy, k.UpdatedAt = time.Time{}, "impostor", time.Time{}
		k.Name, k.Description, k.UpdatedBy = "Renamed", "agent: research", "editor"
		k.Role, k.RPMLimit = "admin", 7
		return nil
	}, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, "reader", updated.ID)
	assert.Equal(t, created.UUID, updated.UUID)
	assert.Equal(t, created.HashedKey, updated.HashedKey)
	assert.Empty(t, updated.KeyEnv)
	assert.Equal(t, "team", updated.TeamID)
	assert.Equal(t, string(teams.RoleMember), updated.TeamRole)
	assert.Equal(t, created.CreatedAt, updated.CreatedAt)
	assert.Equal(t, "creator", updated.CreatedBy)
	assert.False(t, updated.UpdatedAt.Before(created.CreatedAt), "every write stamps UpdatedAt")
	assert.Equal(t, "Renamed", updated.Name)
	assert.Equal(t, "agent: research", updated.Description)
	assert.Equal(t, "editor", updated.UpdatedBy)
	assert.Equal(t, "admin", updated.Role)
	assert.Equal(t, 7, updated.RPMLimit)
	assert.Equal(t, updated, s.Snapshot().Key("reader"))
	assert.Nil(t, s.Snapshot().Key("moved"))
}

func testKeyRotation(t *testing.T, h Harness) {
	s := h.Store
	created, oldRaw, err := s.CreateKey(t.Context(), "reader", newKey("team"), audit.Event{})
	require.NoError(t, err)
	before := s.Snapshot()
	rotated, newRaw, err := s.RotateKey(t.Context(), "reader", audit.Event{})
	require.NoError(t, err)
	assert.NotEqual(t, oldRaw, newRaw)
	assert.NotEqual(t, created.HashedKey, rotated.HashedKey)
	assert.Equal(t, created.UUID, rotated.UUID)
	assert.True(t, rotated.UpdatedAt.After(created.UpdatedAt), "rotation stamps UpdatedAt")
	id, _ := s.Snapshot().ValidateRawKey(oldRaw)
	assert.Empty(t, id, "the replaced key stops authenticating")
	id, _ = s.Snapshot().ValidateRawKey(newRaw)
	assert.Equal(t, "reader", id)
	id, _ = before.ValidateRawKey(oldRaw)
	assert.Equal(t, "reader", id, "an earlier snapshot never changes")
}

func testTeamLifecycle(t *testing.T, h Harness) {
	s := h.Store
	created, err := s.CreateTeam(t.Context(), "eng", teams.Team{UUID: "caller-uuid", Name: "Engineering", Kind: teams.KindShared, CreatedBy: "creator"}, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, "eng", created.ID)
	assert.NotEmpty(t, created.UUID)
	assert.NotEqual(t, "caller-uuid", created.UUID, "the store assigns the UUID")
	assert.False(t, created.CreatedAt.IsZero())
	assert.Equal(t, created.CreatedAt, created.UpdatedAt)
	assert.Equal(t, created, s.Snapshot().Team("eng"))
	_, err = s.CreateTeam(t.Context(), "eng", sharedTeam(), audit.Event{})
	require.ErrorIs(t, err, teams.ErrTeamExists)

	updated, err := s.UpdateTeam(t.Context(), "eng", func(team *teams.Team) error {
		team.ID, team.UUID, team.Kind = "moved", "other-uuid", teams.KindPersonal
		team.CreatedAt, team.CreatedBy, team.UpdatedAt = time.Time{}, "impostor", time.Time{}
		team.Name, team.AllowedModels, team.UpdatedBy = "Renamed", []string{"qwen"}, "editor"
		return nil
	}, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, "eng", updated.ID)
	assert.Equal(t, created.UUID, updated.UUID)
	assert.Equal(t, teams.KindShared, updated.Kind, "kind is immutable")
	assert.Equal(t, created.CreatedAt, updated.CreatedAt)
	assert.Equal(t, "creator", updated.CreatedBy)
	assert.False(t, updated.UpdatedAt.Before(created.CreatedAt), "every write stamps UpdatedAt")
	assert.Equal(t, "Renamed", updated.Name)
	assert.Equal(t, []string{"qwen"}, updated.AllowedModels)
	assert.Equal(t, "editor", updated.UpdatedBy)
	assert.Equal(t, updated, s.Snapshot().Team("eng"))

	_, err = s.UpdateTeam(t.Context(), "missing", func(*teams.Team) error { return nil }, audit.Event{})
	require.ErrorIs(t, err, teams.ErrTeamNotFound)
	require.ErrorIs(t, s.DeleteTeam(t.Context(), "missing", audit.Event{}), teams.ErrTeamNotFound)
	require.NoError(t, s.DeleteTeam(t.Context(), "eng", audit.Event{}))
	assert.Nil(t, s.Snapshot().Team("eng"))
}

// testInvalidRecords refuses, per operation, every record the store could not load again.
func testInvalidRecords(t *testing.T, h Harness) {
	s := h.Store
	createKey(t, s, "reader")
	createTeam(t, s, "eng", sharedTeam())
	rec := observe(s)
	key := func(apply func(*keys.VirtualKey)) keys.VirtualKey { k := newKey("team"); apply(&k); return k }
	team := func(apply func(*teams.Team)) teams.Team { team := sharedTeam(); apply(&team); return team }
	for name, run := range map[string]func() error{
		"create key without name": func() error {
			_, _, err := s.CreateKey(t.Context(), "bad", key(func(k *keys.VirtualKey) { k.Name = "" }), audit.Event{})
			return err
		},
		"create key without team": func() error {
			_, _, err := s.CreateKey(t.Context(), "bad", key(func(k *keys.VirtualKey) { k.TeamID = "" }), audit.Event{})
			return err
		},
		"create key without team role": func() error {
			_, _, err := s.CreateKey(t.Context(), "bad", key(func(k *keys.VirtualKey) { k.TeamRole = "" }), audit.Event{})
			return err
		},
		"update key to no name": func() error {
			_, err := s.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error { k.Name = ""; return nil }, audit.Event{})
			return err
		},
	} {
		require.ErrorIs(t, run(), keys.ErrInvalid, name)
	}
	for name, run := range map[string]func() error{
		"create team without kind": func() error {
			_, err := s.CreateTeam(t.Context(), "bad", team(func(team *teams.Team) { team.Kind = "" }), audit.Event{})
			return err
		},
		"create team of unknown kind": func() error {
			_, err := s.CreateTeam(t.Context(), "bad", team(func(team *teams.Team) { team.Kind = "bogus" }), audit.Event{})
			return err
		},
		"create team with invalid routes": func() error {
			_, err := s.CreateTeam(t.Context(), "bad", team(func(team *teams.Team) { team.RouteAccess = teams.RouteAccess{Mode: "selected"} }), audit.Event{})
			return err
		},
		"create team with invalid providers": func() error {
			_, err := s.CreateTeam(t.Context(), "bad", team(func(team *teams.Team) { team.ProviderAccess = teams.ProviderAccess{Mode: "selected"} }), audit.Event{})
			return err
		},
		"update team to invalid routes": func() error {
			_, err := s.UpdateTeam(t.Context(), "eng", func(team *teams.Team) error { team.RouteAccess = teams.RouteAccess{Mode: "select"}; return nil }, audit.Event{})
			return err
		},
		"update team to invalid providers": func() error {
			_, err := s.UpdateTeam(t.Context(), "eng", func(team *teams.Team) error { team.ProviderAccess = teams.ProviderAccess{Mode: "selected"}; return nil }, audit.Event{})
			return err
		},
	} {
		require.ErrorIs(t, run(), teams.ErrInvalid, name)
	}
	events, commits := rec.counts()
	assert.Zero(t, events)
	assert.Zero(t, commits, "a refused record publishes nothing")
	for _, snap := range []*access.Snapshot{s.Snapshot(), h.Reopen().Snapshot()} {
		assert.Nil(t, snap.Key("bad"))
		assert.Nil(t, snap.Team("bad"))
		assert.Equal(t, "Reader", snap.Key("reader").Name)
		assert.Equal(t, sharedTeam().Name, snap.Team("eng").Name)
		assert.False(t, snap.Team("eng").RouteAccess.Restricted())
		assert.False(t, snap.Team("eng").ProviderAccess.Restricted())
	}
}

// testConcurrentUpdates runs every apply on the record the previous one committed.
func testConcurrentUpdates(t *testing.T, h Harness) {
	s := h.Store
	createKey(t, s, "reader")
	createTeam(t, s, "eng", sharedTeam())
	const writers = 8
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			_, err := s.UpdateKey(context.Background(), "reader", func(k *keys.VirtualKey) error { k.RPMLimit++; return nil }, audit.Event{})
			assert.NoError(t, err)
		})
		wg.Go(func() {
			_, err := s.UpdateTeam(context.Background(), "eng", func(team *teams.Team) error { team.RPMLimit++; return nil }, audit.Event{})
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	assert.Equal(t, writers, s.Snapshot().Key("reader").RPMLimit)
	assert.Equal(t, writers, s.Snapshot().Team("eng").RPMLimit)
}

func testApplyError(t *testing.T, h Harness) {
	s := h.Store
	createKey(t, s, "reader")
	createTeam(t, s, "eng", sharedTeam())
	rec := observe(s)
	refused := errors.New("refused by the caller")
	_, err := s.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error {
		k.Name = "half applied"
		return refused
	}, event(audit.ActionKeyUpdated, "reader"))
	require.Equal(t, refused, err)
	_, err = s.UpdateTeam(t.Context(), "eng", func(team *teams.Team) error {
		team.Name = "half applied"
		return refused
	}, event(audit.ActionTeamUpdated, "eng"))
	require.Equal(t, refused, err)
	assert.Equal(t, "Reader", s.Snapshot().Key("reader").Name)
	assert.Equal(t, "Engineering", s.Snapshot().Team("eng").Name)
	events, commits := rec.counts()
	assert.Zero(t, events)
	assert.Zero(t, commits)
}

func testHasGatedTeam(t *testing.T, h Harness) {
	s := h.Store
	assert.False(t, s.Snapshot().HasGatedTeam())
	createTeam(t, s, "open", sharedTeam())
	assert.False(t, s.Snapshot().HasGatedTeam())
	models := sharedTeam()
	models.AllowedModels = []string{"qwen"}
	createTeam(t, s, "models", models)
	assert.True(t, s.Snapshot().HasGatedTeam())
	require.NoError(t, s.DeleteTeam(t.Context(), "models", audit.Event{}))
	assert.False(t, s.Snapshot().HasGatedTeam(), "deleting the only gated team clears the gate")
	for _, apply := range []func(*teams.Team){
		func(team *teams.Team) {
			team.ProviderAccess = teams.ProviderAccess{Mode: "selected", Providers: []string{"ollama"}}
		},
		func(team *teams.Team) { team.ProviderAccess = teams.ProviderAccess{Mode: "none"} },
		func(team *teams.Team) { team.RouteAccess = teams.RouteAccess{Mode: "none"} },
		func(team *teams.Team) {
			team.RouteAccess = teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}}
		},
	} {
		_, err := s.UpdateTeam(t.Context(), "open", func(team *teams.Team) error { apply(team); return nil }, audit.Event{})
		require.NoError(t, err)
		assert.True(t, s.Snapshot().HasGatedTeam())
		_, err = s.UpdateTeam(t.Context(), "open", func(team *teams.Team) error {
			team.ProviderAccess, team.RouteAccess = teams.ProviderAccess{Mode: "all"}, teams.RouteAccess{Mode: "direct"}
			return nil
		}, audit.Event{})
		require.NoError(t, err)
		assert.False(t, s.Snapshot().HasGatedTeam())
	}
}

func testHasGatedTeamDuringBlockedUpdate(t *testing.T, h Harness) {
	s := h.Store
	createTeam(t, s, "eng", sharedTeam())
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.UpdateTeam(context.Background(), "eng", func(team *teams.Team) error {
			close(entered)
			<-release
			team.AllowedModels = []string{"qwen"}
			return nil
		}, audit.Event{})
		done <- err
	}()
	<-entered
	answered := make(chan bool, 1)
	go func() { answered <- s.Snapshot().HasGatedTeam() }()
	select {
	case gated := <-answered:
		assert.False(t, gated, "the blocked update is not published yet")
	case <-time.After(10 * time.Second):
		t.Fatal("HasGatedTeam waited for the writer gate")
	}
	close(release)
	require.NoError(t, <-done)
	assert.True(t, s.Snapshot().HasGatedTeam())
}

func testKeySnapshotIsolation(t *testing.T, h Harness) {
	s := h.Store
	expires, suspended := utils.NowUTC().Add(time.Hour), utils.NowUTC()
	inputExpires, inputSuspended := expires, suspended
	input := newKey("team")
	input.ExpiresAt, input.SuspendedAt, input.Metadata = &inputExpires, &inputSuspended, map[string]string{"label": "original"}
	created, raw, err := s.CreateKey(t.Context(), "reader", input, audit.Event{})
	require.NoError(t, err)
	input.Name, input.Metadata["label"] = "edited input", "edited input"
	*input.ExpiresAt = time.Time{}

	_, validated := s.Snapshot().ValidateRawKey(raw)
	var listed, byTeam *keys.VirtualKey
	for _, key := range s.Snapshot().Keys() {
		listed = key
	}
	for _, key := range s.Snapshot().KeysByTeam("team") {
		byTeam = key
	}
	for _, key := range []*keys.VirtualKey{created, s.Snapshot().Key("reader"), validated, listed, byTeam} {
		require.Equal(t, "Reader", key.Name)
		require.Equal(t, "original", key.Metadata["label"])
		key.Name, key.Metadata["label"] = "outside edit", "outside edit"
		*key.ExpiresAt, *key.SuspendedAt = time.Time{}, time.Time{}
	}
	stored := s.Snapshot().Key("reader")
	assert.Equal(t, "Reader", stored.Name)
	assert.Equal(t, "original", stored.Metadata["label"])
	assert.True(t, expires.Equal(*stored.ExpiresAt))
	assert.True(t, suspended.Equal(*stored.SuspendedAt))

	before := s.Snapshot()
	metadata := map[string]string{"label": "applied"}
	_, err = s.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error {
		k.Metadata["label"] = "edited in place"
		k.Metadata = metadata
		return nil
	}, audit.Event{})
	require.NoError(t, err)
	metadata["label"] = "edited after the update"
	assert.Equal(t, "original", before.Key("reader").Metadata["label"], "an earlier snapshot never changes")
	assert.Equal(t, "applied", s.Snapshot().Key("reader").Metadata["label"])
}

func testTeamSnapshotIsolation(t *testing.T, h Harness) {
	s := h.Store
	suspended := utils.NowUTC()
	inputSuspended := suspended
	input := teams.Team{
		Name: "Engineering", Kind: teams.KindShared, AllowedModels: []string{"qwen"}, SuspendedAt: &inputSuspended,
		RouteAccess:    teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}},
		ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"ollama"}},
		Metadata:       map[string]string{"label": "original"},
	}
	created, err := s.CreateTeam(t.Context(), "eng", input, audit.Event{})
	require.NoError(t, err)
	edit := func(team *teams.Team) {
		team.Name, team.AllowedModels[0], team.Metadata["label"] = "outside edit", "outside", "outside edit"
		team.RouteAccess.Routes[0], team.ProviderAccess.Providers[0] = "outside", "outside"
		*team.SuspendedAt = time.Time{}
	}
	edit(&input)
	var listed *teams.Team
	for _, team := range s.Snapshot().Teams() {
		listed = team
	}
	for _, team := range []*teams.Team{created, s.Snapshot().Team("eng"), listed} {
		require.Equal(t, "Engineering", team.Name)
		edit(team)
	}
	stored := s.Snapshot().Team("eng")
	assert.Equal(t, "Engineering", stored.Name)
	assert.Equal(t, []string{"qwen"}, stored.AllowedModels)
	assert.Equal(t, []string{"coding"}, stored.RouteAccess.Routes)
	assert.Equal(t, []string{"ollama"}, stored.ProviderAccess.Providers)
	assert.Equal(t, "original", stored.Metadata["label"])
	assert.True(t, suspended.Equal(*stored.SuspendedAt))

	before := s.Snapshot()
	updated, err := s.UpdateTeam(t.Context(), "eng", func(team *teams.Team) error {
		team.AllowedModels[0] = "edited in place"
		return nil
	}, audit.Event{})
	require.NoError(t, err)
	edit(updated)
	assert.Equal(t, []string{"qwen"}, before.Team("eng").AllowedModels, "an earlier snapshot never changes")
	assert.Equal(t, []string{"edited in place"}, s.Snapshot().Team("eng").AllowedModels)
}

func testCommitNotifications(t *testing.T, h Harness) {
	s := h.Store
	rec, second := observe(s), &recorder{}
	s.OnCommit(second.commit)
	_, raw, err := s.CreateKey(t.Context(), "reader", newKey("team"), audit.Event{})
	require.NoError(t, err)
	c := rec.lastCommit()
	assert.True(t, c.KeysChanged)
	assert.NotNil(t, s.Snapshot().Key("reader"))
	_, err = s.UpdateKey(t.Context(), "reader", func(*keys.VirtualKey) error { return nil }, audit.Event{})
	require.NoError(t, err)
	c = rec.lastCommit()
	assert.True(t, c.KeysChanged)
	_, _, err = s.RotateKey(t.Context(), "reader", audit.Event{})
	require.NoError(t, err)
	c = rec.lastCommit()
	assert.True(t, c.KeysChanged)
	id, _ := s.Snapshot().ValidateRawKey(raw)
	assert.Empty(t, id)
	createTeam(t, s, "eng", sharedTeam())
	c = rec.lastCommit()
	assert.False(t, c.KeysChanged, "a team write leaves key caches alone")
	_, err = s.UpdateTeam(t.Context(), "eng", func(*teams.Team) error { return nil }, audit.Event{})
	require.NoError(t, err)
	c = rec.lastCommit()
	assert.False(t, c.KeysChanged)
	require.NoError(t, s.DeleteKey(t.Context(), "reader", audit.Event{}))
	c = rec.lastCommit()
	assert.True(t, c.KeysChanged)
	assert.Equal(t, []string{"reader"}, c.RemovedKeys)
	assert.Empty(t, c.RemovedTeams)
	require.NoError(t, s.DeleteTeam(t.Context(), "eng", audit.Event{}))
	c = rec.lastCommit()
	assert.False(t, c.KeysChanged)
	assert.Equal(t, []string{"eng"}, c.RemovedTeams)
	assert.Empty(t, c.RemovedKeys)

	require.ErrorIs(t, s.DeleteTeam(t.Context(), "eng", audit.Event{}), teams.ErrTeamNotFound)
	_, commits := rec.counts()
	assert.Equal(t, 7, commits, "one notification per committed operation, none for a refusal")
	_, otherCommits := second.counts()
	assert.Equal(t, commits, otherCommits, "every subscriber is notified")
}

func testSubscriberReentry(t *testing.T, h Harness) {
	s := h.Store
	var once sync.Once
	reentered := make(chan error, 1)
	s.OnCommit(func(c access.Commit) {
		if !c.KeysChanged {
			return
		}
		once.Do(func() {
			_ = s.Snapshot().Key("reader")
			_, err := s.CreateTeam(context.Background(), "from-subscriber", sharedTeam(), audit.Event{})
			reentered <- err
		})
	})
	createKey(t, s, "reader")
	require.NoError(t, <-reentered)
	assert.NotNil(t, s.Snapshot().Team("from-subscriber"))
}

func testAuditEvents(t *testing.T, h Harness) {
	s := h.Store
	rec := observe(s)
	steps := []struct {
		ev  audit.Event
		run func(audit.Event) error
	}{
		{event(audit.ActionTeamCreated, "eng"), func(ev audit.Event) error { _, err := s.CreateTeam(t.Context(), "eng", sharedTeam(), ev); return err }},
		{event(audit.ActionTeamUpdated, "eng"), func(ev audit.Event) error {
			_, err := s.UpdateTeam(t.Context(), "eng", func(*teams.Team) error { return nil }, ev)
			return err
		}},
		{event(audit.ActionKeyCreated, "reader"), func(ev audit.Event) error {
			_, _, err := s.CreateKey(t.Context(), "reader", newKey("eng"), ev)
			return err
		}},
		{event(audit.ActionKeyUpdated, "reader"), func(ev audit.Event) error {
			_, err := s.UpdateKey(t.Context(), "reader", func(*keys.VirtualKey) error { return nil }, ev)
			return err
		}},
		{event(audit.ActionKeyRotated, "reader"), func(ev audit.Event) error { _, _, err := s.RotateKey(t.Context(), "reader", ev); return err }},
		{event(audit.ActionKeyDeleted, "reader"), func(ev audit.Event) error { return s.DeleteKey(t.Context(), "reader", ev) }},
		{event(audit.ActionTeamDeleted, "eng"), func(ev audit.Event) error { return s.DeleteTeam(t.Context(), "eng", ev) }},
	}
	for _, step := range steps {
		require.NoError(t, step.run(step.ev))
		ev := rec.lastEvent()
		assert.Equal(t, step.ev.Action, ev.Action)
		assert.Equal(t, step.ev.Actor, ev.Actor)
		assert.Equal(t, step.ev.Subject, ev.Subject)
		assert.Equal(t, step.ev.Metadata, ev.Metadata)
	}
	createTeam(t, s, "silent", sharedTeam())
	events, commits := rec.counts()
	assert.Equal(t, len(steps), events, "a zero event records nothing")
	assert.Equal(t, len(steps)+1, commits)

	s.Record(t.Context(), event(audit.ActionKeyUsageReset, "reader"))
	ev := rec.lastEvent()
	assert.Equal(t, audit.ActionKeyUsageReset, ev.Action)
	s.Record(t.Context(), audit.Event{})
	events, _ = rec.counts()
	assert.Equal(t, len(steps)+1, events)
}

func testPersistFailure(t *testing.T, h Harness) {
	s := h.Store
	_, oldRaw, err := s.CreateKey(t.Context(), "reader", newKey("eng"), audit.Event{})
	require.NoError(t, err)
	createTeam(t, s, "eng", sharedTeam())
	rec := observe(s)
	for name, run := range map[string]func() error{
		"create key": func() error {
			_, _, err := s.CreateKey(t.Context(), "writer", newKey("eng"), event(audit.ActionKeyCreated, "writer"))
			return err
		},
		"update key": func() error {
			_, err := s.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error { k.Name = "lost"; return nil }, event(audit.ActionKeyUpdated, "reader"))
			return err
		},
		"rotate key": func() error {
			_, _, err := s.RotateKey(t.Context(), "reader", event(audit.ActionKeyRotated, "reader"))
			return err
		},
		"delete key": func() error { return s.DeleteKey(t.Context(), "reader", event(audit.ActionKeyDeleted, "reader")) },
		"create team": func() error {
			_, err := s.CreateTeam(t.Context(), "ops", sharedTeam(), event(audit.ActionTeamCreated, "ops"))
			return err
		},
		"update team": func() error {
			_, err := s.UpdateTeam(t.Context(), "eng", func(team *teams.Team) error { team.Suspended = true; return nil }, event(audit.ActionTeamUpdated, "eng"))
			return err
		},
		"delete team": func() error { return s.DeleteTeam(t.Context(), "eng", event(audit.ActionTeamDeleted, "eng")) },
	} {
		h.FailNextCommit()
		require.Error(t, run(), name)
	}
	events, commits := rec.counts()
	assert.Zero(t, events, "a failed persist records no audit event")
	assert.Zero(t, commits, "a failed persist publishes nothing")
	unchanged := func(snap *access.Snapshot) {
		assert.Equal(t, "Reader", snap.Key("reader").Name)
		id, _ := snap.ValidateRawKey(oldRaw)
		assert.Equal(t, "reader", id, "a failed rotation keeps the old key")
		assert.Nil(t, snap.Key("writer"))
		assert.False(t, snap.Team("eng").Suspended)
		assert.Nil(t, snap.Team("ops"))
	}
	unchanged(s.Snapshot())
	unchanged(h.Reopen().Snapshot())
}

func testCanceledContext(t *testing.T, h Harness) {
	s := h.Store
	createKey(t, s, "reader")
	createTeam(t, s, "eng", sharedTeam())
	rec := observe(s)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := s.CreateKey(ctx, "writer", newKey("eng"), event(audit.ActionKeyCreated, "writer"))
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.UpdateTeam(ctx, "eng", func(team *teams.Team) error { team.Suspended = true; return nil }, event(audit.ActionTeamUpdated, "eng"))
	require.ErrorIs(t, err, context.Canceled)

	// A request that ends while its change is being applied changes nothing either.
	inside, cancelInside := context.WithCancel(t.Context())
	_, err = s.UpdateKey(inside, "reader", func(k *keys.VirtualKey) error { k.Name = "lost"; cancelInside(); return nil }, event(audit.ActionKeyUpdated, "reader"))
	require.ErrorIs(t, err, context.Canceled)
	inside, cancelInside = context.WithCancel(t.Context())
	_, err = s.UpdateTeam(inside, "eng", func(team *teams.Team) error { team.Suspended = true; cancelInside(); return nil }, event(audit.ActionTeamUpdated, "eng"))
	require.ErrorIs(t, err, context.Canceled)

	events, commits := rec.counts()
	assert.Zero(t, events)
	assert.Zero(t, commits)
	for _, snap := range []*access.Snapshot{s.Snapshot(), h.Reopen().Snapshot()} {
		assert.Nil(t, snap.Key("writer"))
		assert.Equal(t, "Reader", snap.Key("reader").Name)
		assert.False(t, snap.Team("eng").Suspended)
	}
}

func testClosedStore(t *testing.T, h Harness) {
	s := h.Store
	_, oldRaw, err := s.CreateKey(t.Context(), "reader", newKey("eng"), audit.Event{})
	require.NoError(t, err)
	createTeam(t, s, "eng", sharedTeam())
	rec := observe(s)
	last := s.Snapshot()
	require.NoError(t, s.Close())
	for name, run := range map[string]func() error{
		"create key": func() error {
			_, _, err := s.CreateKey(t.Context(), "writer", newKey("eng"), event(audit.ActionKeyCreated, "writer"))
			return err
		},
		"update key": func() error {
			_, err := s.UpdateKey(t.Context(), "reader", func(*keys.VirtualKey) error { return nil }, audit.Event{})
			return err
		},
		"rotate key":  func() error { _, _, err := s.RotateKey(t.Context(), "reader", audit.Event{}); return err },
		"delete key":  func() error { return s.DeleteKey(t.Context(), "reader", audit.Event{}) },
		"create team": func() error { _, err := s.CreateTeam(t.Context(), "ops", sharedTeam(), audit.Event{}); return err },
		"update team": func() error {
			_, err := s.UpdateTeam(t.Context(), "eng", func(*teams.Team) error { return nil }, audit.Event{})
			return err
		},
		"delete team": func() error { return s.DeleteTeam(t.Context(), "eng", audit.Event{}) },
	} {
		require.ErrorIs(t, run(), access.ErrClosed, name)
	}
	s.Record(t.Context(), event(audit.ActionKeyUsageReset, "reader"))
	events, commits := rec.counts()
	assert.Zero(t, events, "a closed store records nothing")
	assert.Zero(t, commits)
	assert.Same(t, last, s.Snapshot(), "a closed store keeps its last published snapshot")
	snap := h.Reopen().Snapshot()
	id, _ := snap.ValidateRawKey(oldRaw)
	assert.Equal(t, "reader", id)
	assert.Nil(t, snap.Key("writer"))
	assert.NotNil(t, snap.Team("eng"))
	assert.Nil(t, snap.Team("ops"))
}

func testConcurrentCreates(t *testing.T, h Harness) {
	s := h.Store
	rec := observe(s)
	const callers = 4
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			<-start
			_, _, err := s.CreateKey(t.Context(), "reader", newKey("team"), audit.Event{})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		require.ErrorIs(t, err, keys.ErrKeyExists)
	}
	assert.Equal(t, 1, winners)
	_, commits := rec.counts()
	assert.Equal(t, 1, commits)
}

// fullKey and fullTeam set every field a caller may set, so a field a store
// forgets fails the round trip; storeOwned lists what the store sets itself.
func fullKey(teamID string) keys.VirtualKey {
	expires, suspended := utils.NowUTC().Add(time.Hour), utils.NowUTC()
	return keys.VirtualKey{
		Name: "Reader", Description: "agent: research-bot", Role: "admin", TeamID: teamID, TeamRole: string(teams.RoleOwner),
		Suspended: true, SuspendedAt: &suspended, SuspendedBy: "suspender", ExpiresAt: &expires,
		CreatedBy: "creator", UpdatedBy: "updater", Metadata: map[string]string{"env": "ci"},
		QuotaConfig: quota.QuotaConfig{RPMLimit: 10, TPMLimit: 20, MaxParallelRequests: 3, SpendLimit: 1.5, ResetPeriod: "monthly", DefaultMaxTokens: 256},
	}
}

func fullTeam() teams.Team {
	suspended := utils.NowUTC()
	return teams.Team{
		Name: "Engineering", Kind: teams.KindShared, AllowedModels: []string{"qwen"}, Metadata: map[string]string{"dept": "r&d"},
		RouteAccess:    teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}},
		ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"ollama"}},
		Suspended:      true, SuspendedAt: &suspended, SuspendedBy: "suspender", CreatedBy: "creator", UpdatedBy: "updater",
		QuotaConfig: quota.QuotaConfig{RPMLimit: 100, TPMLimit: 200, MaxParallelRequests: 4, SpendLimit: 50, ResetPeriod: "daily", DefaultMaxTokens: 512},
	}
}

// requireAllSet fails for every exported field left zero, except those named;
// KeyEnv excludes HashedKey and nothing stores LastSeenAt yet.
func requireAllSet(t *testing.T, record any, except ...string) {
	t.Helper()
	var walk func(reflect.Value, string)
	walk = func(v reflect.Value, path string) {
		for i := range v.NumField() {
			field, value := v.Type().Field(i), v.Field(i)
			name := path + field.Name
			switch {
			case !field.IsExported() || slices.Contains(except, field.Name):
			case value.Kind() == reflect.Struct && value.Type() != reflect.TypeFor[time.Time]():
				walk(value, name+".")
			case value.IsZero():
				t.Errorf("%s is zero: set it so the round trip covers it", name)
			}
		}
	}
	walk(reflect.ValueOf(record).Elem(), "")
}

// seconds drops what a store may not keep below second precision.
func seconds[T any](record *T) *T {
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		for i := range v.NumField() {
			switch field := v.Field(i); {
			case !field.CanSet():
			case field.Type() == reflect.TypeFor[time.Time]():
				at, _ := field.Interface().(time.Time)
				field.Set(reflect.ValueOf(at.Truncate(time.Second)))
			case field.Type() == reflect.TypeFor[*time.Time]() && !field.IsNil():
				at, _ := field.Elem().Interface().(time.Time)
				truncated := at.Truncate(time.Second)
				field.Set(reflect.ValueOf(&truncated))
			case field.Kind() == reflect.Struct:
				walk(field)
			}
		}
	}
	walk(reflect.ValueOf(record).Elem())
	return record
}

func testReopenRoundTrip(t *testing.T, h Harness) {
	s := h.Store
	key, raw, err := s.CreateKey(t.Context(), "reader", fullKey("eng"), audit.Event{})
	require.NoError(t, err)
	team, err := s.CreateTeam(t.Context(), "eng", fullTeam(), audit.Event{})
	require.NoError(t, err)
	requireAllSet(t, key, "KeyEnv", "LastSeenAt")
	requireAllSet(t, team)
	reopen := func(wantKey *keys.VirtualKey, wantTeam *teams.Team) access.Store {
		t.Helper()
		reopened := h.Reopen()
		gotKey, gotTeam := reopened.Snapshot().Key("reader"), reopened.Snapshot().Team("eng")
		require.NotNil(t, gotKey)
		require.NotNil(t, gotTeam)
		assert.Equal(t, seconds(wantKey.Clone()), seconds(gotKey))
		assert.Equal(t, seconds(wantTeam.Clone()), seconds(gotTeam))
		return reopened
	}
	s = reopen(key, team)
	id, _ := s.Snapshot().ValidateRawKey(raw)
	assert.Equal(t, "reader", id)
	assert.True(t, s.Snapshot().HasGatedTeam())

	key, err = s.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error {
		k.Name, k.Role, k.Suspended, k.SuspendedAt, k.Metadata = "Renamed", "user", false, nil, map[string]string{"env": "prod"}
		k.RPMLimit, k.ResetPeriod = 11, "weekly"
		return nil
	}, audit.Event{})
	require.NoError(t, err)
	team, err = s.UpdateTeam(t.Context(), "eng", func(team *teams.Team) error {
		team.Name, team.AllowedModels, team.Suspended, team.SuspendedAt = "Platform", []string{"llama"}, false, nil
		team.RouteAccess, team.ProviderAccess = teams.RouteAccess{Mode: "none"}, teams.ProviderAccess{Mode: "all"}
		return nil
	}, audit.Event{})
	require.NoError(t, err)
	s = reopen(key, team)

	key, newRaw, err := s.RotateKey(t.Context(), "reader", audit.Event{})
	require.NoError(t, err)
	s = reopen(key, team)
	id, _ = s.Snapshot().ValidateRawKey(newRaw)
	assert.Equal(t, "reader", id)
	id, _ = s.Snapshot().ValidateRawKey(raw)
	assert.Empty(t, id)

	require.NoError(t, s.DeleteKey(t.Context(), "reader", audit.Event{}))
	require.NoError(t, s.DeleteTeam(t.Context(), "eng", audit.Event{}))
	snap := h.Reopen().Snapshot()
	assert.Nil(t, snap.Key("reader"))
	assert.Nil(t, snap.Team("eng"))
}
