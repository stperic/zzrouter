package control

import (
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stretchr/testify/require"
)

func TestCredentialReferenceLiveAuthority(t *testing.T) {
	st := keys.NewFileKeyStore(filepath.Join(t.TempDir(), "keys.yaml"))
	ts := teams.NewFileTeamStore(filepath.Join(t.TempDir(), "teams.yaml"))
	raw, err := st.Create("reader", &keys.VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"})
	require.NoError(t, err)
	core := NewCore(StaticKeySet{Admin: "test-admin-reference-long-enough-secret"}, st, ts, nil, nil, nil)
	ref, err := core.Reference(raw)
	require.NoError(t, err)
	ac, err := core.AuthenticateReference(ref)
	require.NoError(t, err)
	require.Equal(t, "reader", ac.Key.ID)
	require.True(t, ac.Key.IsVirtual)
	k := st.Get("reader")
	k.RPMLimit = 3
	require.NoError(t, st.Update("reader", k))
	ac, err = core.AuthenticateReference(ref)
	require.NoError(t, err)
	require.Equal(t, 3, ac.Key.Quotas.RPMLimit)
	k = st.Get("reader")
	k.Suspended = true
	require.NoError(t, st.Update("reader", k))
	_, err = core.AuthenticateReference(ref)
	require.ErrorIs(t, err, ErrKeySuspended)
	k = st.Get("reader")
	k.Suspended = false
	require.NoError(t, st.Update("reader", k))
	_, err = st.RotateKey("reader")
	require.NoError(t, err)
	_, err = core.AuthenticateReference(ref)
	require.ErrorIs(t, err, ErrInvalidKey)
	_, err = core.Reference(raw)
	require.Error(t, err)
	admin, err := core.Reference("test-admin-reference-long-enough-secret")
	require.NoError(t, err)
	other := NewCore(StaticKeySet{Admin: "another-test-admin-reference-long-secret"}, st, ts, nil, nil, nil)
	_, err = other.AuthenticateReference(admin)
	require.ErrorIs(t, err, ErrInvalidKey)
}

type rotatingReferenceStore struct {
	keys.KeyStore
	rotate bool
}

func (s *rotatingReferenceStore) ValidateRawKey(raw string) (string, *keys.VirtualKey) {
	id, k := s.KeyStore.ValidateRawKey(raw)
	if s.rotate && k != nil {
		s.rotate = false
		_, _ = s.KeyStore.RotateKey(id)
	}
	return id, k
}
func TestCredentialReferenceRotationDuringLogin(t *testing.T) {
	st := keys.NewFileKeyStore(filepath.Join(t.TempDir(), "keys.yaml"))
	raw, err := st.Create("reader", &keys.VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"})
	require.NoError(t, err)
	rotating := &rotatingReferenceStore{KeyStore: st, rotate: true}
	core := NewCore(StaticKeySet{}, rotating, nil, nil, nil, nil)
	_, err = core.Reference(raw)
	require.ErrorIs(t, err, ErrInvalidKey)
}

func TestCredentialReferenceConcurrentRotation(t *testing.T) {
	store := keys.NewFileKeyStore(filepath.Join(t.TempDir(), "keys.yaml"))
	raw, err := store.Create("reader", &keys.VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"})
	require.NoError(t, err)
	core := NewCore(StaticKeySet{}, store, nil, nil, nil, nil)
	ref, err := core.Reference(raw)
	require.NoError(t, err)
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		for range 3 {
			if _, err := store.RotateKey("reader"); err != nil {
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
