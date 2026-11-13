package keys

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/common/atomicfile"
	"github.com/stretchr/testify/require"
)

func TestKeyRotationAcknowledgesCommittedReplacement(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "before-rename"
		if committed {
			name = "after-rename"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys.yaml")
			store := NewFileKeyStore(path)
			oldKey, err := store.Create("reader", &VirtualKey{Name: "Reader", Role: "user", TeamID: "team", TeamRole: "member"})
			require.NoError(t, err)
			require.NoError(t, store.Save())
			newKey, err := store.RotateKey("reader")
			require.NoError(t, err)
			err = store.save(func(path string, data []byte, mode os.FileMode) error {
				if !committed {
					return io.ErrClosedPipe
				}
				if err := atomicfile.WriteFile(path, data, mode); err != nil {
					return err
				}
				return errors.Join(atomicfile.ErrPostRenameDurability, io.ErrClosedPipe)
			})
			if committed {
				require.NoError(t, err, "a saved rotation must deliver its one-time key instead of asking for a retry")
			} else {
				require.ErrorIs(t, err, io.ErrClosedPipe)
			}
			restarted := NewFileKeyStore(path)
			require.NoError(t, restarted.Load())
			want := oldKey
			if committed {
				want = newKey
				require.Equal(t, store.Get("reader").HashedKey, restarted.Get("reader").HashedKey)
				require.False(t, VerifyKey(oldKey, restarted.Get("reader").HashedKey))
			}
			require.True(t, VerifyKey(want, restarted.Get("reader").HashedKey))
		})
	}
}
