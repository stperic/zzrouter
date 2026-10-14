package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChecksumMismatchCanBeDistinguishedFromReadFailures(t *testing.T) {
	file := filepath.Join(t.TempDir(), "archive")
	err := VerifySHA256(file, strings.Repeat("a", 64))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NotErrorIs(t, err, ErrChecksumMismatch)
	require.NoError(t, os.WriteFile(file, []byte("bad cached bytes"), 0600))
	require.ErrorIs(t, VerifySHA256(file, strings.Repeat("a", 64)), ErrChecksumMismatch)
}
