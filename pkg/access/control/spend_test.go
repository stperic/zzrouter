package control

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/model/pricing"
)

var spendLimits = quota.QuotaConfig{SpendLimit: 5, ResetPeriod: "monthly", DefaultMaxTokens: 100}

// writeSpend leaves path as a stopped node's spend.json, one current row per scope.
func writeSpend(t *testing.T, path string, spent map[quota.SpendScope]int64) {
	t.Helper()
	file := &quota.SpendFile{Version: 1, States: make(map[string]*quota.SpendState, len(spent))}
	for scope, micro := range spent {
		file.States[scope.String()] = &quota.SpendState{Kind: scope.Kind, EntityID: scope.EntityID, SpendMicro: micro,
			Period: quota.PeriodMonthly, PeriodStart: quota.PeriodStartFor(time.Now(), quota.PeriodMonthly)}
	}
	require.NoError(t, quota.SaveSpend(path, file))
}

// startCore builds a node's access control over state and the spend.json at path.
func startCore(t *testing.T, state accessState, path string) *Core {
	t.Helper()
	enforcer := quota.NewEnforcer(quota.NewRateLimiter(), quota.NewConcurrencyLimiter(), quota.NewSpendTracker(path))
	c := NewCore(StaticKeySet{}, state, nil, enforcer, nil)
	t.Cleanup(func() { c.Stop(context.Background()) })
	return c
}

// spentBy maps every spend row c holds to its total.
func spentBy(c *Core) map[string]int64 {
	spent := map[string]int64{}
	for scope, st := range c.GetAllSpendStates() {
		spent[scope] = st.SpendMicro
	}
	return spent
}

// spendAs admits one priced request for the key raw names and settles costMicro.
func spendAs(t *testing.T, c *Core, raw string, costMicro int64) {
	t.Helper()
	ac, err := c.Authenticate(raw)
	require.NoError(t, err)
	decision, release, reservation := c.EnforceQuotas(ac, []pricing.Quote{{Rates: &pricing.ModelPricing{InputCostPerToken: 0.000001}}})
	require.True(t, decision.Allowed, decision.Message)
	release()
	c.RecordSpend(ac.Key.Subject(), reservation, costMicro, 1, 1)
}

func createTeam(t *testing.T, store access.Store, id string, limits quota.QuotaConfig) {
	t.Helper()
	_, err := store.CreateTeam(t.Context(), id, teams.Team{Name: id, Kind: "shared", QuotaConfig: limits}, audit.Event{})
	require.NoError(t, err)
}

func createKey(t *testing.T, store access.Store, id, teamID string, limits quota.QuotaConfig) string {
	t.Helper()
	_, raw, err := store.CreateKey(t.Context(), id, keys.VirtualKey{Name: id, Role: "user", TeamID: teamID, TeamRole: "member", QuotaConfig: limits}, audit.Event{})
	require.NoError(t, err)
	return raw
}

func TestStartForgetsSpendOfKeysAndTeamsTheStoreLacks(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, sqlstore.Options{})
	createTeam(t, store, "kept-team", quota.QuotaConfig{})
	createKey(t, store, "kept-key", "kept-team", quota.QuotaConfig{})
	path := filepath.Join(t.TempDir(), "spend.json")
	writeSpend(t, path, map[quota.SpendScope]int64{
		{Kind: quota.ScopeKey, EntityID: "kept-key"}: 1, {Kind: quota.ScopeTeam, EntityID: "kept-team"}: 2,
		{Kind: quota.ScopeKey, EntityID: "gone-key"}: 3, {Kind: quota.ScopeTeam, EntityID: "gone-team"}: 4,
		{Kind: quota.ScopePerson, EntityID: "gone-person"}: 5,
	})

	assert.Equal(t, map[string]int64{"key:kept-key:": 1, "team:kept-team:": 2, "person:gone-person:": 5},
		spentBy(startCore(t, store, path)), "person rows follow neither the commit subscriber nor the start")
}

// The delete committed, then the process died before the 60-second spend flush.
func TestStartForgetsSpendOfAKeyDeletedBeforeTheFlush(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "spend.json")
	store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	createTeam(t, store, "eng", spendLimits)
	raw := createKey(t, store, "gone", "eng", spendLimits)
	enforcer := quota.NewEnforcer(quota.NewRateLimiter(), quota.NewConcurrencyLimiter(), quota.NewSpendTracker(path))
	first := NewCore(StaticKeySet{}, store, nil, enforcer, nil)
	spendAs(t, first, raw, 1_000)
	first.FlushSpend()
	require.NoError(t, store.DeleteKey(t.Context(), "gone", audit.Event{}))
	require.Nil(t, first.GetKeySpend("gone"), "the commit subscriber forgets the row in memory only")
	require.NoError(t, store.Close()) // killed: first never stops, so nothing flushes again
	file, err := quota.LoadSpend(path)
	require.NoError(t, err)
	require.Contains(t, file.States, "key:gone:")

	store, err = sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	second := startCore(t, store, path)
	assert.Equal(t, map[string]int64{"team:eng:": 1_000}, spentBy(second))
	second.FlushSpend()
	file, err = quota.LoadSpend(path)
	require.NoError(t, err)
	assert.NotContains(t, file.States, "key:gone:", "the next flush persists the purge")
	assert.Contains(t, file.States, "team:eng:")
}

func TestKeyCreatedAgainUnderAnOldIDStartsAtZeroSpend(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, sqlstore.Options{})
	createTeam(t, store, "eng", quota.QuotaConfig{})
	path := filepath.Join(t.TempDir(), "spend.json")
	writeSpend(t, path, map[quota.SpendScope]int64{{Kind: quota.ScopeKey, EntityID: "reused"}: 5_000_000})
	core := startCore(t, store, path)

	raw := createKey(t, store, "reused", "eng", spendLimits)
	require.Nil(t, core.GetKeySpend("reused"))
	spendAs(t, core, raw, 1_000) // the old row alone exhausts the new key's $5
	assert.Equal(t, int64(1_000), core.GetKeySpend("reused").SpendMicro)
}

func TestStartKeepsSpendWhenNoStoreVouchesForAbsence(t *testing.T) {
	t.Parallel()
	for name, state := range map[string]accessState{
		"worker":       nil,
		"failed store": &fixedState{snap: access.FailedSnapshot()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "spend.json")
			writeSpend(t, path, map[quota.SpendScope]int64{
				{Kind: quota.ScopeKey, EntityID: "k"}: 1, {Kind: quota.ScopeTeam, EntityID: "t"}: 2, {Kind: quota.ScopePerson, EntityID: "p"}: 3,
			})
			assert.Equal(t, map[string]int64{"key:k:": 1, "team:t:": 2, "person:p:": 3}, spentBy(startCore(t, state, path)))
		})
	}
}
