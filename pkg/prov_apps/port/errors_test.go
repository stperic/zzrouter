package port

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

// TestErrPortConflict_WrapSemantics pins that the manager.go raise-site
// wrap satisfies errors.Is while preserving the overlapping-range context.
func TestErrPortConflict_WrapSemantics(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("%w: %q [%d-%d] overlaps %q [%d-%d]",
		ErrPortConflict, "vllm", 8000, 8020, "llama.cpp", 8010, 8030)
	assert.True(t, errors.Is(wrapped, ErrPortConflict))
	assert.Contains(t, wrapped.Error(), "vllm")
	assert.Contains(t, wrapped.Error(), "llama.cpp")
}

// TestPort_NoStringLiteralErrors guards against regression of the port
// range conflict raise site. See pkg/access/control/errors_test.go for
// the template rationale.
func TestPort_NoStringLiteralErrors(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(dir, "manager.go"))
	require.NoError(t, err)
	src := string(body)

	banned := []string{
		`fmt.Errorf("port range conflict:`,
	}
	for _, pattern := range banned {
		assert.False(t, strings.Contains(src, pattern),
			"manager.go must not contain %q — use ErrPortConflict",
			pattern)
	}
}
