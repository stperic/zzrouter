package control

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

func TestAuthCacheDropsEntriesOnlyOnKeyCommits(t *testing.T) {
	t.Parallel()
	c, store := newTestCore(t, StaticKeySet{})
	_, err := store.CreateTeam(t.Context(), "team", teams.Team{Name: "Team", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	_, raw, err := store.CreateKey(t.Context(), "reader", keys.VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	_, err = c.Authenticate(raw)
	require.NoError(t, err)
	fp := keys.SHA256Fingerprint(raw)
	require.NotNil(t, c.authenticator.getCached(fp))

	_, err = store.UpdateTeam(t.Context(), "team", func(team *teams.Team) error { team.RPMLimit = 5; return nil }, audit.Event{})
	require.NoError(t, err)
	assert.NotNil(t, c.authenticator.getCached(fp), "a team commit keeps warmed entries; each dropped one costs an Argon2 verify")

	_, err = store.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error { k.RPMLimit = 1; return nil }, audit.Event{})
	require.NoError(t, err)
	assert.Nil(t, c.authenticator.getCached(fp), "a key commit drops warmed entries")
}
