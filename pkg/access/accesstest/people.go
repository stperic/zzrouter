package accesstest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

const (
	installation = "installation"
	password     = "a long contract password"
	proof        = "a contract browser proof"
)

// ValidateExternalIssuer is the issuer authority a harness store's people use.
func ValidateExternalIssuer(_ context.Context, reference string) error {
	if reference != installation {
		return people.ErrInvalidGrant
	}
	return nil
}

// ValidateTeam admits people into any team of the store's own snapshot.
func ValidateTeam(snap *access.Snapshot, id string) error {
	if snap.Team(id) == nil {
		return people.ErrInvalidGrant
	}
	return nil
}

// boundGrant issues a grant and binds it to the contract's browser proof.
func boundGrant(t *testing.T, s access.Store, purpose people.Purpose, teamID, role string) *people.IssuedGrant {
	t.Helper()
	grant, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: purpose, TeamID: teamID, Role: role, Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, err)
	bound, err := s.People().Exchange(t.Context(), grant.Token, proof)
	require.NoError(t, err)
	return bound
}

func claimOwner(t *testing.T, s access.Store) *people.Person {
	t.Helper()
	createTeam(t, s, "workspace", sharedTeam())
	bound := boundGrant(t, s, people.PurposeSetup, "workspace", "admin")
	owner, err := s.People().ClaimOwner(t.Context(), bound.Token, proof, people.CreatePerson{Username: "owner", Name: "Owner", Password: password})
	require.NoError(t, err)
	return owner
}

func invite(t *testing.T, s access.Store, username string) *people.Person {
	t.Helper()
	createTeam(t, s, "eng", sharedTeam())
	bound := boundGrant(t, s, people.PurposeInvitation, "eng", "user")
	member, err := s.People().AcceptInvitation(t.Context(), bound.Token, proof, people.CreatePerson{Username: username, Name: username, Password: password})
	require.NoError(t, err)
	return member
}

func testPeopleOwnerSetup(t *testing.T, h Harness) {
	s := h.Store
	require.NotNil(t, s.People())
	assert.False(t, s.Snapshot().Initialized())
	rec := observe(s)
	createTeam(t, s, "workspace", sharedTeam())
	ctx := audit.WithIdentity(t.Context(), "operator", "setup-request")
	grant, err := s.People().Issue(ctx, people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, err)
	issued := rec.lastEvent()
	assert.Equal(t, audit.ActionGrantIssued, issued.Action)
	assert.Equal(t, "operator", issued.Actor, "people events carry the caller's identity")
	assert.Equal(t, "setup-request", issued.RequestID)
	bound, err := s.People().Exchange(t.Context(), grant.Token, proof)
	require.NoError(t, err)
	owner, err := s.People().ClaimOwner(t.Context(), bound.Token, proof, people.CreatePerson{Username: "Owner", Name: "Owner", Password: password})
	require.NoError(t, err)

	snap := s.Snapshot()
	assert.True(t, snap.Initialized())
	assert.Equal(t, owner, snap.Owner())
	assert.Equal(t, owner, snap.Person(owner.ID))
	assert.True(t, snap.OwnerTeam("workspace"))
	assert.Equal(t, "workspace", owner.TeamID)
	assert.True(t, owner.Owner)
	assert.Equal(t, audit.ActionPersonCreated, rec.lastEvent().Action)
	_, err = s.People().ClaimOwner(t.Context(), bound.Token, proof, people.CreatePerson{Username: "again", Name: "Again", Password: password})
	require.Error(t, err, "a setup grant is single use")
}

func testPeopleInvitation(t *testing.T, h Harness) {
	s := h.Store
	claimOwner(t, s)
	member := invite(t, s, "alice")
	assert.Equal(t, "eng", member.TeamID)
	assert.Equal(t, "user", member.Role)
	assert.False(t, member.Owner)
	snap := s.Snapshot()
	assert.Equal(t, member, snap.Person(member.ID))
	assert.Len(t, snap.People(), 2)
	assert.Equal(t, 1, snap.TeamImpact("eng").People)
	assert.Equal(t, "accepted", snap.Invitations()[0].Status)
}

func testPeoplePasswordChange(t *testing.T, h Harness) {
	s := h.Store
	claimOwner(t, s)
	member := invite(t, s, "alice")
	changed, err := s.People().ChangePassword(t.Context(), member.ID, member.Revision, password, "another long contract password")
	require.NoError(t, err)
	assert.NotEqual(t, member.Revision, changed.Revision)
	assert.Equal(t, changed.Revision, s.Snapshot().Person(member.ID).Revision)
	_, err = s.People().RevokeSessions(t.Context(), member.ID, member.Revision)
	require.ErrorIs(t, err, people.ErrInvalidCredentials, "the old revision no longer authorizes anything")
	_, err = s.People().Authenticate(t.Context(), "alice", password)
	require.ErrorIs(t, err, people.ErrInvalidCredentials)
	_, err = s.People().Authenticate(t.Context(), "alice", "another long contract password")
	require.NoError(t, err)
}

func testPeopleFailedCommit(t *testing.T, h Harness) {
	s := h.Store
	createTeam(t, s, "workspace", sharedTeam())
	bound := boundGrant(t, s, people.PurposeSetup, "workspace", "admin")
	rec := observe(s)
	h.FailNextCommit()
	_, err := s.People().ClaimOwner(t.Context(), bound.Token, proof, people.CreatePerson{Username: "owner", Name: "Owner", Password: password})
	require.Error(t, err)
	events, commits := rec.counts()
	assert.Zero(t, events, "a failed people commit records nothing")
	assert.Zero(t, commits)
	assert.False(t, s.Snapshot().Initialized())
	assert.Empty(t, s.Snapshot().People())
	_, err = s.People().Inspect(t.Context(), bound.Token)
	require.NoError(t, err, "the grant is not consumed")
	_, err = s.People().ClaimOwner(t.Context(), bound.Token, proof, people.CreatePerson{Username: "owner", Name: "Owner", Password: password})
	require.NoError(t, err)
}

func testPeopleReopen(t *testing.T, h Harness) {
	s := h.Store
	owner := claimOwner(t, s)
	member := invite(t, s, "alice")
	require.NoError(t, s.People().UpdateProgress(t.Context(), people.ProgressUpdate{Step: new("app")}))
	before := s.Snapshot()

	reopened := h.Reopen()
	snap := reopened.Snapshot()
	assert.True(t, snap.Initialized())
	assert.Equal(t, owner, snap.Owner())
	assert.Equal(t, before.People(), snap.People())
	assert.Equal(t, before.Invitations(), snap.Invitations())
	assert.Equal(t, "app", snap.Progress().Step)
	authenticated, err := reopened.People().Authenticate(t.Context(), "alice", password)
	require.NoError(t, err)
	assert.Equal(t, member.ID, authenticated.ID)
}

func testPeopleCommitNotifies(t *testing.T, h Harness) {
	s := h.Store
	createTeam(t, s, "workspace", sharedTeam())
	rec := observe(s)
	_, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, err)
	_, commits := rec.counts()
	require.Equal(t, 1, commits, "a people commit notifies subscribers")
	c := rec.lastCommit()
	assert.False(t, c.KeysChanged, "a people commit leaves key caches alone")
	assert.Empty(t, c.RemovedKeys)
	assert.Empty(t, c.RemovedTeams)
}

func testPeopleSnapshotIsolation(t *testing.T, h Harness) {
	s := h.Store
	createTeam(t, s, "workspace", sharedTeam())
	createTeam(t, s, "eng", teams.Team{Name: "Engineering", Kind: teams.KindShared})
	before := s.Snapshot()
	claimOwner := func() {
		bound := boundGrant(t, s, people.PurposeSetup, "workspace", "admin")
		_, err := s.People().ClaimOwner(t.Context(), bound.Token, proof, people.CreatePerson{Username: "owner", Name: "Owner", Password: password})
		require.NoError(t, err)
	}
	claimOwner()
	assert.False(t, before.Initialized(), "an earlier snapshot never changes")
	assert.Empty(t, before.People())
	assert.Nil(t, before.Owner())
	assert.Zero(t, before.TeamImpact("workspace").People)
	middle := s.Snapshot()
	invited := boundGrant(t, s, people.PurposeInvitation, "eng", "user")
	assert.Empty(t, middle.Invitations(), "a grant issued later is not in an earlier snapshot")
	_, err := s.People().AcceptInvitation(t.Context(), invited.Token, proof, people.CreatePerson{Username: "alice", Name: "alice", Password: password})
	require.NoError(t, err)
	assert.Len(t, middle.People(), 1)
	assert.Len(t, s.Snapshot().People(), 2)
}
