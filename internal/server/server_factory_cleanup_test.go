package server

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestConstructorRefusesAStoreThatCannotOpen: a coordinator whose access
// store cannot open is not built, and the error is the store's refusal.
func TestConstructorRefusesAStoreThatCannotOpen(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	database := func(dir string) string { return filepath.Join(dir, "access", "access.sqlite") }
	for _, tc := range []struct {
		reason sqlstore.Reason
		damage func(t *testing.T, dir string)
	}{
		{sqlstore.Missing, func(t *testing.T, dir string) { require.NoError(t, os.Remove(database(dir))) }},
		{sqlstore.Unreadable, func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(database(dir), []byte("not a database, just text long enough to have a header"), 0o600))
		}},
		// A valid member key must not run without its team policy.
		{sqlstore.IntegrityFailed, func(t *testing.T, dir string) {
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(database(dir)))
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			_, err = db.ExecContext(t.Context(), `UPDATE teams SET provider_access = '{"mode":"select","providers":["ollama"]}'`)
			require.NoError(t, err)
		}},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			modelregistry.SetModelsRootDirOverride(t.TempDir())
			defer modelregistry.ClearModelsRootDirOverride()
			dir := config.NewConfigManager("zzrouter").GetNodeConfigDir()
			require.NoError(t, os.MkdirAll(dir, 0o700))
			fixture, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
			require.NoError(t, err)
			_, err = fixture.CreateTeam(t.Context(), "restricted", teams.Team{Name: "Restricted", Kind: teams.KindShared, ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"ollama"}}}, audit.Event{})
			require.NoError(t, err)
			seedTestKey(t, fixture, "client", keys.VirtualKey{Name: "Client", TeamID: "restricted", TeamRole: "member", Role: "user"})
			require.NoError(t, fixture.Close())
			tc.damage(t, dir)
			cfg := &config.NodeConfig{Node: config.ServeConfig{Bind: "localhost", Port: 19191, Name: "fixture"}, Auth: config.AuthConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, ClusterNetworkKey: TestClusterKey}}
			server, err := NewServerWithOptions(cfg)
			require.Nil(t, server)
			var refusal *sqlstore.Refusal
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, tc.reason, refusal.Reason)
		})
	}
}

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
		// The access store opened before the lock refusal; goleak sees its connection if it stays open.
	}
}
