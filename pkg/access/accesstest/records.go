package accesstest

import (
	"context"
	"fmt"
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

// testKeyWithPersonalTeam creates and deletes a key with its personal team in
// one commit each, both events recorded with the key event's actor and request.
func testKeyWithPersonalTeam(t *testing.T, h Harness) {
	s := h.Store
	rec := observe(t, h)
	ctx := audit.WithIdentity(t.Context(), "operator", "request-1")
	input := newKey("")
	input.TeamRole, input.CreatedBy = "", "creator"
	created, raw, err := s.CreateKey(ctx, "solo", input, audit.Event{Action: audit.ActionKeyCreated, Subject: "solo", Metadata: map[string]any{"role": "user"}})
	require.NoError(t, err)
	personal := teams.PersonalID("solo")
	assert.Equal(t, personal, created.TeamID)
	assert.Equal(t, string(teams.RoleOwner), created.TeamRole)
	snap := s.Snapshot()
	assert.Equal(t, created, snap.Key("solo"))
	team := snap.Team(personal)
	require.NotNil(t, team)
	assert.Equal(t, teams.KindPersonal, team.Kind)
	assert.Equal(t, "Personal: Reader", team.Name)
	assert.Equal(t, "creator", team.CreatedBy)
	assert.Equal(t, "creator", team.UpdatedBy)
	assert.Equal(t, created.CreatedAt, team.CreatedAt)
	assert.Equal(t, []string{"solo"}, keyIDs(snap, personal))
	events, commits := rec.counts()
	assert.Equal(t, 1, commits, "the key and its team commit together")
	require.Equal(t, 2, events)
	history := auditEvents(t, s)
	teamEvent, keyEvent := history[len(history)-2], history[len(history)-1]
	assert.Equal(t, audit.Event{Time: teamEvent.Time, Action: audit.ActionTeamCreated, Actor: "operator", Subject: personal, RequestID: "request-1",
		Metadata: map[string]any{"kind": "personal", "key_name": "Reader"}}, teamEvent)
	assert.Equal(t, audit.ActionKeyCreated, keyEvent.Action)
	assert.Equal(t, "operator", keyEvent.Actor)
	assert.Equal(t, "request-1", keyEvent.RequestID)

	reopened := h.Reopen()
	assert.Equal(t, snap.Key("solo"), reopened.Snapshot().Key("solo"), "a reopen reads what was published")
	assert.Equal(t, team, reopened.Snapshot().Team(personal))
	s = reopened
	rec = observe(t, Harness{Store: s})
	require.NoError(t, s.DeleteKey(ctx, "solo", event(audit.ActionKeyDeleted, "solo")))
	snap = s.Snapshot()
	assert.Nil(t, snap.Key("solo"))
	assert.Nil(t, snap.Team(personal), "the personal team goes with its key")
	id, _ := snap.ValidateRawKey(raw)
	assert.Empty(t, id)
	c := rec.lastCommit()
	assert.Equal(t, []string{"solo"}, c.RemovedKeys)
	assert.Equal(t, []string{personal}, c.RemovedTeams)
	events, commits = rec.counts()
	assert.Equal(t, 1, commits)
	require.Equal(t, 2, events)
	history = auditEvents(t, s)
	keyEvent, teamEvent = history[len(history)-2], history[len(history)-1]
	assert.Equal(t, audit.ActionKeyDeleted, keyEvent.Action)
	assert.Equal(t, audit.Event{Time: teamEvent.Time, Action: audit.ActionTeamDeleted, Actor: "contract", Subject: personal, RequestID: "request-1",
		Metadata: map[string]any{"kind": "personal", "cascade": true}}, teamEvent)
	s = h.Reopen()
	assert.Nil(t, s.Snapshot().Team(personal))

	// A shared team stays when its key goes.
	createTeam(t, s, "eng", sharedTeam())
	_, _, err = s.CreateKey(t.Context(), "member", newKey("eng"), audit.Event{})
	require.NoError(t, err)
	require.NoError(t, s.DeleteKey(t.Context(), "member", audit.Event{}))
	assert.NotNil(t, s.Snapshot().Team("eng"))
	_, _, err = s.CreateKey(t.Context(), "solo", newKey(""), audit.Event{})
	require.NoError(t, err, "the ID and its personal team can be created again")
}

// testRecordRules refuses every state a rule between records forbids.
func testRecordRules(t *testing.T, h Harness) {
	s := h.Store
	createKey(t, s, "solo")
	personal := teams.PersonalID("solo")
	claimOwner(t, s)
	member := invite(t, s, "alice")
	rec := observe(t, h)
	joining := func(teamID, role string) keys.VirtualKey {
		k := newKey(teamID)
		k.TeamRole = role
		return k
	}
	for name, tc := range map[string]struct {
		run  func() error
		want []error
	}{
		"key in a missing team": {func() error {
			_, _, err := s.CreateKey(t.Context(), "lost", newKey("missing"), audit.Event{})
			return err
		}, []error{keys.ErrInvalid, teams.ErrTeamNotFound}},
		"second key in a personal team": {func() error {
			_, _, err := s.CreateKey(t.Context(), "second", joining(personal, string(teams.RoleMember)), audit.Event{})
			return err
		}, []error{keys.ErrInvalid, teams.ErrPersonalTeamImmutable}},
		"second owner of a personal team": {func() error {
			_, _, err := s.CreateKey(t.Context(), "second", joining(personal, string(teams.RoleOwner)), audit.Event{})
			return err
		}, []error{keys.ErrInvalid, teams.ErrPersonalTeamImmutable}},
		"key claiming a personal team that is not its own": {func() error {
			_, _, err := s.CreateKey(t.Context(), "other", joining(teams.PersonalID("missing"), string(teams.RoleOwner)), audit.Event{})
			return err
		}, []error{keys.ErrInvalid, teams.ErrTeamNotFound}},
		"person in a personal team": {func() error {
			_, err := s.People().Update(t.Context(), member.ID, people.UpdatePerson{TeamID: &personal})
			return err
		}, []error{people.ErrInvalidGrant}},
		"person in a missing team": {func() error {
			missing := "missing"
			_, err := s.People().Update(t.Context(), member.ID, people.UpdatePerson{TeamID: &missing})
			return err
		}, []error{people.ErrInvalidGrant}},
	} {
		err := tc.run()
		for _, want := range tc.want {
			require.ErrorIs(t, err, want, name)
		}
	}
	events, commits := rec.counts()
	assert.Zero(t, events)
	assert.Zero(t, commits, "a refused state publishes nothing")
	snap := h.Reopen().Snapshot()
	assert.Equal(t, []string{"solo"}, keyIDs(snap, personal))
	assert.Equal(t, "eng", snap.Person(member.ID).TeamID)
}

// testTeamRefusals keeps a team that anything depends on, and every personal team.
func testTeamRefusals(t *testing.T, h Harness) {
	s := h.Store
	owner := claimOwner(t, s)
	createKey(t, s, "solo")
	personal := teams.PersonalID("solo")
	createTeam(t, s, "keys", sharedTeam())
	_, _, err := s.CreateKey(t.Context(), "member", newKey("keys"), audit.Event{})
	require.NoError(t, err)
	member := invite(t, s, "alice")
	createTeam(t, s, "invited", sharedTeam())
	pending, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "invited", Role: "user", Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, err)
	rec := observe(t, h)
	for name, tc := range map[string]struct {
		run  func() error
		want error
	}{
		"delete a missing team":            {func() error { return s.DeleteTeam(t.Context(), "missing", audit.Event{}) }, teams.ErrTeamNotFound},
		"delete a personal team":           {func() error { return s.DeleteTeam(t.Context(), personal, audit.Event{}) }, teams.ErrPersonalTeamImmutable},
		"delete a team with a key":         {func() error { return s.DeleteTeam(t.Context(), "keys", audit.Event{}) }, teams.ErrTeamHasMembers},
		"delete a team with a person":      {func() error { return s.DeleteTeam(t.Context(), member.TeamID, audit.Event{}) }, teams.ErrTeamHasMembers},
		"delete the owner's workspace":     {func() error { return s.DeleteTeam(t.Context(), owner.TeamID, audit.Event{}) }, teams.ErrTeamHasMembers},
		"delete a team with an invitation": {func() error { return s.DeleteTeam(t.Context(), "invited", audit.Event{}) }, teams.ErrTeamHasMembers},
		"update a personal team":           {func() error { _, err := s.UpdateTeam(t.Context(), personal, rename, audit.Event{}); return err }, teams.ErrPersonalTeamImmutable},
		"suspend the owner's workspace":    {func() error { _, err := s.UpdateTeam(t.Context(), owner.TeamID, suspend, audit.Event{}); return err }, people.ErrOwnerProtected},
		"update a missing team":            {func() error { _, err := s.UpdateTeam(t.Context(), "missing", rename, audit.Event{}); return err }, teams.ErrTeamNotFound},
	} {
		require.ErrorIs(t, tc.run(), tc.want, name)
	}
	events, commits := rec.counts()
	assert.Zero(t, events)
	assert.Zero(t, commits, "a refusal publishes nothing")
	_, err = s.UpdateTeam(t.Context(), owner.TeamID, rename, audit.Event{})
	require.NoError(t, err, "the owner's workspace takes other changes")
	require.NoError(t, s.People().RevokeGrant(t.Context(), pending.ID))
	require.NoError(t, s.DeleteTeam(t.Context(), "invited", audit.Event{}), "a revoked invitation no longer holds its team")
	require.NoError(t, s.DeleteKey(t.Context(), "member", audit.Event{}))
	require.NoError(t, s.DeleteTeam(t.Context(), "keys", audit.Event{}))
}

func rename(team *teams.Team) error  { team.Name = "Renamed"; return nil }
func suspend(team *teams.Team) error { team.Suspended = true; return nil }

func testMembership(t *testing.T, h Harness) {
	s := h.Store
	owner := claimOwner(t, s)
	createKey(t, s, "solo")
	member := invite(t, s, "alice")
	_, _, err := s.CreateKey(t.Context(), "app", newKey(member.TeamID), audit.Event{})
	require.NoError(t, err)
	_, err = s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: member.TeamID, Role: "user", Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, err)
	createTeam(t, s, "empty", sharedTeam())
	snap := s.Snapshot()
	assert.Equal(t, access.Membership{TeamDependencies: people.TeamDependencies{People: 1, Invitations: 1}, AppKeys: 1, CanSuspend: true}, snap.Membership(member.TeamID))
	assert.Equal(t, access.Membership{TeamDependencies: people.TeamDependencies{People: 1, OwnerProtected: true}}, snap.Membership(owner.TeamID))
	assert.Equal(t, access.Membership{AppKeys: 1}, snap.Membership(teams.PersonalID("solo")), "a personal team is neither deleted nor suspended")
	assert.Equal(t, access.Membership{CanDelete: true, CanSuspend: true}, snap.Membership("empty"))
	assert.Equal(t, access.Membership{}, snap.Membership("missing"))
	assert.Equal(t, access.Membership{}, (*access.Snapshot)(nil).Membership("empty"))
}

// QueuedRace runs first and then second against s with both waiting at its
// writer gate, held meanwhile by an update of gateTeam, so the gate alone
// orders them: first starts twice a key hash ahead, so it is queued even when
// it hashes a credential before taking the gate; the gate opens once second
// had time to queue unless second hashes too, which then commits last anyway.
func QueuedRace(t *testing.T, s access.Store, gateTeam string, first, second func()) {
	t.Helper()
	start := utils.Now()
	_, err := keys.HashKey("zzr_queued_race")
	require.NoError(t, err)
	ahead := 2 * utils.Now().Sub(start)
	entered, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		_, err := s.UpdateTeam(context.Background(), gateTeam, func(*teams.Team) error {
			close(entered)
			<-release
			return nil
		}, audit.Event{})
		held <- err
	}()
	<-entered
	var wg sync.WaitGroup
	wg.Go(first)
	time.Sleep(ahead)
	wg.Go(second)
	time.Sleep(100 * time.Millisecond)
	close(release)
	require.NoError(t, <-held)
	wg.Wait()
}

// testKeyCreationRacesTeamDeletion: a key joining a team and that team's
// deletion are one after the other, never both, in either order.
func testKeyCreationRacesTeamDeletion(t *testing.T, h Harness) {
	s := h.Store
	createTeam(t, s, "gate", sharedTeam())
	joined := map[bool]int{}
	for round := 0; joined[true] == 0 || joined[false] == 0; round++ {
		require.Less(t, round, 12, "both orders occur")
		teamID, keyID := fmt.Sprint("team-", round), fmt.Sprint("key-", round)
		createTeam(t, s, teamID, sharedTeam())
		var createErr, deleteErr error
		create := func() { _, _, createErr = s.CreateKey(context.Background(), keyID, newKey(teamID), audit.Event{}) }
		remove := func() { deleteErr = s.DeleteTeam(context.Background(), teamID, audit.Event{}) }
		if round%2 == 0 {
			QueuedRace(t, s, "gate", create, remove)
		} else {
			QueuedRace(t, s, "gate", remove, create)
		}
		snap := s.Snapshot()
		if createErr == nil {
			require.ErrorIs(t, deleteErr, teams.ErrTeamHasMembers)
			assert.Equal(t, []string{keyID}, keyIDs(snap, teamID))
		} else {
			require.ErrorIs(t, createErr, teams.ErrTeamNotFound)
			require.ErrorIs(t, createErr, keys.ErrInvalid)
			require.NoError(t, deleteErr)
			assert.Nil(t, snap.Team(teamID))
			assert.Nil(t, snap.Key(keyID))
		}
		joined[createErr == nil]++
	}
}

// testAuditRowsOfOneCommit pages a history whose commits wrote one, two and
// no rows: newest first, a commit's rows in the order it lists them.
func testAuditRowsOfOneCommit(t *testing.T, h Harness) {
	s := h.Store
	require.NoError(t, s.Record(t.Context(), event(audit.ActionKeyUsageReset, "before")))
	_, _, err := s.CreateKey(t.Context(), "solo", newKey(""), event(audit.ActionKeyCreated, "solo"))
	require.NoError(t, err)
	createTeam(t, s, "silent", sharedTeam())
	require.NoError(t, s.DeleteKey(t.Context(), "solo", event(audit.ActionKeyDeleted, "solo")))
	require.NoError(t, s.Record(t.Context(), event(audit.ActionKeyUsageReset, "after")))
	personal := teams.PersonalID("solo")
	want := []string{"after", personal, "solo", "solo", personal, "before"}
	for _, limit := range []int{1, 2, 4, 100} {
		got, _ := filtered(t, s, audit.Filter{}, limit)
		assert.Equal(t, want, subjectsOf(got), "pages of %d", limit)
	}
}
