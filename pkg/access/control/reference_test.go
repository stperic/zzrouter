package control

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/filestore"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialReferenceShortEnvironmentKey(t *testing.T) {
	const envName = "ZZROUTER_TEST_SHORT_REFERENCE_KEY"
	const raw = "tiny-secret"
	t.Setenv(envName, raw)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keys.yaml"), []byte(`keys:
  reader:
    uuid: short-env-reference
    name: Reader
    role: user
    key_env: ZZROUTER_TEST_SHORT_REFERENCE_KEY
    team_id: team
    team_role: member
`), 0o600))
	store, err := filestore.Open(t.Context(), dir, filestore.Options{})
	require.NoError(t, err)
	createReferenceTeam(t, store)
	core := NewCore(StaticKeySet{}, store, nil, nil, nil)
	ref, err := core.Reference(raw)
	require.NoError(t, err)
	data, err := json.Marshal(ref)
	require.NoError(t, err)
	assert.NotContains(t, string(data), raw, "persisted bindings must not contain a reusable environment credential")
	var saved CredentialReference
	require.NoError(t, json.Unmarshal(data, &saved))
	ac, err := core.AuthenticateReference(saved)
	require.NoError(t, err)
	assert.Equal(t, "reader", ac.Key.ID)
	assert.True(t, ac.Key.IsVirtual)
	assert.Equal(t, "[redacted]", ac.Key.Fingerprint)

	const rotated = "next-key"
	t.Setenv(envName, rotated)
	_, err = core.AuthenticateReference(saved)
	require.ErrorIs(t, err, ErrInvalidKey)
	_, err = core.Reference(raw)
	require.ErrorIs(t, err, ErrInvalidKey)
	next, err := core.Reference(rotated)
	require.NoError(t, err)
	assert.NotEqual(t, saved.Version, next.Version)
	data, err = json.Marshal(next)
	require.NoError(t, err)
	assert.NotContains(t, string(data), rotated)
	_, err = core.AuthenticateReference(next)
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
	store := openTestStore(t, filestore.Options{})
	createReferenceTeam(t, store)
	return store
}

func createReferenceTeam(t *testing.T, store access.Store) {
	t.Helper()
	_, err := store.CreateTeam(t.Context(), "team", teams.Team{Name: "Reference team", Kind: "shared"}, audit.Event{})
	require.NoError(t, err)
}
