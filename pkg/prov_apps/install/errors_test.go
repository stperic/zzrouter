package install

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

// TestSentinelErrors_WrapSemantics pins that the raise-site wraps satisfy
// errors.Is against each sentinel while preserving operator-facing context
// (platform, filename, etc.) in the rendered message. Downstream callers
// and logs depend on both properties holding together.
func TestSentinelErrors_WrapSemantics(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		wrapped     error
		target      error
		wantContext string
	}{
		{
			name:        "ErrUnsupportedPlatform preserves provider+platform",
			wrapped:     fmt.Errorf("%w: mlx is not supported on linux/amd64", ErrUnsupportedPlatform),
			target:      ErrUnsupportedPlatform,
			wantContext: "linux/amd64",
		},
		{
			name:        "ErrNoWritePermission preserves path + inner err",
			wrapped:     fmt.Errorf("%w on /readonly: %w", ErrNoWritePermission, os.ErrPermission),
			target:      ErrNoWritePermission,
			wantContext: "/readonly",
		},
		{
			name:        "ErrNoMatchingVariant preserves platform",
			wrapped:     fmt.Errorf("%w: freebsd/amd64", ErrNoMatchingVariant),
			target:      ErrNoMatchingVariant,
			wantContext: "freebsd/amd64",
		},
		{
			name:        "ErrNoGPUMatch preserves host context",
			wrapped:     fmt.Errorf("%w: platform windows/amd64, host no GPU detected", ErrNoGPUMatch),
			target:      ErrNoGPUMatch,
			wantContext: "no GPU detected",
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

	// ErrNoWritePermission wraps os.ErrPermission as the second %w, so
	// both errors.Is targets must succeed simultaneously.
	writePerm := fmt.Errorf("%w on /x: %w", ErrNoWritePermission, os.ErrPermission)
	assert.True(t, errors.Is(writePerm, ErrNoWritePermission))
	assert.True(t, errors.Is(writePerm, os.ErrPermission))
}

// TestInstall_NoStringLiteralErrors guards against regression — every
// raise site flagged during the sentinel sweep must route through its
// sentinel rather than a rendered string literal. If someone reintroduces
// the pre-sweep fmt.Errorf patterns, these fail loudly.
//
// Scan is deliberately coarse (matches pkg/access/control/errors_test.go):
// file-level string search, zero build-time cost, catches every realistic
// regression. AST-walking would be stricter but overengineered.
func TestInstall_NoStringLiteralErrors(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	require.NoError(t, err)

	// Map of file → banned patterns in that file.
	scans := map[string][]string{
		"builtins/pythonvenv/pythonvenv.go": {
			`fmt.Errorf("%s is not supported on %s/%s"`,
		},
		"preflight/preflight_unix.go": {
			`fmt.Errorf("no write permission on %s: %w"`,
		},
		"preflight/preflight_windows.go": {
			`fmt.Errorf("no write permission on %s: %w"`,
		},
		"variant/variant.go": {
			`fmt.Errorf("no install_variants declared")`,
			`fmt.Errorf("no install_variants matches target platform`,
			`fmt.Errorf("platform %s/%s has only GPU-gated`,
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
