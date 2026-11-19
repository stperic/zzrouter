package control

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/accesstest"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
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

// dyingCore builds a node's access control over state and the spend.json at path
// for a process that dies: nothing stops it, so nothing flushes at the end.
func dyingCore(state accessState, path string) *Core {
	enforcer := quota.NewEnforcer(quota.NewRateLimiter(), quota.NewConcurrencyLimiter(), quota.NewSpendTracker(path))
	return NewCore(StaticKeySet{}, state, nil, enforcer, nil)
}

// startCore is dyingCore for a node that stops when the test ends.
func startCore(t *testing.T, state accessState, path string) *Core {
	t.Helper()
	c := dyingCore(state, path)
	t.Cleanup(func() { c.Stop(context.Background()) })
	return c
}

// spendRows reads the spend.json at path as a node's next start would.
func spendRows(t *testing.T, path string) map[string]*quota.SpendState {
	t.Helper()
	file, err := quota.LoadSpend(path)
	require.NoError(t, err)
	return file.States
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

// The delete committed, then the process died before its spend purge was written:
// the delete goes through a store no core subscribes to.
func TestStartForgetsSpendOfAKeyDeletedBeforeTheFlush(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "spend.json")
	store := openStoreAt(t, dir, sqlstore.Options{})
	createTeam(t, store, "eng", spendLimits)
	first := dyingCore(store, path)
	spendAs(t, first, createKey(t, store, "gone", "eng", spendLimits), 1_000)
	first.FlushSpend()
	require.NoError(t, store.Close())
	unwatched := openStoreAt(t, dir, sqlstore.Options{})
	require.NoError(t, unwatched.DeleteKey(t.Context(), "gone", audit.Event{}))
	require.NoError(t, unwatched.Close())
	require.Contains(t, spendRows(t, path), "key:gone:")

	second := startCore(t, openStoreAt(t, dir, sqlstore.Options{}), path)
	assert.Equal(t, map[string]int64{"team:eng:": 1_000}, spentBy(second))
	rows := spendRows(t, path)
	assert.NotContains(t, rows, "key:gone:", "the start writes its purge at once")
	assert.Contains(t, rows, "team:eng:")
}

// A key deleted and created again under its ID, then the process died before the
// periodic flush: the delete wrote its purge, so the new key inherits nothing.
func TestKeyRecreatedBeforeTheFlushStartsAtZeroAfterACrash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "spend.json")
	store := openStoreAt(t, dir, sqlstore.Options{})
	createTeam(t, store, "eng", quota.QuotaConfig{})
	first := dyingCore(store, path)
	spendAs(t, first, createKey(t, store, "k", "eng", spendLimits), 4_000_000)
	first.FlushSpend()
	require.NoError(t, store.DeleteKey(t.Context(), "k", audit.Event{}))
	createKey(t, store, "k", "eng", spendLimits)
	require.NoError(t, store.Close())

	second := startCore(t, openStoreAt(t, dir, sqlstore.Options{}), path)
	assert.Nil(t, second.GetKeySpend("k"))
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

// A reset purges the spend of every key and team it removes and writes the
// purge at once; the owner's team, which it keeps, keeps its spend.
func TestResetForgetsAndFlushesTheSpendOfWhatItRemoves(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "spend.json")
	store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{People: true, ValidateExternalIssuer: accesstest.ValidateExternalIssuer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	setup, err := store.IssueSetup(t.Context(), people.Issuer{Reference: "installation"})
	require.NoError(t, err)
	bound, err := store.People().Exchange(t.Context(), setup.Token, "browser")
	require.NoError(t, err)
	owner, err := store.People().ClaimOwner(t.Context(), bound.Token, "browser", people.CreatePerson{Username: "owner", Name: "Owner", Password: "a long reset spend password"}) //nolint:gosec // test fixture, not a credential
	require.NoError(t, err)
	createTeam(t, store, "eng", quota.QuotaConfig{})
	createKey(t, store, "member", owner.TeamID, quota.QuotaConfig{})
	createKey(t, store, "engineer", "eng", quota.QuotaConfig{})
	writeSpend(t, path, map[quota.SpendScope]int64{
		{Kind: quota.ScopeKey, EntityID: "member"}: 1, {Kind: quota.ScopeKey, EntityID: "engineer"}: 2,
		{Kind: quota.ScopeTeam, EntityID: "eng"}: 3, {Kind: quota.ScopeTeam, EntityID: owner.TeamID}: 4,
	})
	core := startCore(t, store, path)

	_, err = store.Reset(t.Context(), "backup-1", func(sqlstore.Meta) error { return nil })
	require.NoError(t, err)
	kept := map[string]int64{"team:" + owner.TeamID + ":": 4}
	assert.Equal(t, kept, spentBy(core))
	rows := map[string]int64{}
	for scope, st := range spendRows(t, path) {
		rows[scope] = st.SpendMicro
	}
	assert.Equal(t, kept, rows, "the purge is written at once")
}
