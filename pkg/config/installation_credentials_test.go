package config

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallationCredentialsCreatesOnceAndPreservesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	const original = "# Keep operator settings\nPROVIDER_LABEL=\"my provider\"\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	first, err := EnsureInstallationCredentials(t.Context(), dir, InstallationCredentialOptions{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(first), 32)
	second, err := EnsureInstallationCredentials(t.Context(), dir, InstallationCredentialOptions{})
	require.NoError(t, err)
	require.Equal(t, first, second, "re-running an installer must never rotate access")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), original)
	require.Equal(t, 1, strings.Count(string(data), "ZZROUTER_ADMIN_API_KEY="))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestInstallationCredentialMigrationHonorsServiceProvenance(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "inert legacy", true: "active service"}[active], func(t *testing.T) {
			dir := t.TempDir()
			path, legacy := filepath.Join(dir, ".env"), filepath.Join(dir, "legacy.env")
			require.NoError(t, os.WriteFile(path, []byte("# canonical\nZZROUTER_ADMIN_API_KEY=synthetic-canonical-admin\nZZROUTER_API_KEY=synthetic-canonical-user\n"), 0o600))
			require.NoError(t, os.WriteFile(legacy, []byte("ZZROUTER_ADMIN_API_KEY=synthetic-legacy-admin\nZZROUTER_API_KEY=synthetic-legacy-user\nZZROUTER_CLUSTER_NETWORK_KEY=synthetic-cluster\n"), 0o600))
			key, err := EnsureInstallationCredentials(t.Context(), dir, InstallationCredentialOptions{LegacyFile: legacy, LegacyOverrides: active})
			require.NoError(t, err)
			expected := "synthetic-canonical-admin"
			if active {
				expected = "synthetic-legacy-admin"
			}
			require.Equal(t, expected, key)
			data, values, err := readInstallationEnvironment(path)
			require.NoError(t, err)
			require.Contains(t, string(data), "# canonical")
			require.Equal(t, "synthetic-cluster", values["ZZROUTER_CLUSTER_NETWORK_KEY"])
			// A later install after the service migration keeps the adopted key.
			key, err = EnsureInstallationCredentials(t.Context(), dir, InstallationCredentialOptions{LegacyFile: legacy})
			require.NoError(t, err)
			require.Equal(t, expected, key)
		})
	}
}

func TestInstallationCredentialsRefuseAmbiguousOrConflictingFiles(t *testing.T) {
	for name, data := range map[string]string{
		"duplicate": "ZZROUTER_ADMIN_API_KEY=first\nZZROUTER_ADMIN_API_KEY=second\n",
		"invalid":   "this is not an assignment\n",
		"too large": strings.Repeat("#", (1<<20)+1),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".env")
			require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
			_, err := EnsureInstallationCredentials(t.Context(), dir, InstallationCredentialOptions{})
			require.Error(t, err)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, data, string(after), "failed migration must leave the source unchanged")
		})
	}
	dir := t.TempDir()
	path, legacy := filepath.Join(dir, ".env"), filepath.Join(dir, "legacy.env")
	require.NoError(t, os.WriteFile(path, []byte("PROVIDER_LABEL=canonical\n"), 0o600))
	require.NoError(t, os.WriteFile(legacy, []byte("PROVIDER_LABEL=legacy\n"), 0o600))
	_, err := EnsureInstallationCredentials(t.Context(), dir, InstallationCredentialOptions{LegacyFile: legacy, LegacyOverrides: true})
	require.ErrorContains(t, err, "conflicting installation settings")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = EnsureInstallationCredentials(ctx, t.TempDir(), InstallationCredentialOptions{})
	require.ErrorIs(t, err, context.Canceled)
}
