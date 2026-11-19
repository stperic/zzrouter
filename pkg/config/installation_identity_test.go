package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallationIdentityUsesConfigParserWithoutChangingFiles(t *testing.T) {
	dir := t.TempDir()
	const data = `node: {name: "fixture node", bind: "127.0.0.2", port: 9190, tls_cert: "cert", tls_key: "key"}
cluster:
  mode: 'worker' # Keep inline comments and quoted scalars.
  bind_port: 9191
auth: {admin_key: "synthetic-secret-never-in-identity"}
`
	path := filepath.Join(dir, "node.yaml")
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
	identity, err := ReadInstallationIdentity(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, &InstallationIdentity{Name: "fixture node", Bind: "127.0.0.2", Port: 9190, Mode: ClusterModeWorker, ClusterPort: 9191, Secure: true, ConfigPath: path}, identity)
	encoded, err := json.Marshal(identity)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "synthetic-secret")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, string(after))
}

func TestInstallationIdentityRefusesMissingConfigInsteadOfInitializing(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadInstallationIdentity(t.Context(), dir)
	require.Error(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
