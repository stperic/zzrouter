package control

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixtureProof = "a validated fixture browser proof"

// identityFixture enrolls a real member in engineering beside an app key there.
func identityFixture(t *testing.T, limits quota.QuotaConfig) (*Core, access.Store, CredentialReference, CredentialReference) {
	t.Helper()
	store := openTestStore(t, sqlstore.Options{
		People:                 true,
		ValidateExternalIssuer: func(context.Context, string) error { return nil },
	})
	core := newTestCoreFor(t, StaticKeySet{Admin: "test-installation-admin-with-sufficient-length"}, store)
	for id, team := range map[string]teams.Team{"owners": {Name: "Owners", Kind: "shared"}, "engineering": {Name: "Engineering", Kind: "shared", QuotaConfig: limits}} {
		_, err := store.CreateTeam(t.Context(), id, team, audit.Event{})
		require.NoError(t, err)
	}
	_, raw, err := store.CreateKey(t.Context(), "editor", keys.VirtualKey{Name: "Editor", Role: "user", TeamID: "engineering", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	keyRef, err := core.Reference(raw)
	require.NoError(t, err)
	enroll(t, store.People(), people.PurposeSetup, "owners", "admin", "owner")
	personRef, err := core.PersonReference(enroll(t, store.People(), people.PurposeInvitation, "engineering", "user", "alice"))
	require.NoError(t, err)
	return core, store, personRef, keyRef
}

func enroll(t *testing.T, p access.People, purpose people.Purpose, teamID, role, username string) *people.Person {
	t.Helper()
	grant, err := p.Issue(t.Context(), people.GrantRequest{Purpose: purpose, TeamID: teamID, Role: role, Issuer: people.Issuer{Reference: "installation"}})
	require.NoError(t, err)
	bound, err := p.Exchange(t.Context(), grant.Token, fixtureProof)
	require.NoError(t, err)
	input := people.CreatePerson{Username: username, Name: username, Password: fixturePassword}
	enrolled := p.AcceptInvitation
	if purpose == people.PurposeSetup {
		enrolled = p.ClaimOwner
	}
	person, err := enrolled(t.Context(), bound.Token, fixtureProof, input)
	require.NoError(t, err)
	return person
}

const fixturePassword = "a long fixture password"

func updateTeam(t *testing.T, store access.Store, apply func(*teams.Team)) {
	t.Helper()
	_, err := store.UpdateTeam(t.Context(), "engineering", func(team *teams.Team) error { apply(team); return nil }, audit.Event{})
	require.NoError(t, err)
}

func TestPeopleAndKeysShareProviderModelAndRoutePolicy(t *testing.T) {
	t.Parallel()
	core, store, personRef, keyRef := identityFixture(t, quota.QuotaConfig{})
	updateTeam(t, store, func(team *teams.Team) {
		team.AllowedModels = []string{"qwen"}
		team.ProviderAccess = teams.ProviderAccess{Mode: "selected", Providers: []string{"ollama"}}
	})
	for _, ref := range []CredentialReference{personRef, keyRef} {
		ac, err := core.AuthenticateReference(ref)
		require.NoError(t, err)
		assert.True(t, ac.Key.TeamBound())
		assert.True(t, core.Authorize(ac, "qwen").Allowed)
		assert.False(t, core.Authorize(ac, "other-model").Allowed)
		assert.True(t, ProviderAllowed(ac, "ollama"))
		assert.False(t, ProviderAllowed(ac, "groq"), "humans must not take the static-key unrestricted branch")
		assert.False(t, ProviderAllowed(ac, ""))
	}
	core.authorizer.groups = stubGroups{"coding": &modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "qwen-local", App: "ollama", Model: "qwen"}}}}
	updateTeam(t, store, func(team *teams.Team) {
		team.RouteAccess = teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}}
	})
	for _, ref := range []CredentialReference{personRef, keyRef} {
		ac, err := core.AuthenticateReference(ref)
		require.NoError(t, err)
		assert.True(t, RoutesRestricted(ac))
		assert.True(t, core.Authorize(ac, "coding").Allowed)
		assert.False(t, core.Authorize(ac, "qwen").Allowed, "direct requests must not bypass assigned routes")
		assert.True(t, core.DestinationAllowed(ac, "qwen", "ollama"))
		assert.False(t, core.DestinationAllowed(ac, "other-model", "ollama"))
		assert.False(t, core.DestinationAllowed(ac, "qwen", "groq"))
		assert.False(t, RouteResolved(ac, "coding", ""))
	}
}

func TestPeopleAndKeysShareTeamQuotas(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		limits       quota.QuotaConfig
		denial       DenialReason
		cost, tokens int64
	}{
		{"requests", quota.QuotaConfig{RPMLimit: 1}, DenyRPM, 0, 0},
		{"tokens", quota.QuotaConfig{TPMLimit: 10}, DenyTPM, 0, 10},
		{"spend", quota.QuotaConfig{SpendLimit: 0.01}, DenyBudget, 9_000, 0},
		{"parallel", quota.QuotaConfig{MaxParallelRequests: 1}, DenyConcurrency, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, personFirst := range []bool{true, false} {
				core, _, personRef, keyRef := identityFixture(t, test.limits)
				first, second := personRef, keyRef
				if !personFirst {
					first, second = second, first
				}
				ac, err := core.AuthenticateReference(first)
				require.NoError(t, err)
				decision, release, reservation := core.EnforceQuotas(ac, []pricing.Quote{{Rates: &pricing.ModelPricing{InputCostPerToken: 0.01}}})
				require.True(t, decision.Allowed)
				defer release()
				core.RecordSpend(ac.Key.Subject(), reservation, test.cost, test.tokens, 0)
				other, err := core.AuthenticateReference(second)
				require.NoError(t, err)
				decision, otherRelease, _ := core.EnforceQuotas(other, []pricing.Quote{{Rates: &pricing.ModelPricing{InputCostPerToken: 0.01}}})
				require.False(t, decision.Allowed, "a person and app must share the team's %s limit", test.name)
				assert.Equal(t, test.denial, decision.Reason)
				assert.Equal(t, "team", decision.Scope)
				assert.Equal(t, "engineering", decision.EntityID)
				assert.Nil(t, otherRelease)
			}
		})
	}
}

func TestPeopleSettlementKeepsAdmittedTeamAfterMembershipChange(t *testing.T) {
	t.Parallel()
	core, store, ref, keyRef := identityFixture(t, quota.QuotaConfig{SpendLimit: 1, TPMLimit: 50})
	_, err := store.CreateTeam(t.Context(), "research", teams.Team{Name: "Research", Kind: "shared", QuotaConfig: quota.QuotaConfig{SpendLimit: 1, TPMLimit: 50}}, audit.Event{})
	require.NoError(t, err)
	admitted, err := core.AuthenticateReference(ref)
	require.NoError(t, err)
	subject := admitted.Key.Subject()
	decision, release, reservation := core.EnforceQuotas(admitted, []pricing.Quote{{Rates: &pricing.ModelPricing{InputCostPerToken: 0.001}}})
	require.True(t, decision.Allowed)
	defer release()
	require.NotZero(t, reservation)
	research := "research"
	moved, err := store.People().Update(t.Context(), ref.ID, people.UpdatePerson{TeamID: &research})
	require.NoError(t, err)
	_, err = core.AuthenticateReference(ref)
	require.ErrorIs(t, err, ErrInvalidKey)
	currentRef, err := core.PersonReference(moved)
	require.NoError(t, err)
	core.RecordSpend(subject, reservation, 12_345, 30, 20)
	oldUsage := core.GetTeamSpend("engineering")
	require.NotNil(t, oldUsage)
	assert.Equal(t, int64(12_345), oldUsage.SpendMicro)
	assert.Equal(t, int64(30), oldUsage.TokensIn)
	assert.Equal(t, int64(20), oldUsage.TokensOut)
	assert.Zero(t, oldUsage.ReservedMicro)
	assert.Nil(t, core.GetTeamSpend("research"), "membership edits cannot move an in-flight charge")
	oldTeamKey, err := core.AuthenticateReference(keyRef)
	require.NoError(t, err)
	decision, _, _ = core.EnforceQuotas(oldTeamKey, nil)
	assert.Equal(t, DenyTPM, decision.Reason, "TPM settlement must use the admitted team too")
	current, err := core.AuthenticateReference(currentRef)
	require.NoError(t, err)
	decision, releaseCurrent, currentReservation := core.EnforceQuotas(current, []pricing.Quote{{Rates: &pricing.ModelPricing{}}})
	require.True(t, decision.Allowed, "the new team's token allowance has not been consumed")
	releaseCurrent()
	core.CancelPending(current.Key.ReservationKey(), currentReservation)
}

func TestPeopleReferencesFailClosedForChangedAuthorityAndTeams(t *testing.T) {
	t.Parallel()
	core, store, personRef, keyRef := identityFixture(t, quota.QuotaConfig{})
	oldVerified := store.Snapshot().Person(personRef.ID)
	changed, err := store.People().ChangePassword(t.Context(), oldVerified.ID, oldVerified.Revision, fixturePassword, "another long fixture password")
	require.NoError(t, err)
	_, err = core.PersonReference(oldVerified)
	require.ErrorIs(t, err, ErrInvalidKey, "an old-password result cannot silently bind a newer credential revision")
	personRef, err = core.PersonReference(changed)
	require.NoError(t, err)
	suspend := true
	suspended, err := store.People().Update(t.Context(), changed.ID, people.UpdatePerson{Suspended: &suspend})
	require.NoError(t, err)
	_, err = core.AuthenticateReference(personRef)
	require.ErrorIs(t, err, ErrInvalidKey, "suspension advances the revision")
	_, err = core.PersonReference(suspended)
	require.ErrorIs(t, err, ErrKeySuspended)
	suspend = false
	restored, err := store.People().Update(t.Context(), changed.ID, people.UpdatePerson{Suspended: &suspend})
	require.NoError(t, err)
	personRef, err = core.PersonReference(restored)
	require.NoError(t, err)
	updateTeam(t, store, func(team *teams.Team) { team.Suspended = true })
	for _, ref := range []CredentialReference{personRef, keyRef} {
		_, err := core.AuthenticateReference(ref)
		require.ErrorIs(t, err, ErrKeySuspended)
	}
	require.ErrorIs(t, store.DeleteTeam(t.Context(), "engineering", audit.Event{}), teams.ErrTeamHasMembers)
	// The store refuses a team removed under its members; references fail closed against one anyway.
	snap := store.Snapshot()
	remaining := maps.Collect(snap.Teams())
	delete(remaining, "engineering")
	gone := &fixedState{snap: access.NewSnapshot(snap.Generation(), snap.KeysVersion(), maps.Collect(snap.Keys()), remaining, snap.PeopleState())}
	for _, ref := range []CredentialReference{personRef, keyRef} {
		_, err := NewCore(StaticKeySet{}, gone, nil, nil, nil).AuthenticateReference(ref)
		require.ErrorIs(t, err, ErrInvalidKey)
	}
	for _, principal := range []*KeyPrincipal{{ID: "missing", Kind: KindPerson, TeamID: "removed"}, {ID: "missing", IsVirtual: true, TeamID: "removed"}} {
		ac := &AccessContext{Key: principal}
		assert.False(t, core.Authorize(ac, "any").Allowed)
		assert.False(t, ProviderAllowed(ac, "ollama"))
		assert.False(t, RouteResolved(ac, "coding", "coding"))
	}
	static, err := core.Authenticate("test-installation-admin-with-sufficient-length")
	require.NoError(t, err)
	assert.False(t, static.Key.TeamBound())
	assert.True(t, ProviderAllowed(static, "ollama"))
}

func TestPeopleReferenceKindPreservesLegacyJSON(t *testing.T) {
	t.Parallel()
	core, _, personRef, keyRef := identityFixture(t, quota.QuotaConfig{})
	data, err := json.Marshal(keyRef)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"kind"`)
	var legacy CredentialReference
	require.NoError(t, json.Unmarshal(data, &legacy))
	_, err = core.AuthenticateReference(legacy)
	require.NoError(t, err)
	data, err = json.Marshal(personRef)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"kind":"person"`)
	personRef.IsVirtual = true
	_, err = core.AuthenticateReference(personRef)
	require.ErrorIs(t, err, ErrInvalidKey)
}
