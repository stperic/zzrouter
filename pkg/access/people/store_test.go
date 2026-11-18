package people

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPassword = "a long test password"
const testProof = "a validated browser csrf proof"

func testOptions() Options {
	return Options{
		ValidateExternalIssuer: func(_ context.Context, reference string) error {
			if reference == "installation" || reference == "local-recovery" {
				return nil
			}
			return ErrInvalidCredentials
		},
		ValidateTeam: func(_ context.Context, id string) error {
			if id == "workspace" || id == "engineering" {
				return nil
			}
			return ErrNotFound
		},
	}
}

// memoryOwner stands in for the access store: one gate, the published state,
// an audit sink and an optional persist failure.
type memoryOwner struct {
	gate    sync.Mutex
	current atomic.Pointer[State]
	sink    audit.Sink
	failure error
}

func (o *memoryOwner) mutate(ctx context.Context, change func(*State) (audit.Event, error)) (*State, error) {
	o.gate.Lock()
	defer o.gate.Unlock()
	next := o.current.Load().Clone()
	event, err := change(next)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.failure != nil {
		return nil, o.failure
	}
	o.current.Store(next)
	if event.Action != "" && o.sink != nil {
		audit.Log(ctx, o.sink, event)
	}
	return next, nil
}

// failPersists makes every later persist fail with err; nil restores success.
func (o *memoryOwner) failPersists(err error) {
	o.gate.Lock()
	defer o.gate.Unlock()
	o.failure = err
}

func (o *memoryOwner) setSink(sink audit.Sink) {
	o.gate.Lock()
	defer o.gate.Unlock()
	o.sink = sink
}

// testPeople is a Store over its own in-memory owner.
type testPeople struct {
	*Store
	owner *memoryOwner
}

func (p *testPeople) state() *State { return p.owner.current.Load() }

func newTestPeople(t *testing.T, state *State) *testPeople {
	t.Helper()
	owner := &memoryOwner{}
	owner.current.Store(state)
	options := testOptions()
	options.Mutate, options.View = owner.mutate, owner.current.Load
	s, err := New(options)
	require.NoError(t, err)
	return &testPeople{Store: s, owner: owner}
}

func openTestStore(t *testing.T) *testPeople { return newTestPeople(t, nil) }

// reopen restarts over the persisted form of the current state.
func (p *testPeople) reopen(t *testing.T) *testPeople {
	t.Helper()
	data, err := json.Marshal(p.state())
	require.NoError(t, err)
	state, err := DecodeState(data)
	require.NoError(t, err)
	return newTestPeople(t, state)
}

func setupGrant(t *testing.T, s *testPeople) *IssuedGrant {
	t.Helper()
	grant, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: Issuer{Reference: "installation"}})
	require.NoError(t, err)
	bound, err := s.Exchange(t.Context(), grant.Token, testProof)
	require.NoError(t, err)
	return bound
}

func claimTestOwner(t *testing.T, s *testPeople) *Person {
	t.Helper()
	grant := setupGrant(t, s)
	p, err := s.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: "Owner", Name: "Router owner", Password: testPassword})
	require.NoError(t, err)
	return p
}

func TestOwnerClaimConcurrentAndDurable(t *testing.T) {
	s := openTestStore(t)
	grant := setupGrant(t, s)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, username := range []string{"alice", "bob"} {
		wg.Go(func() {
			<-start
			_, err := s.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: username, Name: username, Password: testPassword})
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
		} else {
			require.True(t, errors.Is(err, ErrInvalidGrant) || errors.Is(err, ErrInitialized), "%v", err)
		}
	}
	require.Equal(t, 1, winners)
	require.True(t, s.state().Initialized())
	require.Len(t, s.state().People(), 1)
	before := s.state().Owner()
	require.True(t, s.state().OwnerTeam(before.TeamID))
	require.True(t, s.state().TeamImpact(before.TeamID).HasMembers())
	data, err := json.Marshal(s.state())
	require.NoError(t, err)
	assert.NotContains(t, string(data), testPassword)
	assert.NotContains(t, string(data), grant.Token)
	assert.NotContains(t, string(data), testProof)
	public, err := json.Marshal(s.state().People())
	require.NoError(t, err)
	assert.NotContains(t, string(public), "password_hash")
	reopened := s.reopen(t)
	assert.Equal(t, before, reopened.state().Owner())
	_, err = reopened.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: "retry", Name: "Retry", Password: testPassword})
	require.Error(t, err)
}

func TestFailedOwnerSaveDoesNotConsumeGrant(t *testing.T) {
	s := openTestStore(t)
	grant := setupGrant(t, s)
	before := s.state()
	s.owner.failPersists(errors.New("write failed"))
	_, err := s.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: "alice", Name: "Alice", Password: testPassword})
	require.Error(t, err)
	assert.False(t, s.state().Initialized())
	assert.Empty(t, s.state().People())
	_, err = s.Inspect(t.Context(), grant.Token)
	require.NoError(t, err)
	assert.Same(t, before, s.state(), "a failed persist publishes nothing")
	s.owner.failPersists(nil)
	_, err = s.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: "alice", Name: "Alice", Password: testPassword})
	require.NoError(t, err)
}

func TestInvitationBindingConsumptionAndIssuerRevocation(t *testing.T) {
	s := openTestStore(t)
	owner := claimTestOwner(t, s)
	issuer := Issuer{PersonID: owner.ID, Revision: owner.Revision, Reference: "opaque-person-reference"}
	invite, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeInvitation, TeamID: "engineering", Role: "user", Issuer: issuer})
	require.NoError(t, err)
	bound, err := s.Exchange(t.Context(), invite.Token, testProof)
	require.NoError(t, err)
	_, err = s.Exchange(t.Context(), invite.Token, testProof)
	require.ErrorIs(t, err, ErrInvalidGrant)
	input := CreatePerson{Username: "  ALICE  ", Name: "Alice", Password: testPassword}
	_, err = s.AcceptInvitation(t.Context(), bound.Token, "wrong-browser", input)
	require.ErrorIs(t, err, ErrInvalidGrant)
	member, err := s.AcceptInvitation(t.Context(), bound.Token, testProof, input)
	require.NoError(t, err)
	assert.Equal(t, "alice", member.Username)
	assert.Equal(t, "engineering", member.TeamID)
	assert.Equal(t, "user", member.Role)
	assert.False(t, member.Owner)
	_, err = s.AcceptInvitation(t.Context(), bound.Token, testProof, CreatePerson{Username: "reuse", Name: "Reuse", Password: testPassword})
	require.ErrorIs(t, err, ErrInvalidGrant)
	assert.Equal(t, "accepted", s.state().Invitations()[0].Status)
	invite, err = s.Issue(t.Context(), GrantRequest{Purpose: PurposeInvitation, TeamID: "engineering", Role: "user", Issuer: issuer})
	require.NoError(t, err)
	_, err = s.RevokeSessions(t.Context(), owner.ID, owner.Revision)
	require.NoError(t, err)
	_, err = s.Inspect(t.Context(), invite.Token)
	require.ErrorIs(t, err, ErrInvalidGrant)
	_, err = s.Issue(t.Context(), GrantRequest{Purpose: PurposeSession, Issuer: issuer})
	require.ErrorIs(t, err, ErrInvalidGrant)
}

func TestOwnerProtectionAndRecoveryPreserveMembership(t *testing.T) {
	s := openTestStore(t)
	owner := claimTestOwner(t, s)
	for _, update := range []UpdatePerson{{Role: ptr("user")}, {TeamID: ptr("engineering")}, {Suspended: ptr(true)}} {
		_, err := s.Update(t.Context(), owner.ID, update)
		require.ErrorIs(t, err, ErrOwnerProtected)
	}
	_, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeRecovery, TeamID: owner.TeamID, PersonID: owner.ID,
		Issuer: Issuer{PersonID: owner.ID, Revision: owner.Revision, Reference: "person-reference"}})
	require.ErrorIs(t, err, ErrOwnerProtected)
	grant, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeRecovery, TeamID: owner.TeamID, PersonID: owner.ID, Issuer: Issuer{Reference: "local-recovery"}})
	require.NoError(t, err)
	bound, err := s.Exchange(t.Context(), grant.Token, testProof)
	require.NoError(t, err)
	recovered, err := s.Recover(t.Context(), bound.Token, testProof, "a different long password")
	require.NoError(t, err)
	assert.Equal(t, owner.ID, recovered.ID)
	assert.Equal(t, owner.TeamID, recovered.TeamID)
	assert.NotEqual(t, owner.Revision, recovered.Revision)
	_, err = s.Authenticate(t.Context(), owner.Username, testPassword)
	require.ErrorIs(t, err, ErrInvalidCredentials)
	_, err = s.Authenticate(t.Context(), owner.Username, "a different long password")
	require.NoError(t, err)
	_, err = s.Recover(t.Context(), bound.Token, testProof, testPassword)
	require.ErrorIs(t, err, ErrInvalidGrant)
}

func TestGrantPurposeExpiryAndFailedExchange(t *testing.T) {
	s := openTestStore(t)
	grant, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeSession, Issuer: Issuer{Reference: "installation"}})
	require.NoError(t, err)
	s.owner.failPersists(errors.New("disk unavailable"))
	_, err = s.Exchange(t.Context(), grant.Token, testProof)
	require.Error(t, err)
	_, err = s.Inspect(t.Context(), grant.Token)
	require.NoError(t, err)
	s.owner.failPersists(nil)
	bound, err := s.Exchange(t.Context(), grant.Token, testProof)
	require.NoError(t, err)
	_, err = s.ClaimOwner(t.Context(), bound.Token, testProof, CreatePerson{Username: "alice", Name: "Alice", Password: testPassword})
	require.ErrorIs(t, err, ErrInvalidGrant)
	issuer, err := s.ConsumeSession(t.Context(), bound.Token, testProof)
	require.NoError(t, err)
	assert.Equal(t, "installation", issuer.Reference)
	_, err = s.ConsumeSession(t.Context(), bound.Token, testProof)
	require.ErrorIs(t, err, ErrInvalidGrant)
	grant, err = s.Issue(t.Context(), GrantRequest{Purpose: PurposeSetup, Role: "admin", TeamID: "workspace", Issuer: Issuer{Reference: "installation"}, TTL: time.Second})
	require.NoError(t, err)
	restore := utils.SetClock(utils.FixedClock(utils.NowUTC().Add(time.Minute)))
	defer restore()
	_, err = s.Exchange(t.Context(), grant.Token, testProof)
	require.ErrorIs(t, err, ErrInvalidGrant)
}

func TestPasswordValidationThrottlingAndCapacity(t *testing.T) {
	s := openTestStore(t)
	owner := claimTestOwner(t, s)
	for _, password := range []string{"short", strings.Repeat("x", 129), string([]byte{0xff, 0xfe})} {
		_, err := s.hashPassword(t.Context(), password)
		require.ErrorIs(t, err, ErrInvalidInput)
	}
	for _, encoded := range []string{"", "$argon2id$v=19$m=4294967295,t=3,p=1$AAAA$AAAA", passwordPrefix + strings.Repeat("A", 257)} {
		require.False(t, validPasswordHash(encoded))
	}
	for range 10 {
		_, err := s.Authenticate(t.Context(), "unknown", testPassword)
		require.ErrorIs(t, err, ErrInvalidCredentials)
	}
	_, err := s.Authenticate(t.Context(), "unknown", testPassword)
	require.ErrorIs(t, err, ErrLimited)
	s.passwords <- struct{}{}
	s.passwords <- struct{}{}
	_, err = s.Authenticate(t.Context(), owner.Username, testPassword)
	require.ErrorIs(t, err, ErrLimited)
	<-s.passwords
	<-s.passwords
	_, err = s.Authenticate(t.Context(), owner.Username, testPassword)
	require.NoError(t, err)
}

func TestProgressUsesObservedMilestones(t *testing.T) {
	s := openTestStore(t)
	owner := claimTestOwner(t, s)
	require.NoError(t, s.UpdateProgress(t.Context(), ProgressUpdate{Step: ptr("app"), Dismissed: ptr(true)}))
	assert.False(t, s.state().Progress().ResponseVerified)
	assert.False(t, s.state().Progress().AppConnected)
	require.ErrorIs(t, s.MarkResponse(t.Context(), "another-person"), ErrNotFound)
	require.NoError(t, s.MarkResponse(t.Context(), owner.ID))
	require.NoError(t, s.MarkAppConnected(t.Context()))
	assert.True(t, s.state().Progress().ResponseVerified)
	assert.True(t, s.state().Progress().AppConnected)
}

func ptr[T any](value T) *T { return &value }

func TestInvitationConcurrentConsumptionSurvivesRestart(t *testing.T) {
	s := openTestStore(t)
	owner := claimTestOwner(t, s)
	grant, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeInvitation, TeamID: "engineering", Role: "user",
		Issuer: Issuer{PersonID: owner.ID, Revision: owner.Revision, Reference: "owner-reference"}})
	require.NoError(t, err)
	bound, err := s.Exchange(t.Context(), grant.Token, testProof)
	require.NoError(t, err)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"alice", "bob"} {
		wg.Go(func() {
			<-start
			_, err := s.AcceptInvitation(t.Context(), bound.Token, testProof, CreatePerson{Username: name, Name: name, Password: testPassword})
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
		} else {
			require.ErrorIs(t, err, ErrInvalidGrant)
		}
	}
	require.Equal(t, 1, winners)
	require.Len(t, s.state().People(), 2)
	reopened := s.reopen(t)
	_, err = reopened.Inspect(t.Context(), bound.Token)
	require.ErrorIs(t, err, ErrInvalidGrant)
	assert.Equal(t, "accepted", reopened.state().Invitations()[0].Status)
}

func TestInvitationIssuerSuspensionRevokesPendingAuthority(t *testing.T) {
	s := openTestStore(t)
	owner := claimTestOwner(t, s)
	grant, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeInvitation, TeamID: "engineering", Role: "admin",
		Issuer: Issuer{PersonID: owner.ID, Revision: owner.Revision, Reference: "owner-reference"}})
	require.NoError(t, err)
	bound, err := s.Exchange(t.Context(), grant.Token, testProof)
	require.NoError(t, err)
	admin, err := s.AcceptInvitation(t.Context(), bound.Token, testProof, CreatePerson{Username: "team-admin", Name: "Team administrator", Password: testPassword})
	require.NoError(t, err)
	invite, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeInvitation, TeamID: "engineering", Role: "user",
		Issuer: Issuer{PersonID: admin.ID, Revision: admin.Revision, Reference: "admin-reference"}})
	require.NoError(t, err)
	bound, err = s.Exchange(t.Context(), invite.Token, testProof)
	require.NoError(t, err)
	suspended, err := s.Update(t.Context(), admin.ID, UpdatePerson{Suspended: ptr(true)})
	require.NoError(t, err)
	assert.NotEqual(t, admin.Revision, suspended.Revision)
	_, err = s.AcceptInvitation(t.Context(), bound.Token, testProof, CreatePerson{Username: "recipient", Name: "Recipient", Password: testPassword})
	require.ErrorIs(t, err, ErrInvalidGrant)
	assert.Len(t, s.state().People(), 2)
}

func TestGrantExternalIssuerAndTeamRevalidated(t *testing.T) {
	s := openTestStore(t)
	grant := setupGrant(t, s)
	validate := s.options.ValidateExternalIssuer
	s.options.ValidateExternalIssuer = func(context.Context, string) error { return ErrInvalidCredentials }
	_, err := s.Inspect(t.Context(), grant.Token)
	require.ErrorIs(t, err, ErrInvalidGrant)
	s.options.ValidateExternalIssuer = validate
	s.options.ValidateTeam = func(context.Context, string) error { return ErrNotFound }
	_, err = s.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: "alice", Name: "Alice", Password: testPassword})
	require.ErrorIs(t, err, ErrInvalidGrant)
	assert.False(t, s.state().Initialized())
}

// Both changes verify the same revision; the commit's compare-and-swap admits one.
func TestConcurrentPasswordChangesOneWins(t *testing.T) {
	s := openTestStore(t)
	owner := claimTestOwner(t, s)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, next := range []string{"first replacement password", "second replacement password"} {
		wg.Go(func() {
			<-start
			_, err := s.ChangePassword(t.Context(), owner.ID, owner.Revision, testPassword, next)
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
		} else {
			require.ErrorIs(t, err, ErrInvalidCredentials)
		}
	}
	require.Equal(t, 1, winners)
	assert.NotEqual(t, owner.Revision, s.state().Person(owner.ID).Revision)
}
