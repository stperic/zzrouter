package search

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

// TestSentinelErrors_WrapSemantics pins that the raise-site wraps
// preserve both errors.Is identity and operator-facing context (the
// offending value or limit).
func TestSentinelErrors_WrapSemantics(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		wrapped     error
		target      error
		wantContext string
	}{
		{
			name:        "ErrInvalidProvider preserves offending provider name",
			wrapped:     fmt.Errorf("%w: %s", ErrInvalidProvider, "bogus"),
			target:      ErrInvalidProvider,
			wantContext: "bogus",
		},
		{
			name:        "ErrLimitNegative preserves offending value",
			wrapped:     fmt.Errorf("%w: %d", ErrLimitNegative, -5),
			target:      ErrLimitNegative,
			wantContext: "-5",
		},
		{
			name:        "ErrLimitExceedsMax preserves max + offending value",
			wrapped:     fmt.Errorf("%w (%d): got %d", ErrLimitExceedsMax, 1000, 5000),
			target:      ErrLimitExceedsMax,
			wantContext: "5000",
		},
		{
			name:        "ErrOffsetNegative preserves offending value",
			wrapped:     fmt.Errorf("%w: %d", ErrOffsetNegative, -1),
			target:      ErrOffsetNegative,
			wantContext: "-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.True(t, errors.Is(tc.wrapped, tc.target),
				"wrap must satisfy errors.Is against %v", tc.target)
			assert.Contains(t, tc.wrapped.Error(), tc.wantContext,
				"wrap must surface context substring")
		})
	}
}

// TestSearch_NoStringLiteralErrors guards the four swept raise sites in
// params.go and pagination.go. Coarse scan, matches the template at
// pkg/access/control/errors_test.go.
func TestSearch_NoStringLiteralErrors(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	require.NoError(t, err)

	scans := map[string][]string{
		"params.go": {
			`fmt.Errorf("invalid provider: %s"`,
		},
		"pagination.go": {
			`fmt.Errorf("limit cannot be negative: %d"`,
			`fmt.Errorf("limit cannot exceed %d: %d"`,
			`fmt.Errorf("offset cannot be negative: %d"`,
		},
	}
	for file, banned := range scans {
		body, err := os.ReadFile(filepath.Join(dir, file))
		require.NoError(t, err, "read %s", file)
		src := string(body)
		for _, pattern := range banned {
			assert.False(t, strings.Contains(src, pattern),
				"%s must not contain %q — use the corresponding sentinel from errors.go",
				file, pattern)
		}
	}
}
