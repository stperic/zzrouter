package clusterid

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/common/atomicfile"
	"github.com/stretchr/testify/require"
)

func TestInstallSignedCertPublishesCommittedReplacement(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "pre-rename"
		if committed {
			name = "post-rename"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			id, err := LoadOrCreateIdentity(dir)
			require.NoError(t, err)
			old := id.CertPEM()
			ca, err := LoadOrCreateCA(t.TempDir())
			require.NoError(t, err)
			csr, err := id.CSR("test-worker", nil, nil)
			require.NoError(t, err)
			fresh, err := ca.Sign(csr, RoleWorker)
			require.NoError(t, err)
			write := func(path string, data []byte) error {
				if !committed {
					return io.ErrClosedPipe
				}
				if err := writeIdentityFile(path, data); err != nil {
					return err
				}
				return errors.Join(atomicfile.ErrPostRenameDurability, io.ErrClosedPipe)
			}
			err = id.installSignedCert(fresh, write)
			require.ErrorIs(t, err, io.ErrClosedPipe)
			expected := old
			if committed {
				require.ErrorIs(t, err, atomicfile.ErrPostRenameDurability)
				expected = fresh
			} else {
				require.NotErrorIs(t, err, atomicfile.ErrPostRenameDurability)
			}
			disk, err := os.ReadFile(filepath.Join(dir, identityCertFile))
			require.NoError(t, err)
			require.Equal(t, expected, disk)
			require.Equal(t, expected, id.CertPEM(), "live TLS identity must match the committed file")
			parsed, err := parseSingleCert(expected)
			require.NoError(t, err)
			require.Equal(t, parsed.Raw, id.Certificate().Raw)
			restarted, err := LoadOrCreateIdentity(dir)
			require.NoError(t, err)
			require.Equal(t, restarted.CertPEM(), id.CertPEM())
		})
	}
}
