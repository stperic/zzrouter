package accesstest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
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

// boundGrant issues an installation grant and binds it to the contract's browser proof.
func boundGrant(t *testing.T, s access.Store, purpose people.Purpose, teamID, role string) *people.IssuedGrant {
	t.Helper()
	return bind(t, s, people.GrantRequest{Purpose: purpose, TeamID: teamID, Role: role, Issuer: people.Issuer{Reference: installation}})
}

func bind(t *testing.T, s access.Store, req people.GrantRequest) *people.IssuedGrant {
	t.Helper()
	grant, err := s.People().Issue(t.Context(), req)
	require.NoError(t, err)
	bound, err := s.People().Exchange(t.Context(), grant.Token, proof)
	require.NoError(t, err)
	return bound
}

// race runs call n times at once and requires exactly one success; every
// loser fails with one of allowed.
func race(t *testing.T, n int, call func(int) error, allowed ...error) {
	t.Helper()
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			errs <- call(i)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
			continue
		}
		known := false
		for _, want := range allowed {
			known = known || errors.Is(err, want)
		}
		assert.True(t, known, "unexpected loser error: %v", err)
	}
	assert.Equal(t, 1, winners)
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
	rec := observe(t, h)
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
	assert.True(t, snap.Membership("workspace").OwnerProtected)
	assert.Equal(t, "workspace", owner.TeamID)
	assert.True(t, owner.Owner)
	assert.Equal(t, audit.ActionPersonCreated, rec.lastEvent().Action)
	_, err = s.People().ClaimOwner(t.Context(), bound.Token, proof, people.CreatePerson{Username: "again", Name: "Again", Password: password})
	require.ErrorIs(t, err, people.ErrInvalidGrant, "a setup grant is single use")
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
	rec := observe(t, h)
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

// testPeopleReopen requires every part of the people state to survive a
// reopen: issuers, browser bindings, suspension and the whole progress.
func testPeopleReopen(t *testing.T, h Harness) {
	s := h.Store
	createKey(t, s, "reader")
	owner := claimOwner(t, s)
	member := invite(t, s, "alice")
	suspended := bind(t, s, people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "eng", Role: "user", Issuer: people.Issuer{Reference: installation}})
	bob, err := s.People().AcceptInvitation(t.Context(), suspended.Token, proof, people.CreatePerson{Username: "bob", Name: "Bob", Password: password})
	require.NoError(t, err)
	_, err = s.People().Update(t.Context(), bob.ID, people.UpdatePerson{Suspended: new(true)})
	require.NoError(t, err)
	byOwner := people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "eng", Role: "user", Issuer: people.Issuer{PersonID: owner.ID, Revision: owner.Revision, Reference: "owner"}}
	redeemed, revoked := bind(t, s, byOwner), bind(t, s, byOwner)
	session := bind(t, s, people.GrantRequest{Purpose: people.PurposeSession, Issuer: people.Issuer{Reference: installation}})
	_, err = s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeRecovery, TeamID: "eng", PersonID: member.ID, Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, err)
	withdrawn := bind(t, s, people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "eng", Role: "admin", Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, s.People().RevokeGrant(t.Context(), withdrawn.ID))
	require.NoError(t, s.People().UpdateProgress(t.Context(), people.ProgressUpdate{Step: new("app"), Dismissed: new(true)}))
	require.NoError(t, s.People().MarkResponse(t.Context(), owner.ID))
	require.NoError(t, s.People().MarkAppConnected(t.Context()))
	before := s.Snapshot()
	records := before.PeopleState().Records()
	requireCovered(t, slices.Collect(maps.Values(records.People)))
	requireCovered(t, slices.Collect(maps.Values(records.Grants)), "Status") // derived on read
	requireAllSet(t, &records.Progress)

	reopened := h.Reopen()
	snap := reopened.Snapshot()
	assert.Equal(t, records, snap.PeopleState().Records(), "every person, grant and the progress survive")

	authenticated, err := reopened.People().Authenticate(t.Context(), "alice", password)
	require.NoError(t, err)
	assert.Equal(t, member.ID, authenticated.ID)
	_, err = reopened.People().Authenticate(t.Context(), "bob", password)
	require.ErrorIs(t, err, people.ErrInvalidCredentials, "a suspended person stays suspended")
	carol := people.CreatePerson{Username: "carol", Name: "Carol", Password: password}
	_, err = reopened.People().AcceptInvitation(t.Context(), redeemed.Token, "another browser", carol)
	require.ErrorIs(t, err, people.ErrInvalidGrant, "the browser binding survives")
	_, err = reopened.People().AcceptInvitation(t.Context(), redeemed.Token, proof, carol)
	require.NoError(t, err, "a bound invitation is redeemed with its proof")
	issuer, err := reopened.People().ConsumeSession(t.Context(), session.Token, proof)
	require.NoError(t, err)
	assert.Equal(t, people.Issuer{Reference: installation}, issuer)
	_, err = reopened.People().RevokeSessions(t.Context(), owner.ID, owner.Revision)
	require.NoError(t, err)
	_, err = reopened.People().AcceptInvitation(t.Context(), revoked.Token, proof, people.CreatePerson{Username: "dave", Name: "Dave", Password: password})
	require.ErrorIs(t, err, people.ErrInvalidGrant, "the issuer survives, so revoking it revokes the invitation")
	snap = reopened.Snapshot()
	assert.NotNil(t, snap.Key("reader"), "people commits keep the keys")
	assert.NotNil(t, snap.Team("eng"), "people commits keep the teams")
}

// testPeopleResults requires every people operation to answer with the record
// the snapshot then holds, in the store's own form, with no tolerance.
func testPeopleResults(t *testing.T, h Harness) {
	s := h.Store
	invitation := func(g *people.IssuedGrant) people.Grant {
		for _, grant := range s.Snapshot().Invitations() {
			if grant.ID == g.ID {
				return grant
			}
		}
		return people.Grant{}
	}
	owner := claimOwner(t, s)
	assert.Equal(t, owner, s.Snapshot().Owner(), "ClaimOwner")
	createTeam(t, s, "eng", sharedTeam())
	issued, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "eng", Role: "user", Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, err)
	assert.Equal(t, issued.Grant, invitation(issued), "Issue")
	bound, err := s.People().Exchange(t.Context(), issued.Token, proof)
	require.NoError(t, err)
	assert.Equal(t, bound.Grant, invitation(bound), "Exchange")
	member, err := s.People().AcceptInvitation(t.Context(), bound.Token, proof, people.CreatePerson{Username: "alice", Name: "Alice", Password: password})
	require.NoError(t, err)
	assert.Equal(t, member, s.Snapshot().Person(member.ID), "AcceptInvitation")
	member, err = s.People().Update(t.Context(), member.ID, people.UpdatePerson{Name: new("Alice Renamed")})
	require.NoError(t, err)
	assert.Equal(t, member, s.Snapshot().Person(member.ID), "Update")
	member, err = s.People().RevokeSessions(t.Context(), member.ID, member.Revision)
	require.NoError(t, err)
	assert.Equal(t, member, s.Snapshot().Person(member.ID), "RevokeSessions")
	member, err = s.People().ChangePassword(t.Context(), member.ID, member.Revision, password, "another long contract password")
	require.NoError(t, err)
	assert.Equal(t, member, s.Snapshot().Person(member.ID), "ChangePassword")
	recovery := bind(t, s, people.GrantRequest{Purpose: people.PurposeRecovery, TeamID: "eng", PersonID: member.ID, Issuer: people.Issuer{Reference: installation}})
	member, err = s.People().Recover(t.Context(), recovery.Token, proof, password)
	require.NoError(t, err)
	assert.Equal(t, member, s.Snapshot().Person(member.ID), "Recover")
}

// testPeopleViewMatchesSnapshot requires what people transitions read never to
// run ahead of Snapshot(), while commits continue.
func testPeopleViewMatchesSnapshot(t *testing.T, h Harness) {
	s := h.Store
	owner := claimOwner(t, s)
	revoked, err := s.People().RevokeSessions(t.Context(), owner.ID, owner.Revision)
	require.NoError(t, err)
	authenticated, err := s.People().Authenticate(t.Context(), "owner", password)
	require.NoError(t, err)
	assert.Equal(t, revoked.Revision, authenticated.Revision)
	assert.Equal(t, authenticated.Revision, s.Snapshot().Person(owner.ID).Revision)

	createKey(t, s, "reader")
	createTeam(t, s, "eng", sharedTeam())
	grants := make([]*people.IssuedGrant, 48)
	for i := range grants {
		grants[i], err = s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "eng", Role: "user", Issuer: people.Issuer{Reference: installation}})
		require.NoError(t, err)
	}
	status := func(snap *access.Snapshot, id string) string {
		for _, grant := range snap.Invitations() {
			if grant.ID == id {
				return grant.Status
			}
		}
		return ""
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(done)
		for _, grant := range grants {
			assert.NoError(t, s.People().RevokeGrant(context.Background(), grant.ID))
		}
	})
	wg.Go(func() {
		for range grants {
			_, err := s.UpdateKey(context.Background(), "reader", func(k *keys.VirtualKey) error { k.RPMLimit++; return nil }, audit.Event{})
			assert.NoError(t, err)
		}
	})
	wg.Go(func() {
		seen := make([]bool, len(grants))
		for {
			select {
			case <-done:
				return
			default:
			}
			for i, grant := range grants {
				if seen[i] {
					continue
				}
				// A revocation People() already sees is in every later snapshot.
				if _, err := s.People().Inspect(context.Background(), grant.Token); errors.Is(err, people.ErrInvalidGrant) {
					if got := status(s.Snapshot(), grant.ID); got != "revoked" {
						t.Errorf("People() saw grant %s revoked before Snapshot() did (status %q)", grant.ID, got)
						return
					}
					seen[i] = true
				}
			}
		}
	})
	wg.Wait()
}

// testPeopleConcurrentRedemption gives every single-use grant and revision one winner.
func testPeopleConcurrentRedemption(t *testing.T, h Harness) {
	s := h.Store
	const callers = 3
	createTeam(t, s, "workspace", sharedTeam())
	setup := boundGrant(t, s, people.PurposeSetup, "workspace", "admin")
	race(t, callers, func(i int) error {
		_, err := s.People().ClaimOwner(context.Background(), setup.Token, proof, people.CreatePerson{Username: fmt.Sprint("owner", i), Name: "Owner", Password: password})
		return err
	}, people.ErrInvalidGrant, people.ErrInitialized, people.ErrLimited)
	require.Len(t, s.Snapshot().People(), 1, "one setup grant makes one person")

	createTeam(t, s, "eng", sharedTeam())
	invitation := boundGrant(t, s, people.PurposeInvitation, "eng", "user")
	race(t, callers, func(i int) error {
		_, err := s.People().AcceptInvitation(context.Background(), invitation.Token, proof, people.CreatePerson{Username: fmt.Sprint("member", i), Name: "Member", Password: password})
		return err
	}, people.ErrInvalidGrant, people.ErrLimited)
	members := s.Snapshot().People()
	require.Len(t, members, 2, "one invitation makes one person")
	var member people.Person
	for _, p := range members {
		if !p.Owner {
			member = p
		}
	}
	race(t, callers, func(i int) error {
		_, err := s.People().ChangePassword(context.Background(), member.ID, member.Revision, password, fmt.Sprint("another long contract password ", i))
		return err
	}, people.ErrInvalidCredentials, people.ErrLimited)
	assert.NotEqual(t, member.Revision, s.Snapshot().Person(member.ID).Revision)
}

// testPeopleSingleUseCodes races single-use codes with no password work on
// the path, so only the writer gate decides the one winner.
func testPeopleSingleUseCodes(t *testing.T, h Harness) {
	s := h.Store
	const callers = 16
	session := boundGrant(t, s, people.PurposeSession, "", "")
	race(t, callers, func(int) error {
		_, err := s.People().ConsumeSession(context.Background(), session.Token, proof)
		return err
	}, people.ErrInvalidGrant)
	issued, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSession, Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, err)
	race(t, callers, func(int) error {
		_, err := s.People().Exchange(context.Background(), issued.Token, proof)
		return err
	}, people.ErrInvalidGrant)
}

// testPeopleAndKeyWriters interleaves people and key commits and loses none.
func testPeopleAndKeyWriters(t *testing.T, h Harness) {
	s := h.Store
	createKey(t, s, "reader")
	const writers = 8
	tokens := make([]string, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			_, err := s.UpdateKey(context.Background(), "reader", func(k *keys.VirtualKey) error { k.RPMLimit++; return nil }, audit.Event{})
			assert.NoError(t, err)
		})
		wg.Go(func() {
			grant, err := s.People().Issue(context.Background(), people.GrantRequest{Purpose: people.PurposeSession, Issuer: people.Issuer{Reference: installation}})
			if assert.NoError(t, err) {
				tokens[i] = grant.Token
			}
		})
	}
	wg.Wait()
	assert.Equal(t, writers, s.Snapshot().Key("reader").RPMLimit)
	for _, token := range tokens {
		_, err := s.People().Inspect(t.Context(), token)
		assert.NoError(t, err, "every session grant was kept")
	}
}

func testPeopleCommitNotifies(t *testing.T, h Harness) {
	s := h.Store
	createTeam(t, s, "workspace", sharedTeam())
	rec := observe(t, h)
	version := s.Snapshot().KeysVersion()
	_, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, err)
	_, commits := rec.counts()
	require.Equal(t, 1, commits, "a people commit notifies subscribers")
	c := rec.lastCommit()
	assert.Equal(t, version, s.Snapshot().KeysVersion(), "a people commit leaves key caches alone")
	assert.Empty(t, c.RemovedKeys)
	assert.Empty(t, c.RemovedTeams)

	invitation, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "workspace", Role: "user", Issuer: people.Issuer{Reference: installation}})
	require.NoError(t, err)
	require.NoError(t, s.People().RevokeGrant(t.Context(), invitation.ID))
	events, commits := rec.counts()
	require.NoError(t, s.People().RevokeGrant(t.Context(), invitation.ID), "revoking a revoked grant succeeds")
	gotEvents, gotCommits := rec.counts()
	assert.Equal(t, events, gotEvents, "a commit that changes nothing and carries no event records nothing")
	assert.Equal(t, commits, gotCommits, "and notifies nobody")
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

// testIssueSetup issues setup grants into one owner workspace, created with
// the first grant in one commit, and none once the owner is claimed.
func testIssueSetup(t *testing.T, h Harness) {
	s := h.Store
	rec := observe(t, h)
	ctx := audit.WithIdentity(t.Context(), "static:admin", "setup-1")
	first, err := s.IssueSetup(ctx, people.Issuer{Reference: installation})
	require.NoError(t, err)
	assert.Equal(t, people.PurposeSetup, first.Purpose)
	assert.Equal(t, "admin", first.Role)
	workspace := s.Snapshot().Team(first.TeamID)
	require.NotNil(t, workspace)
	assert.Equal(t, teams.KindShared, workspace.Kind)
	assert.Equal(t, "Personal workspace", workspace.Name)
	assert.Equal(t, "installation", workspace.CreatedBy)
	assert.Equal(t, map[string]string{"owner_workspace": "true"}, workspace.Metadata)
	inspected, err := s.People().Inspect(t.Context(), first.Token)
	require.NoError(t, err)
	assert.Equal(t, inspected, &first.Grant, "the grant returned is the grant stored")
	events, commits := rec.counts()
	assert.Equal(t, 1, commits, "the workspace and its grant commit together")
	assert.Equal(t, 1, events)
	issued := rec.lastEvent()
	assert.Equal(t, audit.ActionGrantIssued, issued.Action)
	assert.Equal(t, "static:admin", issued.Actor)
	assert.Equal(t, "setup-1", issued.RequestID)

	second, err := s.IssueSetup(t.Context(), people.Issuer{Reference: installation})
	require.NoError(t, err)
	assert.Equal(t, first.TeamID, second.TeamID, "a second setup grant targets the same workspace")
	assert.Equal(t, workspace, s.Snapshot().Team(first.TeamID))
	bound, err := s.People().Exchange(t.Context(), second.Token, proof)
	require.NoError(t, err)
	owner, err := s.People().ClaimOwner(t.Context(), bound.Token, proof, people.CreatePerson{Username: "owner", Name: "Owner", Password: password})
	require.NoError(t, err)
	assert.Equal(t, first.TeamID, owner.TeamID)

	_, commits = rec.counts()
	_, err = s.IssueSetup(t.Context(), people.Issuer{Reference: installation})
	require.ErrorIs(t, err, people.ErrInitialized)
	_, after := rec.counts()
	assert.Equal(t, commits, after, "a refused setup grant creates no workspace")
	_, err = s.People().Inspect(t.Context(), first.Token)
	require.ErrorIs(t, err, people.ErrInitialized, "an earlier setup grant closes with setup")
	assert.Len(t, maps.Collect(h.Reopen().Snapshot().Teams()), 1)
}

func testConcurrentIssueSetup(t *testing.T, h Harness) {
	s := h.Store
	const callers = 8
	grants := make([]*people.IssuedGrant, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Go(func() {
			<-start
			var err error
			grants[i], err = s.IssueSetup(context.Background(), people.Issuer{Reference: installation})
			assert.NoError(t, err)
		})
	}
	close(start)
	wg.Wait()
	teamIDs := maps.Collect(s.Snapshot().Teams())
	require.Len(t, teamIDs, 1, "one workspace team")
	for _, grant := range grants {
		require.NotNil(t, grant)
		assert.Contains(t, teamIDs, grant.TeamID)
	}
}

// testInvitationRacesTeamDeletion: an invitation either sees its team gone or
// keeps it from being deleted.
func testInvitationRacesTeamDeletion(t *testing.T, h Harness) {
	s := h.Store
	for i := range 8 {
		teamID := fmt.Sprint("invited-", i)
		createTeam(t, s, teamID, sharedTeam())
		start := make(chan struct{})
		var issueErr, deleteErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			_, issueErr = s.People().Issue(context.Background(), people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: teamID, Role: "user", Issuer: people.Issuer{Reference: installation}})
		})
		wg.Go(func() { <-start; deleteErr = s.DeleteTeam(context.Background(), teamID, audit.Event{}) })
		close(start)
		wg.Wait()
		snap := s.Snapshot()
		if issueErr == nil {
			require.ErrorIs(t, deleteErr, teams.ErrTeamHasMembers)
			assert.Equal(t, 1, snap.TeamImpact(teamID).Invitations)
		} else {
			require.ErrorIs(t, issueErr, people.ErrInvalidGrant)
			require.NoError(t, deleteErr)
			assert.Nil(t, snap.Team(teamID))
			assert.Zero(t, snap.TeamImpact(teamID).Invitations, "no invitation outlives its team")
		}
	}
}

// testPeopleAdmission admits people and their grants only into an existing,
// active shared team: at issue, at enrollment and when a person moves.
func testPeopleAdmission(t *testing.T, h Harness) {
	s := h.Store
	createKey(t, s, "solo")
	createTeam(t, s, "paused", sharedTeam())
	_, err := s.UpdateTeam(t.Context(), "paused", suspend, audit.Event{})
	require.NoError(t, err)
	refused := map[string]string{"a personal team": teams.PersonalID("solo"), "a suspended team": "paused", "a missing team": "missing"}
	for name, teamID := range refused {
		_, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSetup, TeamID: teamID, Role: "admin", Issuer: people.Issuer{Reference: installation}})
		require.ErrorIs(t, err, people.ErrInvalidGrant, "a setup grant into %s", name)
	}
	claimOwner(t, s)
	member := invite(t, s, "alice")
	for name, teamID := range refused {
		_, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: teamID, Role: "user", Issuer: people.Issuer{Reference: installation}})
		require.ErrorIs(t, err, people.ErrInvalidGrant, "an invitation into %s", name)
		_, err = s.People().Update(t.Context(), member.ID, people.UpdatePerson{TeamID: &teamID})
		require.ErrorIs(t, err, people.ErrInvalidGrant, "a move into %s", name)
	}
	pending := boundGrant(t, s, people.PurposeInvitation, member.TeamID, "user")
	_, err = s.UpdateTeam(t.Context(), member.TeamID, suspend, audit.Event{})
	require.NoError(t, err)
	_, err = s.People().AcceptInvitation(t.Context(), pending.Token, proof, people.CreatePerson{Username: "carol", Name: "Carol", Password: password})
	require.ErrorIs(t, err, people.ErrInvalidGrant, "an invitation whose team was suspended since is refused at enrollment")
	snap := s.Snapshot()
	assert.Len(t, snap.People(), 2)
	assert.Equal(t, member.TeamID, snap.Person(member.ID).TeamID, "a suspended team keeps its members")
	for _, teamID := range refused {
		assert.Zero(t, snap.TeamImpact(teamID), "nothing joined %s", teamID)
	}
}
