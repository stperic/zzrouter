package people

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stretchr/testify/require"
)

type auditRecorder struct{ events []audit.Event }

func (r *auditRecorder) Emit(_ context.Context, event audit.Event) error {
	r.events = append(r.events, event)
	return nil
}

func TestPeopleAuditCommittedChanges(t *testing.T) {
	s := openTestStore(t)
	recorder := &auditRecorder{}
	s.SetAuditSink(recorder)
	owner := claimTestOwner(t, s)
	require.Len(t, recorder.events, 2)
	require.Equal(t, audit.ActionPersonCreated, recorder.events[1].Action)
	require.Equal(t, owner.ID, recorder.events[1].Actor)
	ctx := audit.WithIdentity(t.Context(), owner.ID, "test-request")
	invite, err := s.Issue(ctx, GrantRequest{Purpose: PurposeInvitation, TeamID: "engineering", Role: "user", Issuer: Issuer{Reference: "installation"}})
	require.NoError(t, err)
	require.Equal(t, "test-request", recorder.events[2].RequestID)
	require.Equal(t, owner.ID, recorder.events[2].Actor)
	require.NoError(t, s.RevokeGrant(ctx, invite.ID))
	require.NoError(t, s.RevokeGrant(ctx, invite.ID))
	require.Len(t, recorder.events, 4, "no event for idempotent revocation")
	name := "Changed"
	write := s.write
	s.write = func(string, []byte, os.FileMode) error { return errors.New("persistence failed") }
	_, err = s.Update(ctx, owner.ID, UpdatePerson{Name: &name})
	require.Error(t, err)
	require.Len(t, recorder.events, 4)
	s.write = func(path string, data []byte, mode os.FileMode) error {
		if err := write(path, data, mode); err != nil {
			return err
		}
		return atomicfile.ErrPostRenameDurability
	}
	_, err = s.Update(ctx, owner.ID, UpdatePerson{Name: &name})
	require.NoError(t, err)
	require.Len(t, recorder.events, 5, "committed state is audited despite uncertain directory durability")
	data, err := json.Marshal(recorder.events)
	require.NoError(t, err)
	for _, secret := range []string{testPassword, testProof, owner.Revision, invite.Token, "installation", "Changed"} {
		require.NotContains(t, string(data), secret)
	}
}

func TestTeamImpactIncludesPendingEnrollment(t *testing.T) {
	s := openTestStore(t)
	grant := setupGrant(t, s)
	impact := s.TeamImpact("workspace")
	require.Equal(t, 1, impact.EnrollmentGrants)
	require.Zero(t, impact.People)
	owner, err := s.ClaimOwner(t.Context(), grant.Token, testProof, CreatePerson{Username: "owner", Name: "Owner", Password: testPassword})
	require.NoError(t, err)
	impact = s.TeamImpact("workspace")
	require.Equal(t, 1, impact.People)
	require.Zero(t, impact.EnrollmentGrants)
	require.True(t, impact.OwnerProtected)
	invite, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeInvitation, TeamID: "engineering", Role: "user", Issuer: Issuer{Reference: "installation"}})
	require.NoError(t, err)
	require.Equal(t, 1, s.TeamImpact("engineering").Invitations)
	require.NoError(t, s.RevokeGrant(t.Context(), invite.ID))
	require.False(t, s.TeamImpact("engineering").HasMembers())
	_, err = s.Issue(t.Context(), GrantRequest{Purpose: PurposeRecovery, TeamID: "workspace", PersonID: owner.ID, Issuer: Issuer{Reference: "local-recovery"}})
	require.NoError(t, err)
	require.Equal(t, 1, s.TeamImpact("workspace").EnrollmentGrants)
}
