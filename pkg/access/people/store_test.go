package people

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/common/atomicfile"
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

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "access", "people.json"), testOptions())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func setupGrant(t *testing.T, s *Store) *IssuedGrant {
	t.Helper()
	grant, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: Issuer{Reference: "installation"}})
	require.NoError(t, err)
	bound, err := s.Exchange(t.Context(), grant.Token, testProof)
	require.NoError(t, err)
	return bound
}

func claimTestOwner(t *testing.T, s *Store) *Person {
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
	require.True(t, s.Initialized())
	require.Len(t, s.List(), 1)
	before := s.Owner()
	require.True(t, s.OwnerTeam(before.TeamID))
	require.True(t, s.ReferencedTeam(before.TeamID))
	data, err := os.ReadFile(s.path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), testPassword)
	assert.NotContains(t, string(data), grant.Token)
	assert.NotContains(t, string(data), testProof)
	public, err := json.Marshal(s.List())
	require.NoError(t, err)
	assert.NotContains(t, string(public), "password_hash")
	require.NoError(t, s.Close())
	reopened, err := Open(t.Context(), s.path, testOptions())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	assert.Equal(t, before, reopened.Owner())
	_, err = reopened.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: "retry", Name: "Retry", Password: testPassword})
	require.Error(t, err)
}

func TestFailedOwnerSaveDoesNotConsumeGrant(t *testing.T) {
	s := openTestStore(t)
	grant := setupGrant(t, s)
	before, err := os.ReadFile(s.path)
	require.NoError(t, err)
	write := s.write
	s.write = func(string, []byte, os.FileMode) error { return errors.New("write failed") }
	_, err = s.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: "alice", Name: "Alice", Password: testPassword})
	require.Error(t, err)
	assert.False(t, s.Initialized())
	assert.Empty(t, s.List())
	_, err = s.Inspect(t.Context(), grant.Token)
	require.NoError(t, err)
	after, err := os.ReadFile(s.path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	s.write = write
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
	assert.Equal(t, "accepted", s.Invitations()[0].Status)
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
	write := s.write
	s.write = func(string, []byte, os.FileMode) error { return errors.New("disk unavailable") }
	_, err = s.Exchange(t.Context(), grant.Token, testProof)
	require.Error(t, err)
	_, err = s.Inspect(t.Context(), grant.Token)
	require.NoError(t, err)
	s.write = write
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

func TestStoreLockAndCorruptLoadRelease(t *testing.T) {
	s := openTestStore(t)
	_, err := Open(t.Context(), s.path, testOptions())
	require.ErrorIs(t, err, atomicfile.ErrLocked)
	require.NoError(t, s.Close())
	require.NoError(t, os.WriteFile(s.path, []byte(`{"version":1,"initialized":false,"people":{},"grants":{},"unexpected":true}`), 0o600))
	_, err = Open(t.Context(), s.path, testOptions())
	require.Error(t, err)
	require.NoError(t, os.Remove(s.path))
	reopened, err := Open(t.Context(), s.path, testOptions())
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
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
	assert.False(t, s.Progress().ResponseVerified)
	assert.False(t, s.Progress().AppConnected)
	require.ErrorIs(t, s.MarkResponse(t.Context(), "another-person"), ErrNotFound)
	require.NoError(t, s.MarkResponse(t.Context(), owner.ID))
	require.NoError(t, s.MarkAppConnected(t.Context()))
	assert.True(t, s.Progress().ResponseVerified)
	assert.True(t, s.Progress().AppConnected)
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
	require.Len(t, s.List(), 2)
	require.NoError(t, s.Close())
	reopened, err := Open(t.Context(), s.path, testOptions())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	_, err = reopened.Inspect(t.Context(), bound.Token)
	require.ErrorIs(t, err, ErrInvalidGrant)
	assert.Equal(t, "accepted", reopened.Invitations()[0].Status)
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
	assert.Len(t, s.List(), 2)
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
	assert.False(t, s.Initialized())
}

func TestPostRenameWarningPublishesCommittedClaim(t *testing.T) {
	s := openTestStore(t)
	grant := setupGrant(t, s)
	s.write = func(path string, data []byte, mode os.FileMode) error {
		if err := atomicfile.WriteFile(path, data, mode); err != nil {
			return err
		}
		return atomicfile.ErrPostRenameDurability
	}
	person, err := s.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: "alice", Name: "Alice", Password: testPassword})
	require.NoError(t, err)
	assert.Equal(t, person, s.Owner())
	require.NoError(t, s.Close())
	reopened, err := Open(t.Context(), s.path, testOptions())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	assert.Equal(t, person, reopened.Owner())
	_, err = reopened.Inspect(t.Context(), grant.Token)
	require.ErrorIs(t, err, ErrInvalidGrant)
}
