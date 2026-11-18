package filestore

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stretchr/testify/require"
)

func TestTeamSaveAcknowledgesCommittedPolicy(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "before-rename"
		if committed {
			name = "after-rename"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "teams.yaml")
			store := NewTeams(path)
			require.NoError(t, store.Create("engineering", &teams.Team{Name: "Engineering", Kind: teams.KindShared}))
			require.NoError(t, store.Save())
			require.NoError(t, store.Update("engineering", &teams.Team{Name: "Engineering", Suspended: true}))
			err := store.save(func(path string, data []byte, mode os.FileMode) error {
				if !committed {
					return io.ErrClosedPipe
				}
				if err := atomicfile.WriteFile(path, data, mode); err != nil {
					return err
				}
				return errors.Join(atomicfile.ErrPostRenameDurability, io.ErrClosedPipe)
			})
			if committed {
				require.NoError(t, err, "committed policy must continue the caller's publication path")
			} else {
				require.ErrorIs(t, err, io.ErrClosedPipe)
			}
			restarted := NewTeams(path)
			require.NoError(t, restarted.Load())
			require.Equal(t, committed, restarted.Get("engineering").Suspended)
		})
	}
}
