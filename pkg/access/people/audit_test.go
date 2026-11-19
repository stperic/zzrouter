package people

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stretchr/testify/require"
)

func TestPeopleAuditCommittedChanges(t *testing.T) {
	s := openTestStore(t)
	owner := claimTestOwner(t, s)
	require.Len(t, s.owner.recorded(), 2)
	require.Equal(t, audit.ActionPersonCreated, s.owner.recorded()[1].Action)
	require.Equal(t, owner.ID, s.owner.recorded()[1].Actor)
	ctx := audit.WithIdentity(t.Context(), owner.ID, "test-request")
	invite, err := s.Issue(ctx, GrantRequest{Purpose: PurposeInvitation, TeamID: "engineering", Role: "user", Issuer: Issuer{Reference: "installation"}})
	require.NoError(t, err)
	require.Equal(t, "test-request", s.owner.recorded()[2].RequestID)
	require.Equal(t, owner.ID, s.owner.recorded()[2].Actor)
	require.NoError(t, s.RevokeGrant(ctx, invite.ID))
	require.NoError(t, s.RevokeGrant(ctx, invite.ID))
	require.Len(t, s.owner.recorded(), 4, "no event for idempotent revocation")
	name := "Changed"
	s.owner.failPersists(errors.New("persistence failed"))
	_, err = s.Update(ctx, owner.ID, UpdatePerson{Name: &name})
	require.Error(t, err)
	require.Len(t, s.owner.recorded(), 4)
	s.owner.failPersists(nil)
	_, err = s.Update(ctx, owner.ID, UpdatePerson{Name: &name})
	require.NoError(t, err)
	require.Len(t, s.owner.recorded(), 5, "the committed retry is audited once")
	require.Equal(t, "test-request", s.owner.recorded()[4].RequestID)
	data, err := json.Marshal(s.owner.recorded())
	require.NoError(t, err)
	for _, secret := range []string{testPassword, testProof, owner.Revision, invite.Token, "installation", "Changed"} {
		require.NotContains(t, string(data), secret)
	}
}

func TestTeamImpactIncludesPendingEnrollment(t *testing.T) {
	s := openTestStore(t)
	grant := setupGrant(t, s)
	impact := s.state().TeamImpact("workspace")
	require.Equal(t, 1, impact.EnrollmentGrants)
	require.Zero(t, impact.People)
	owner, err := s.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: "owner", Name: "Owner", Password: testPassword})
	require.NoError(t, err)
	impact = s.state().TeamImpact("workspace")
	require.Equal(t, 1, impact.People)
	require.Zero(t, impact.EnrollmentGrants)
	require.True(t, impact.OwnerProtected)
	invite, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeInvitation, TeamID: "engineering", Role: "user", Issuer: Issuer{Reference: "installation"}})
	require.NoError(t, err)
	require.Equal(t, 1, s.state().TeamImpact("engineering").Invitations)
	require.NoError(t, s.RevokeGrant(t.Context(), invite.ID))
	require.False(t, s.state().TeamImpact("engineering").HasMembers())
	_, err = s.Issue(t.Context(), GrantRequest{Purpose: PurposeRecovery, TeamID: "workspace", PersonID: owner.ID, Issuer: Issuer{Reference: "local-recovery"}})
	require.NoError(t, err)
	require.Equal(t, 1, s.state().TeamImpact("workspace").EnrollmentGrants)
}
