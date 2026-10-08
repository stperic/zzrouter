//go:build !windows

package service

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertEnvFilePermsRestricted verifies the env file is owner-only (0600)
// on Unix platforms.
func assertEnvFilePermsRestricted(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}
