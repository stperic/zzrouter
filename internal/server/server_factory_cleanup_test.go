package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestConstructorFailureReleasesCredentialOwnersAndGoroutines(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	modelregistry.SetModelsRootDirOverride(t.TempDir())
	defer modelregistry.ClearModelsRootDirOverride()
	dir := config.NewConfigManager("zzrouter").GetNodeConfigDir()
	require.NoError(t, os.MkdirAll(dir, 0700))
	lock, err := atomicfile.Lock(filepath.Join(dir, "oauth.json.lock"))
	require.NoError(t, err)
	defer func() { _ = lock.Close() }()
	keyPath := filepath.Join(t.TempDir(), "fixture-key")
	key := bytes.Repeat([]byte{6}, 32)
	require.NoError(t, os.WriteFile(keyPath, key, 0600))
	for range 2 {
		cfg := &config.NodeConfig{Node: config.ServeConfig{Bind: "localhost", Port: 19191, Name: "fixture"}, Auth: config.AuthConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, ClusterNetworkKey: TestClusterKey, OAuth: config.OAuthConfig{Enabled: new(true), IssuerURL: "http://localhost:19191"}}, Security: config.SecurityConfig{Credentials: config.CredentialsConfig{KeyFile: keyPath}}}
		server, err := NewServerWithOptions(cfg)
		require.Nil(t, server)
		require.ErrorIs(t, err, atomicfile.ErrLocked)
		vault, err := secrets.Open(filepath.Join(dir, "credentials.enc"), key)
		require.NoError(t, err, "failed constructor must release its already-open credential store")
		require.NoError(t, vault.Close())
	}
}
