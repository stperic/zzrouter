package people

import (
	"fmt"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestConsoleGrantIssuanceCannotExhaustOwnerRecovery(t *testing.T) {
	s := openTestStore(t)
	owner := claimTestOwner(t, s)
	issuer := Issuer{Reference: "installation"}
	for range 64 {
		_, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeSession, Issuer: issuer})
		require.NoError(t, err)
	}
	_, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeSession, Issuer: issuer})
	require.ErrorIs(t, err, ErrCapacity, "an individual issuer cannot consume the global grant pool")
	request := GrantRequest{Purpose: PurposeRecovery, PersonID: owner.ID, TeamID: owner.TeamID, Role: owner.Role, Issuer: Issuer{Reference: "local-recovery"}}
	_, err = s.Issue(t.Context(), request)
	require.NoError(t, err)
	// Even global normal-grant saturation leaves capacity for local recovery.
	saturated := s.state().Clone()
	saturated.grants = make(map[string]grantRecord)
	now := utils.NowUTC()
	for i := range maxGrants - 1 {
		saturated.grants[fmt.Sprintf("%064x", i)] = grantRecord{Grant: Grant{ID: fmt.Sprint(i), Purpose: PurposeInvitation, TeamID: "engineering", Role: "user", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, Issuer: issuer}
	}
	s.owner.current.Store(saturated)
	_, err = s.Issue(t.Context(), GrantRequest{Purpose: PurposeSession, Issuer: issuer})
	require.ErrorIs(t, err, ErrCapacity)
	first, err := s.Issue(t.Context(), request)
	require.NoError(t, err)
	second, err := s.Issue(t.Context(), request)
	require.NoError(t, err, "reissuing replaces the owner's old recovery instead of filling its reserved slot")
	_, err = s.Inspect(t.Context(), first.Token)
	require.ErrorIs(t, err, ErrInvalidGrant)
	_, err = s.Inspect(t.Context(), second.Token)
	require.NoError(t, err)
}

func TestConsumedConsoleCodesDoNotAccumulate(t *testing.T) {
	s := openTestStore(t)
	for range 70 {
		grant, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeSession, Issuer: Issuer{Reference: "installation"}})
		require.NoError(t, err)
		bound, err := s.Exchange(t.Context(), grant.Token, testProof)
		require.NoError(t, err)
		_, err = s.ConsumeSession(t.Context(), bound.Token, testProof)
		require.NoError(t, err)
		require.Len(t, s.state().grants, 1)
	}
}
