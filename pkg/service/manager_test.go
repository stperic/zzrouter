package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	vars := map[string]string{
		"KEY_A": "value_a",
		"KEY_B": "value_b",
	}

	err := writeEnvFile(path, vars)
	require.NoError(t, err)

	content, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Contains(t, string(content), "KEY_A=value_a")
	assert.Contains(t, string(content), "KEY_B=value_b")

	// Platform-specific access-restriction check — env files contain admin
	// and cluster API keys, so verify the file is not broadly readable.
	// See env_file_unix_test.go / env_file_windows_test.go.
	assertEnvFilePermsRestricted(t, path)
}

func TestStatusType(t *testing.T) {
	s := &Status{
		Running:   true,
		Enabled:   true,
		PID:       1234,
		User:      "zzrouter",
		ConfigDir: "/etc/zzrouter",
		LogPath:   "/var/log/zzrouter",
	}

	assert.True(t, s.Running)
	assert.True(t, s.Enabled)
	assert.Equal(t, 1234, s.PID)
	assert.Empty(t, s.Error)
}
