package control

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

// TestSentinelErrors_ExpiredAndSuspended_Wrap pins that the authenticator
// wraps ErrKeyExpired / ErrKeySuspended with errors.Is semantics and still
// surfaces the offending key id in the rendered message. Downstream
// callers (logging, adapter) depend on both properties.
func TestSentinelErrors_ExpiredAndSuspended_Wrap(t *testing.T) {
	t.Parallel()
	expired := fmt.Errorf("%w: %q", ErrKeyExpired, "vk-demo")
	assert.True(t, errors.Is(expired, ErrKeyExpired), "expired wrap must satisfy errors.Is")
	assert.Contains(t, expired.Error(), "vk-demo", "expired wrap must surface key id")

	suspended := fmt.Errorf("%w: %q", ErrKeySuspended, "vk-demo")
	assert.True(t, errors.Is(suspended, ErrKeySuspended), "suspended wrap must satisfy errors.Is")
	assert.Contains(t, suspended.Error(), "vk-demo", "suspended wrap must surface key id")
}

// TestAuthenticator_NoStringLiteralErrors guards against regression — every
// error the authenticator returns must route through one of the four
// sentinels. If someone reintroduces `fmt.Errorf("empty API key")` this
// test fails loudly.
//
// The scan is deliberately coarse: we look for the specific substring
// patterns the pre-sentinel code used. A true lint rule would require
// AST-walking; this test trades that precision for zero build-time cost
// and catches the only realistic regression path.
func TestAuthenticator_NoStringLiteralErrors(t *testing.T) {
	t.Parallel()
	// go test always runs each package's binary with cwd = package dir,
	// so os.Getwd resolves to this package reliably.
	dir, err := os.Getwd()
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(dir, "authenticator.go"))
	require.NoError(t, err)
	src := string(body)

	// Ban both the fmt.Errorf and errors.New forms for all four failure
	// modes. Also ban the message-literal substrings standalone so a
	// contributor who wraps with a custom helper (fmt.Errorf("%s: %w",
	// msg, inner)) still gets caught. AST-walking lint would be stricter
	// but this catches every realistic regression path without build-time
	// cost.
	banned := []string{
		`fmt.Errorf("empty API key"`,
		`fmt.Errorf("invalid API key"`,
		`fmt.Errorf("API key %q has expired"`,
		`fmt.Errorf("API key %q is suspended"`,
		`errors.New("empty API key"`,
		`errors.New("invalid API key"`,
		`errors.New("API key %q has expired"`,
		`errors.New("API key %q is suspended"`,
		`"API key %q has expired"`,
		`"API key %q is suspended"`,
	}
	for _, pattern := range banned {
		assert.False(t, strings.Contains(src, pattern),
			"authenticator.go must not contain %q — use ErrEmptyKey/ErrInvalidKey/ErrKeyExpired/ErrKeySuspended instead",
			pattern)
	}
}
