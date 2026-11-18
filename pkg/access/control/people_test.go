package control

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type peopleReader struct {
	mu     sync.Mutex
	person people.Person
}

func (r *peopleReader) Get(id string) *people.Person {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.person.ID != id {
		return nil
	}
	p := r.person
	return &p
}

func (r *peopleReader) replace(p people.Person) { r.mu.Lock(); defer r.mu.Unlock(); r.person = p }

func identityFixture(t *testing.T, limits quota.QuotaConfig) (*Core, *teams.FileTeamStore, *peopleReader, CredentialReference, CredentialReference) {
	t.Helper()
	core, keyStore, teamStore := newTestCore(t, StaticKeySet{Admin: "test-installation-admin-with-sufficient-length"})
	require.NoError(t, teamStore.Create("engineering", &teams.Team{Name: "Engineering", Kind: "shared", QuotaConfig: limits}))
	raw, err := keyStore.Create("editor", &keys.VirtualKey{Name: "Editor", Role: "user", TeamID: "engineering", TeamRole: "member"})
	require.NoError(t, err)
	keyRef, err := core.Reference(raw)
	require.NoError(t, err)
	reader := &peopleReader{person: people.Person{ID: "person-alice", Username: "alice", Name: "Alice", Role: "user", TeamID: "engineering", Revision: "verified-revision"}}
	core.SetPeople(reader)
	personRef, err := core.PersonReference(reader.Get("person-alice"))
	require.NoError(t, err)
	return core, teamStore, reader, personRef, keyRef
}

func TestPeopleAndKeysShareProviderModelAndRoutePolicy(t *testing.T) {
	core, store, _, personRef, keyRef := identityFixture(t, quota.QuotaConfig{})
	team := store.Get("engineering")
	team.AllowedModels = []string{"qwen"}
	team.ProviderAccess = teams.ProviderAccess{Mode: "selected", Providers: []string{"ollama"}}
	require.NoError(t, store.Update(team.ID, team))
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
	team.RouteAccess = teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}}
	require.NoError(t, store.Update(team.ID, team))
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
			for _, personFirst := range []bool{true, false} {
				core, _, _, personRef, keyRef := identityFixture(t, test.limits)
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
	core, store, reader, ref, keyRef := identityFixture(t, quota.QuotaConfig{SpendLimit: 1, TPMLimit: 50})
	require.NoError(t, store.Create("research", &teams.Team{Name: "Research", Kind: "shared", QuotaConfig: quota.QuotaConfig{SpendLimit: 1, TPMLimit: 50}}))
	admitted, err := core.AuthenticateReference(ref)
	require.NoError(t, err)
	subject := admitted.Key.Subject()
	decision, release, reservation := core.EnforceQuotas(admitted, []pricing.Quote{{Rates: &pricing.ModelPricing{InputCostPerToken: 0.001}}})
	require.True(t, decision.Allowed)
	defer release()
	require.NotZero(t, reservation)
	moved := *reader.Get(ref.ID)
	moved.TeamID, moved.Revision = "research", "changed-membership-revision"
	reader.replace(moved)
	_, err = core.AuthenticateReference(ref)
	require.ErrorIs(t, err, ErrInvalidKey)
	currentRef, err := core.PersonReference(&moved)
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
	core, store, reader, personRef, keyRef := identityFixture(t, quota.QuotaConfig{})
	oldVerified := reader.Get(personRef.ID)
	changed := *oldVerified
	changed.Revision = "reset-password-revision"
	reader.replace(changed)
	_, err := core.PersonReference(oldVerified)
	require.ErrorIs(t, err, ErrInvalidKey, "an old-password result cannot silently bind a newer credential revision")
	personRef, err = core.PersonReference(&changed)
	require.NoError(t, err)
	changed.Suspended = true
	reader.replace(changed)
	_, err = core.AuthenticateReference(personRef)
	require.ErrorIs(t, err, ErrKeySuspended)
	changed.Suspended = false
	reader.replace(changed)
	team := store.Get("engineering")
	team.Suspended = true
	require.NoError(t, store.Update(team.ID, team))
	for _, ref := range []CredentialReference{personRef, keyRef} {
		_, err := core.AuthenticateReference(ref)
		require.ErrorIs(t, err, ErrKeySuspended)
	}
	require.NoError(t, store.Delete(team.ID))
	for _, ref := range []CredentialReference{personRef, keyRef} {
		_, err := core.AuthenticateReference(ref)
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
	core, _, _, personRef, keyRef := identityFixture(t, quota.QuotaConfig{})
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
