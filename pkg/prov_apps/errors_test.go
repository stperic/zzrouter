package prov_apps

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

// TestParentSentinels_WrapSemantics pins that manager_lifecycle.go's
// two wrapped raise sites satisfy errors.Is and preserve operator
// context (provider name, inner validator error).
func TestParentSentinels_WrapSemantics(t *testing.T) {
	t.Parallel()

	notFound := fmt.Errorf("%w: %q not found in config", ErrProviderNotFound, "nonexistent")
	assert.True(t, errors.Is(notFound, ErrProviderNotFound))
	assert.Contains(t, notFound.Error(), "nonexistent")

	// ErrParameterValidation uses dual %w to preserve the inner
	// validator error chain; both targets must satisfy errors.Is.
	innerErr := errors.New("rejected: key;inject contains shell metacharacter")
	paramFail := fmt.Errorf("%w: %w", ErrParameterValidation, innerErr)
	assert.True(t, errors.Is(paramFail, ErrParameterValidation))
	assert.True(t, errors.Is(paramFail, innerErr))
	assert.Contains(t, paramFail.Error(), "key;inject")
}

// TestParent_NoStringLiteralErrors guards the two manager_lifecycle.go
// raise sites swept in commit 2 of this arc. Same coarse-scan template
// as pkg/access/control/errors_test.go.
func TestParent_NoStringLiteralErrors(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(dir, "manager_lifecycle.go"))
	require.NoError(t, err)
	src := string(body)

	banned := []string{
		`fmt.Errorf("provider %q not found in config"`,
		`fmt.Errorf("parameter validation failed: %w"`,
	}
	for _, pattern := range banned {
		assert.False(t, strings.Contains(src, pattern),
			"manager_lifecycle.go must not contain %q — use the corresponding sentinel",
			pattern)
	}
}
