package keys

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFileKeyStoreSnapshotsDoNotShareMutableState(t *testing.T) {
	store := NewFileKeyStore(filepath.Join(t.TempDir(), "keys.yaml"))
	expires := time.Now().Add(time.Hour)
	input := &VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member", ExpiresAt: &expires, Metadata: map[string]string{"label": "original"}}
	raw, err := store.Create("reader", input)
	require.NoError(t, err)
	input.Name = "changed outside store"
	input.Metadata["label"] = "changed outside store"
	_, validated := store.ValidateRawKey(raw)
	snapshots := []*VirtualKey{store.Get("reader"), store.List()["reader"], store.ListByTeam("team")["reader"], validated}
	for _, snapshot := range snapshots {
		require.Equal(t, "Reader", snapshot.Name)
		require.Equal(t, "original", snapshot.Metadata["label"])
		snapshot.Name = "outside edit"
		snapshot.Metadata["label"] = "outside edit"
		*snapshot.ExpiresAt = time.Time{}
	}
	require.Equal(t, "Reader", store.Get("reader").Name)
	require.Equal(t, "original", store.Get("reader").Metadata["label"])
	require.Equal(t, expires, *store.Get("reader").ExpiresAt)

	before := store.Get("reader")
	_, err = store.RotateKey("reader")
	require.NoError(t, err)
	require.NotEqual(t, before.HashedKey, store.Get("reader").HashedKey)
	require.True(t, VerifyKey(raw, before.HashedKey), "previous snapshot must remain immutable after rotation")
	before.Name = "explicit update"
	require.NoError(t, store.Update("reader", before))
	before.Name = "changed after update"
	before.Metadata["label"] = "changed after update"
	require.Equal(t, "explicit update", store.Get("reader").Name)
	require.Equal(t, "original", store.Get("reader").Metadata["label"])
}
