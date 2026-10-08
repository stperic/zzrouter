package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
)

// newGatewayWithEnforcer builds a minimal AccessControl backed by real
// pkg/quota subsystems so tests can exercise spend lookup / reset and the
// pending-reservation reaper end-to-end. No file persistence, no auth cache
// goroutine — tests only cover the gateway's spend surface.
func newGatewayWithEnforcer(t *testing.T) (*AccessControl, *quota.Enforcer) {
	t.Helper()

	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	teamStore := teams.NewFileTeamStore(filepath.Join(dir, "teams.yaml"))

	rl := quota.NewRateLimiter()
	cl := quota.NewConcurrencyLimiter()
	st := quota.NewSpendTracker("")
	enforcer := quota.NewEnforcer(rl, cl, st)

	gw := NewAccessControl(StaticKeySet{}, keyStore, teamStore, nil, enforcer, nil, ModelHooks{})
	// Note: we deliberately do NOT call Start() here — the reaper test drives
	// reapStalePendingReservations directly with an injected "now".
	t.Cleanup(func() { gw.Stop(context.Background()) })
	return gw, enforcer
}

// ============================================================================
// Gateway spend lookup
// ============================================================================

func TestAccessControl_GetKeySpend_ReflectsSettledTraffic(t *testing.T) {
	gw, enforcer := newGatewayWithEnforcer(t)

	// Pre-condition: no traffic yet.
	assert.Nil(t, gw.GetKeySpend("alice"))

	// Drive a reserve+settle cycle through the enforcer as the dispatcher
	// would. $0.50 actual cost, 100 input / 200 output tokens.
	chain := []quota.QuotaScope{{
		Scope:    quota.ScopeKey,
		EntityID: "alice",
		Quotas: quota.QuotaConfig{
			SpendLimit:       10.00,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 500,
		},
	}}
	_, reservations := enforcer.Check(chain)
	enforcer.SettleReservations(reservations, 500_000, 100, 200)

	state := gw.GetKeySpend("alice")
	require.NotNil(t, state)
	assert.InDelta(t, 0.50, state.SpendUSD(), 1e-9)
	assert.EqualValues(t, 100, state.TokensIn)
	assert.EqualValues(t, 200, state.TokensOut)
	assert.EqualValues(t, 1, state.RequestCount)
	assert.Equal(t, quota.ScopeKey, state.Kind)
	assert.Equal(t, "alice", state.EntityID)
}

func TestAccessControl_GetTeamSpend_Independent(t *testing.T) {
	gw, enforcer := newGatewayWithEnforcer(t)

	// Multi-scope chain: key AND team both reserve.
	chain := []quota.QuotaScope{
		{
			Scope:    quota.ScopeKey,
			EntityID: "alice",
			Quotas: quota.QuotaConfig{
				SpendLimit:       5.00,
				ResetPeriod:      "monthly",
				DefaultMaxTokens: 100,
			},
		},
		{
			Scope:    quota.ScopeTeam,
			EntityID: "eng",
			Quotas: quota.QuotaConfig{
				SpendLimit:       20.00,
				ResetPeriod:      "monthly",
				DefaultMaxTokens: 100,
			},
		},
	}
	_, reservations := enforcer.Check(chain)
	// Each scope in the chain has its own SpendState ledger; SettleReservations
	// applies the full actualCost to each scope (key AND team each spent $0.25).
	enforcer.SettleReservations(reservations, 250_000, 50, 100)

	keyState := gw.GetKeySpend("alice")
	require.NotNil(t, keyState)
	assert.InDelta(t, 0.25, keyState.SpendUSD(), 1e-9)

	teamState := gw.GetTeamSpend("eng")
	require.NotNil(t, teamState)
	assert.InDelta(t, 0.25, teamState.SpendUSD(), 1e-9)

	// Empty queries don't cross contaminate.
	assert.Nil(t, gw.GetKeySpend("bob"))
	assert.Nil(t, gw.GetTeamSpend("ops"))
}

func TestAccessControl_ResetKeySpend_ZerosLedger(t *testing.T) {
	gw, enforcer := newGatewayWithEnforcer(t)

	chain := []quota.QuotaScope{{
		Scope:    quota.ScopeKey,
		EntityID: "alice",
		Quotas: quota.QuotaConfig{
			SpendLimit:       10.00,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 100,
		},
	}}
	_, reservations := enforcer.Check(chain)
	enforcer.SettleReservations(reservations, 123_456, 10, 20)

	require.NotNil(t, gw.GetKeySpend("alice"))

	gw.ResetKeySpend("alice")

	state := gw.GetKeySpend("alice")
	require.NotNil(t, state)
	assert.EqualValues(t, 0, state.SpendMicro)
	assert.EqualValues(t, 0, state.TokensIn)
	assert.EqualValues(t, 0, state.RequestCount)
}

func TestAccessControl_GetAllSpendStates_AllScopes(t *testing.T) {
	gw, enforcer := newGatewayWithEnforcer(t)

	// One key-only request, one team-only request, one multi-scope request.
	tracks := [][]quota.QuotaScope{
		{{Scope: quota.ScopeKey, EntityID: "alice",
			Quotas: quota.QuotaConfig{SpendLimit: 5, ResetPeriod: "monthly", DefaultMaxTokens: 100}}},
		{{Scope: quota.ScopeTeam, EntityID: "ops",
			Quotas: quota.QuotaConfig{SpendLimit: 5, ResetPeriod: "monthly", DefaultMaxTokens: 100}}},
		{
			{Scope: quota.ScopeKey, EntityID: "bob",
				Quotas: quota.QuotaConfig{SpendLimit: 5, ResetPeriod: "monthly", DefaultMaxTokens: 100}},
			{Scope: quota.ScopeTeam, EntityID: "eng",
				Quotas: quota.QuotaConfig{SpendLimit: 5, ResetPeriod: "monthly", DefaultMaxTokens: 100}},
		},
	}
	for _, c := range tracks {
		_, reservations := enforcer.Check(c)
		enforcer.SettleReservations(reservations, 100_000, 10, 10)
	}

	all := gw.GetAllSpendStates()

	// Expect 4 rows: alice/key, bob/key, ops/team, eng/team.
	require.Len(t, all, 4)

	kinds := map[quota.ScopeKind]int{}
	ids := map[string]bool{}
	for _, s := range all {
		kinds[s.Kind]++
		ids[s.EntityID] = true
	}
	assert.Equal(t, 2, kinds[quota.ScopeKey])
	assert.Equal(t, 2, kinds[quota.ScopeTeam])
	assert.True(t, ids["alice"])
	assert.True(t, ids["bob"])
	assert.True(t, ids["ops"])
	assert.True(t, ids["eng"])
}

// ============================================================================
// KeysService / TeamsService end-to-end spend reporting
// ============================================================================

func TestKeysService_GetKeyUsage_ReturnsLiveSpend(t *testing.T) {
	gw, enforcer := newGatewayWithEnforcer(t)

	// Seed the key store.
	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	_, err := keyStore.Create("alice", &keys.VirtualKey{Name: "Alice", Role: "user", TeamID: "t", TeamRole: "owner"})
	require.NoError(t, err)

	svc := NewKeysService(keyStore, nil, gw)

	// Empty state — usage endpoint should succeed with zeroes, not 501.
	usage, err := svc.GetKeyUsage("alice")
	require.NoError(t, err)
	assert.EqualValues(t, 0, usage.SpendUSD)
	assert.EqualValues(t, 0, usage.RequestCount)

	// Drive some traffic through the gateway's enforcer.
	chain := []quota.QuotaScope{{
		Scope:    quota.ScopeKey,
		EntityID: "alice",
		Quotas: quota.QuotaConfig{
			SpendLimit:       10.0,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 100,
		},
	}}
	_, reservations := enforcer.Check(chain)
	enforcer.SettleReservations(reservations, 750_000, 50, 150)

	usage, err = svc.GetKeyUsage("alice")
	require.NoError(t, err)
	assert.InDelta(t, 0.75, usage.SpendUSD, 1e-9)
	assert.EqualValues(t, 50, usage.TokensIn)
	assert.EqualValues(t, 150, usage.TokensOut)
	assert.EqualValues(t, 1, usage.RequestCount)
	assert.Equal(t, "monthly", usage.Period)
	assert.NotEmpty(t, usage.PeriodStart)
}

func TestKeysService_ResetKeyUsage_ZerosLedger(t *testing.T) {
	gw, enforcer := newGatewayWithEnforcer(t)

	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	_, err := keyStore.Create("alice", &keys.VirtualKey{Name: "Alice", Role: "user", TeamID: "t", TeamRole: "owner"})
	require.NoError(t, err)
	svc := NewKeysService(keyStore, nil, gw)

	chain := []quota.QuotaScope{{
		Scope:    quota.ScopeKey,
		EntityID: "alice",
		Quotas: quota.QuotaConfig{
			SpendLimit:       10.0,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 100,
		},
	}}
	_, reservations := enforcer.Check(chain)
	enforcer.SettleReservations(reservations, 500_000, 10, 20)

	require.NoError(t, svc.ResetKeyUsage("alice", "test"))

	usage, err := svc.GetKeyUsage("alice")
	require.NoError(t, err)
	assert.EqualValues(t, 0, usage.SpendUSD)
	assert.EqualValues(t, 0, usage.TokensIn)
}

func TestKeysService_GetSpendReport_AggregatesKeysOnly(t *testing.T) {
	gw, enforcer := newGatewayWithEnforcer(t)

	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	_, err := keyStore.Create("alice", &keys.VirtualKey{Name: "Alice", Role: "user", TeamID: "t", TeamRole: "owner"})
	require.NoError(t, err)
	_, err = keyStore.Create("bob", &keys.VirtualKey{Name: "Bob", Role: "user", TeamID: "t", TeamRole: "owner"})
	require.NoError(t, err)
	svc := NewKeysService(keyStore, nil, gw)

	// Two key-scope requests + one team-scope request. The report must
	// include only the key rows.
	for _, entity := range []string{"alice", "bob"} {
		chain := []quota.QuotaScope{{
			Scope:    quota.ScopeKey,
			EntityID: entity,
			Quotas:   quota.QuotaConfig{SpendLimit: 5, ResetPeriod: "monthly", DefaultMaxTokens: 100},
		}}
		_, reservations := enforcer.Check(chain)
		enforcer.SettleReservations(reservations, 250_000, 10, 10)
	}
	teamChain := []quota.QuotaScope{{
		Scope:    quota.ScopeTeam,
		EntityID: "eng",
		Quotas:   quota.QuotaConfig{SpendLimit: 5, ResetPeriod: "monthly", DefaultMaxTokens: 100},
	}}
	_, reservations := enforcer.Check(teamChain)
	enforcer.SettleReservations(reservations, 100_000, 5, 5)

	report := svc.GetSpendReport()
	require.NotNil(t, report)
	assert.Len(t, report.Keys, 2, "report should only contain key-scope rows")
	assert.InDelta(t, 0.50, report.TotalSpend, 1e-9)

	ids := map[string]bool{}
	for _, k := range report.Keys {
		ids[k.KeyID] = true
	}
	assert.True(t, ids["alice"])
	assert.True(t, ids["bob"])
}

func TestTeamsService_GetTeamUsage_ReturnsLiveSpend(t *testing.T) {
	gw, enforcer := newGatewayWithEnforcer(t)

	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	teamStore := teams.NewFileTeamStore(filepath.Join(dir, "teams.yaml"))
	svc := NewTeamsService(teamStore, keyStore, gw)

	_, err := svc.CreateTeam(&CreateTeamRequest{
		ID: "eng", Name: "Engineering", SpendLimit: 100.0, ResetPeriod: "monthly",
	}, "admin")
	require.NoError(t, err)

	// Zero-traffic usage returns successfully.
	usage, err := svc.GetTeamUsage("eng")
	require.NoError(t, err)
	assert.EqualValues(t, 0, usage.SpendUSD)
	assert.EqualValues(t, 100.0, usage.SpendLimit)

	// Drive team-scope traffic.
	chain := []quota.QuotaScope{{
		Scope:    quota.ScopeTeam,
		EntityID: "eng",
		Quotas: quota.QuotaConfig{
			SpendLimit:       100,
			ResetPeriod:      "monthly",
			DefaultMaxTokens: 100,
		},
	}}
	_, reservations := enforcer.Check(chain)
	enforcer.SettleReservations(reservations, 2_000_000, 200, 400)

	usage, err = svc.GetTeamUsage("eng")
	require.NoError(t, err)
	assert.InDelta(t, 2.0, usage.SpendUSD, 1e-9)
	assert.EqualValues(t, 200, usage.TokensIn)
	assert.EqualValues(t, 400, usage.TokensOut)
	assert.EqualValues(t, 1, usage.RequestCount)
}

func TestTeamsService_ResetTeamUsage_ZerosLedger(t *testing.T) {
	gw, enforcer := newGatewayWithEnforcer(t)

	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	teamStore := teams.NewFileTeamStore(filepath.Join(dir, "teams.yaml"))
	svc := NewTeamsService(teamStore, keyStore, gw)

	_, err := svc.CreateTeam(&CreateTeamRequest{ID: "eng", Name: "Engineering"}, "admin")
	require.NoError(t, err)

	chain := []quota.QuotaScope{{
		Scope:    quota.ScopeTeam,
		EntityID: "eng",
		Quotas:   quota.QuotaConfig{SpendLimit: 10, ResetPeriod: "monthly", DefaultMaxTokens: 100},
	}}
	_, reservations := enforcer.Check(chain)
	enforcer.SettleReservations(reservations, 1_500_000, 50, 75)

	require.NoError(t, svc.ResetTeamUsage("eng", "test"))

	usage, err := svc.GetTeamUsage("eng")
	require.NoError(t, err)
	assert.EqualValues(t, 0, usage.SpendUSD)
	assert.EqualValues(t, 0, usage.TokensIn)
	assert.EqualValues(t, 0, usage.RequestCount)
}

func TestTeamsService_GetTeamUsage_NotFound(t *testing.T) {
	gw, _ := newGatewayWithEnforcer(t)

	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	teamStore := teams.NewFileTeamStore(filepath.Join(dir, "teams.yaml"))
	svc := NewTeamsService(teamStore, keyStore, gw)

	_, err := svc.GetTeamUsage("ghost")
	require.Error(t, err)
	assert.ErrorIs(t, err, teams.ErrTeamNotFound)
}
