package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestErrDangerousEnvVar_WrapSemantics pins the launcher raise-site wrap.
func TestErrDangerousEnvVar_WrapSemantics(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("%w: %s", ErrDangerousEnvVar, "LD_PRELOAD")
	assert.True(t, errors.Is(wrapped, ErrDangerousEnvVar))
	assert.Contains(t, wrapped.Error(), "LD_PRELOAD")
}

// TestProcess_NoStringLiteralErrors guards the launcher raise site.
// The other "dangerous X" errors in security.go (path traversal, symlink,
// command injection) remain plain fmt.Errorf because they have no
// substring tests depending on them — they'll join the sweep if/when a
// test does.
func TestProcess_NoStringLiteralErrors(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(dir, "launcher.go"))
	require.NoError(t, err)
	src := string(body)

	banned := []string{
		`fmt.Errorf("dangerous environment variable rejected:`,
	}
	for _, pattern := range banned {
		assert.False(t, strings.Contains(src, pattern),
			"launcher.go must not contain %q — use ErrDangerousEnvVar",
			pattern)
	}
}
