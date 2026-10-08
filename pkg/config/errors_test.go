package config

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

// TestSentinelErrors_WrapSemantics pins that the raise-site wraps in
// update_config.go, node_config.go, apps_config_load_new.go, and
// security.go satisfy errors.Is while preserving the operator-facing
// context — field name, offending value, or env-var name. Both
// properties are load-bearing for downstream consumers.
func TestSentinelErrors_WrapSemantics(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		wrapped     error
		target      error
		wantContext string
	}{
		{
			name:        "ErrInvalidConfig preserves field name (channel)",
			wrapped:     fmt.Errorf("%w: invalid update channel: %q", ErrInvalidConfig, "bogus"),
			target:      ErrInvalidConfig,
			wantContext: "update channel",
		},
		{
			name:        "ErrInvalidConfig preserves field name (port)",
			wrapped:     fmt.Errorf("%w: node.port must be between 1 and 65535, got %d", ErrInvalidConfig, 70000),
			target:      ErrInvalidConfig,
			wantContext: "node.port",
		},
		{
			name: "ErrInvalidConfig dual-%w preserves inner validator chain",
			wrapped: fmt.Errorf("%w: invalid maintenance_window: %w",
				ErrInvalidConfig,
				errors.New("maintenance window must have 5 fields (cron format), got 3")),
			target:      ErrInvalidConfig,
			wantContext: "maintenance_window",
		},
		{
			name:        "ErrMissingEnvKey preserves env var name",
			wrapped:     fmt.Errorf("%w: %s", ErrMissingEnvKey, "NONEXISTENT_ADMIN_KEY"),
			target:      ErrMissingEnvKey,
			wantContext: "NONEXISTENT_ADMIN_KEY",
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

	// ErrInvalidConfig + inner validator err: both targets must satisfy
	// errors.Is simultaneously (dual-%w). This is what
	// apps_config_load_new.go and update_config.go's maintenance_window
	// path rely on.
	inner := errors.New("minute field contains invalid value: \"bad\"")
	compound := fmt.Errorf("%w: invalid maintenance_window: %w", ErrInvalidConfig, inner)
	assert.True(t, errors.Is(compound, ErrInvalidConfig))
	assert.True(t, errors.Is(compound, inner))
}

// TestConfig_NoStringLiteralErrors guards the swept raise sites across
// the four files. Coarse-scan template, matches pkg/access/control/
// errors_test.go — reads each file once and fails on the pre-sweep
// fmt.Errorf vectors.
func TestConfig_NoStringLiteralErrors(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	require.NoError(t, err)

	scans := map[string][]string{
		"update_config.go": {
			`fmt.Errorf("invalid update channel: %q`,
			`fmt.Errorf("check_interval_hours cannot be negative: %d"`,
			`fmt.Errorf("invalid maintenance_window: %w"`,
			`fmt.Errorf("invalid pinned_version: %w"`,
			`fmt.Errorf("keep_previous_versions cannot be negative: %d"`,
		},
		"node_config.go": {
			`fmt.Errorf("node.port must be between 1 and 65535, got %d"`,
			`fmt.Errorf("auth.admin_key must be at least %d characters`,
			`fmt.Errorf("auth.user_key must be at least %d characters`,
			`fmt.Errorf("cluster.endpoints required in worker mode`,
			`fmt.Errorf("node.tls_cert and node.tls_key must both be set`,
		},
		"apps_config_load_new.go": {
			`fmt.Errorf("provider %q is declared in multiple kinds"`,
		},
		"security.go": {
			`fmt.Errorf("environment variable %s not set"`,
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
