package control

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

func TestAuthCacheCountsOnlyUntilAKeyCommit(t *testing.T) {
	t.Parallel()
	c, store := newTestCore(t, StaticKeySet{})
	_, err := store.CreateTeam(t.Context(), "team", teams.Team{Name: "Team", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	_, raw, err := store.CreateKey(t.Context(), "reader", keys.VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	_, err = c.Authenticate(raw)
	require.NoError(t, err)
	fp := keys.SHA256Fingerprint(raw)
	cached := func() *KeyPrincipal { return c.authenticator.getCached(fp, store.Snapshot().KeysVersion()) }
	require.NotNil(t, cached())

	_, err = store.UpdateTeam(t.Context(), "team", func(team *teams.Team) error { team.RPMLimit = 5; return nil }, audit.Event{})
	require.NoError(t, err)
	assert.NotNil(t, cached(), "a team commit keeps warmed entries; each dropped one costs an Argon2 verify")

	_, err = store.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error { k.RPMLimit = 1; return nil }, audit.Event{})
	require.NoError(t, err)
	assert.Nil(t, cached(), "a key commit retires warmed entries")
}

// rotatingState hands Validate the snapshot it read, then commits a rotation
// before Validate caches what it found there.
type rotatingState struct {
	access.Store
	rotate bool
}

func (s *rotatingState) Snapshot() *access.Snapshot {
	snap := s.Store.Snapshot()
	if s.rotate {
		s.rotate = false
		_, _, _ = s.RotateKey(context.Background(), "reader", audit.Event{})
	}
	return snap
}

// A rotation that commits between the snapshot Validate reads and the entry it
// caches must not leave the old key authenticating for the cache's lifetime.
func TestAuthCacheNeverServesAKeyRotatedDuringValidation(t *testing.T) {
	t.Parallel()
	store := referenceStore(t)
	_, raw, err := store.CreateKey(t.Context(), "reader", keys.VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	state := &rotatingState{Store: store, rotate: true}
	c := newTestCoreFor(t, StaticKeySet{}, state)
	_, err = c.Authenticate(raw)
	require.NoError(t, err, "validated against the snapshot read before the rotation")
	require.False(t, state.rotate, "the rotation committed inside Validate")
	_, err = c.Authenticate(raw)
	require.ErrorIs(t, err, ErrInvalidKey, "the entry cached against the old snapshot does not count")
}
