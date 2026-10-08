package modelregistry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIdentifierParser_NoStringLiteralErrors guards the one raise site
// swept on identifier_parser.go. If someone reintroduces the pre-sweep
// fmt.Errorf literal, this test fails loudly.
//
// Coarse scan, matches pkg/access/control/errors_test.go template.
func TestIdentifierParser_NoStringLiteralErrors(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(dir, "identifier_parser.go"))
	require.NoError(t, err)
	src := string(body)

	banned := []string{
		`fmt.Errorf("invalid format: cannot contain both`,
	}
	for _, pattern := range banned {
		assert.False(t, strings.Contains(src, pattern),
			"identifier_parser.go must not contain %q — use ErrIdentifierMixedSeparators",
			pattern)
	}
}
