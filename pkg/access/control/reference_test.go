package control

import (
	"context"
	"testing"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedState publishes the one snapshot a test sets.
type fixedState struct{ snap *access.Snapshot }

func (s *fixedState) Snapshot() *access.Snapshot   { return s.snap }
func (s *fixedState) OnCommit(func(access.Commit)) {}

// TestCredentialReferenceGeneration: an app-key reference resolves only
// against the content it was issued from, so replacing the generation
// invalidates it even though the key record is unchanged.
func TestCredentialReferenceGeneration(t *testing.T) {
	raw, err := keys.GenerateRawKey()
	require.NoError(t, err)
	hash, err := keys.HashKey(raw)
	require.NoError(t, err)
	under := func(generation string) *access.Snapshot {
		key := &keys.VirtualKey{ID: "reader", UUID: "reader-uuid", Name: "Reader", Role: "user", HashedKey: hash, TeamID: "team", TeamRole: "member"}
		team := &teams.Team{ID: "team", Name: "Reference team", Kind: teams.KindShared}
		return access.NewSnapshot(generation, map[string]*keys.VirtualKey{"reader": key}, map[string]*teams.Team{"team": team}, nil)
	}
	state := &fixedState{snap: under("before")}
	core := NewCore(StaticKeySet{}, state, nil, nil, nil)
	issued, err := core.Reference(raw)
	require.NoError(t, err)
	_, err = core.AuthenticateReference(issued)
	require.NoError(t, err)

	state.snap = under("after")
	_, err = core.AuthenticateReference(issued)
	require.ErrorIs(t, err, ErrInvalidKey, "a reference from another generation does not resolve")
	current, err := core.Reference(raw)
	require.NoError(t, err, "the same key signs in again under the new generation")
	assert.NotEqual(t, issued.Version, current.Version)
	_, err = core.AuthenticateReference(current)
	require.NoError(t, err)
}

func TestCredentialReferenceLiveAuthority(t *testing.T) {
	st := referenceStore(t)
	_, raw, err := st.CreateKey(t.Context(), "reader", keys.VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	core := NewCore(StaticKeySet{Admin: "test-admin-reference-long-enough-secret"}, st, nil, nil, nil)
	ref, err := core.Reference(raw)
	require.NoError(t, err)
	ac, err := core.AuthenticateReference(ref)
	require.NoError(t, err)
	require.Equal(t, "reader", ac.Key.ID)
	require.True(t, ac.Key.IsVirtual)
	update := func(apply func(*keys.VirtualKey)) {
		_, err := st.UpdateKey(t.Context(), "reader", func(k *keys.VirtualKey) error { apply(k); return nil }, audit.Event{})
		require.NoError(t, err)
	}
	update(func(k *keys.VirtualKey) { k.RPMLimit = 3 })
	ac, err = core.AuthenticateReference(ref)
	require.NoError(t, err)
	require.Equal(t, 3, ac.Key.Quotas.RPMLimit)
	update(func(k *keys.VirtualKey) { k.Suspended = true })
	_, err = core.AuthenticateReference(ref)
	require.ErrorIs(t, err, ErrKeySuspended)
	update(func(k *keys.VirtualKey) { k.Suspended = false })
	_, _, err = st.RotateKey(t.Context(), "reader", audit.Event{})
	require.NoError(t, err)
	_, err = core.AuthenticateReference(ref)
	require.ErrorIs(t, err, ErrInvalidKey)
	_, err = core.Reference(raw)
	require.Error(t, err)
	admin, err := core.Reference("test-admin-reference-long-enough-secret")
	require.NoError(t, err)
	other := NewCore(StaticKeySet{Admin: "another-test-admin-reference-long-secret"}, st, nil, nil, nil)
	_, err = other.AuthenticateReference(admin)
	require.ErrorIs(t, err, ErrInvalidKey)
}

// rotatingReferenceStore rotates the key right after the snapshot that authenticated it.
type rotatingReferenceStore struct {
	access.Store
	rotate bool
}

func (s *rotatingReferenceStore) Snapshot() *access.Snapshot {
	snap := s.Store.Snapshot()
	if s.rotate {
		s.rotate = false
		_, _, _ = s.RotateKey(context.Background(), "reader", audit.Event{})
	}
	return snap
}

func TestCredentialReferenceRotationDuringLogin(t *testing.T) {
	st := referenceStore(t)
	_, raw, err := st.CreateKey(t.Context(), "reader", keys.VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	rotating := &rotatingReferenceStore{Store: st, rotate: true}
	core := NewCore(StaticKeySet{}, rotating, nil, nil, nil)
	_, err = core.Reference(raw)
	require.ErrorIs(t, err, ErrInvalidKey)
}

func TestCredentialReferenceConcurrentRotation(t *testing.T) {
	store := referenceStore(t)
	_, raw, err := store.CreateKey(t.Context(), "reader", keys.VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	core := NewCore(StaticKeySet{}, store, nil, nil, nil)
	ref, err := core.Reference(raw)
	require.NoError(t, err)
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		for range 3 {
			if _, _, err := store.RotateKey(context.Background(), "reader", audit.Event{}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	close(start)
	for {
		_, _ = core.AuthenticateReference(ref)
		select {
		case err := <-done:
			require.NoError(t, err)
			_, err = core.AuthenticateReference(ref)
			require.ErrorIs(t, err, ErrInvalidKey)
			return
		default:
		}
	}
}

func referenceStore(t *testing.T) access.Store {
	t.Helper()
	store := openTestStore(t, sqlstore.Options{})
	createReferenceTeam(t, store)
	return store
}

func createReferenceTeam(t *testing.T, store access.Store) {
	t.Helper()
	_, err := store.CreateTeam(t.Context(), "team", teams.Team{Name: "Reference team", Kind: "shared"}, audit.Event{})
	require.NoError(t, err)
}
