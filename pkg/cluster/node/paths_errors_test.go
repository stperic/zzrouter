package clusternode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPaths_NoStringLiteralErrors guards against regression — the one
// discriminable failure mode in paths.go (empty coordinator_url file)
// must route through errCoordinatorURLEmpty, not a rendered string.
// If someone reintroduces fmt.Errorf("coordinator_url is empty"), this
// test fails loudly.
//
// The scan is deliberately coarse, matching pkg/access/control/errors_test.go.
// Scoped here to fmt.Errorf only: the sentinel declaration (`errors.New(
// "coordinator_url is empty")`) lives at the top of this same file, so a
// broader ban would flag the declaration itself. A reintroduction of
// errors.New inside the function body would create a fresh pointer per
// call and break errors.Is — less common than the fmt.Errorf vector, and
// caught by the existing errors.Is assertion in paths_test.go failing.
func TestPaths_NoStringLiteralErrors(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(dir, "paths.go"))
	require.NoError(t, err)
	src := string(body)

	banned := []string{
		`fmt.Errorf("coordinator_url is empty"`,
	}
	for _, pattern := range banned {
		assert.False(t, strings.Contains(src, pattern),
			"paths.go must not contain %q — use errCoordinatorURLEmpty",
			pattern)
	}
}
