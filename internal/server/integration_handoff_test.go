package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/recovery"
	"github.com/stretchr/testify/require"
)

// handoffSource is a stopped coordinator in a new home, as a node leaves one:
// its provider tree, node.yaml, recovery layout and lock, a protected
// connection, an owner, a team and an app key, a route and spend, including
// the spend of a key its store lacks. It returns the home, the raw app key and
// the owner's session and app-key references issued before the handoff.
func handoffSource(t *testing.T) (home, raw string, session, app control.CredentialReference) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", home)
	_, err := templates.PrepareProviders(filepath.Join(home, "providers"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "node.yaml"), []byte("node:\n  name: coord-1\n  port: 9090\n"), 0o600))
	cfg, err := pkgConfig.ReadExistingNodeConfig(home)
	require.NoError(t, err)
	apps, err := pkgConfig.LoadAppsConfig(filepath.Join(home, "providers"))
	require.NoError(t, err)
	inventory, err := recovery.Capture(t.Context(), cfg, apps)
	require.NoError(t, err)
	require.NoError(t, recovery.Save(t.Context(), inventory))
	lock, err := pkgConfig.LockInstallation(home)
	require.NoError(t, errors.Join(err, lock.Close()))
	key, err := pkgConfig.PrepareCredentialKey(t.Context(), home, pkgConfig.CredentialsConfig{})
	require.NoError(t, err)
	defer clear(key)
	vault, err := secrets.Open(filepath.Join(home, "credentials.enc"), key)
	require.NoError(t, err)
	_, err = vault.Replace(t.Context(), "openai", 0, map[string]string{"api_key": "the source's provider secret"})
	require.NoError(t, errors.Join(err, vault.Close()))
	store, err := sqlstore.Open(t.Context(), home, sqlstore.Options{People: true, ValidateExternalIssuer: func(context.Context, string) error { return nil }})
	require.NoError(t, err)
	_, err = store.CreateTeam(t.Context(), "workspace", teams.Team{Name: "Workspace", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	_, raw, err = store.CreateKey(t.Context(), "editor", keys.VirtualKey{Name: "Editor", Role: "user", TeamID: "workspace", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	grant, err := store.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: people.Issuer{Reference: "test"}})
	require.NoError(t, err)
	bound, err := store.People().Exchange(t.Context(), grant.Token, "test-proof")
	require.NoError(t, err)
	_, err = store.People().ClaimOwner(t.Context(), bound.Token, "test-proof", people.CreatePerson{Username: "owner", Name: "Owner", Password: personPassword})
	require.NoError(t, err)
	core := control.NewCore(control.StaticKeySet{}, store, nil, nil, nil)
	session, err = core.PersonReference(store.Snapshot().Owner())
	require.NoError(t, err)
	app, err = core.Reference(raw)
	require.NoError(t, errors.Join(err, store.Close()))
	groups := modelgroup.NewGroupStore()
	require.NoError(t, groups.Set("assistant", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "old", Model: "qwen", App: "llamacpp", Node: "coord-1"}}}))
	require.NoError(t, groups.SaveToFile(filepath.Join(home, "model_groups.yaml")))
	require.NoError(t, quota.SaveSpend(filepath.Join(home, "spend.json"), &quota.SpendFile{Version: 1, States: map[string]*quota.SpendState{
		"team:workspace:": {Kind: quota.ScopeTeam, EntityID: "workspace", SpendMicro: 1750000},
		"key:ghost:":      {Kind: quota.ScopeKey, EntityID: "ghost", SpendMicro: 9},
	}}))
	return home, raw, session, app
}

// startNodeAt builds a coordinator on the existing home, with OAuth served at
// a test endpoint, as a node start does, and stops it when the test ends.
func startNodeAt(t *testing.T, home string) (*Server, *httptest.Server) {
	t.Helper()
	t.Setenv("ZZROUTER_TEST_HOME", home)
	endpoint := httptest.NewUnstartedServer(nil)
	t.Cleanup(endpoint.Close)
	s, err := NewServerWithOptions(&pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{Bind: "localhost", Port: getNextTestPort(), Name: "dest-1"},
		Auth: pkgConfig.AuthConfig{OAuth: pkgConfig.OAuthConfig{IssuerURL: "http://" + endpoint.Listener.Addr().String()}, AdminKey: TestAdminKey, UserKey: TestUserKey},
	})
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(s) })
	endpoint.Config.Handler = s.engine
	endpoint.Start()
	return s, endpoint
}

// readFiles reads each of names under home.
func readFiles(t *testing.T, home string, names ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(name)))
		require.NoError(t, err)
		out[name] = string(data)
	}
	return out
}

// TestIntegrationHandoffActivationStartsAsTheSource: a coordinator that
// started once takes a handoff, then starts on it in-process. The owner signs
// in with the old password, the raw app key authenticates, the session and
// app-key references issued before do not resolve, the source's connection
// decrypts, its route and spend total are there, the spend of a key the image
// lacks is gone, and the node's own node.yaml, static keys, CA and identity
// are byte for byte as before.
func TestIntegrationHandoffActivationStartsAsTheSource(t *testing.T) {
	previous := fsroot.ProviderRootDir()
	fsroot.SetProviderRootOverride(filepath.Join(t.TempDir(), "installed-providers"))
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previous) })
	source, raw, session, app := handoffSource(t)
	archive, key := filepath.Join(t.TempDir(), "handoff.tar.gz"), filepath.Join(t.TempDir(), "handoff.key")
	_, err := recovery.BackupProfile(t.Context(), recovery.ProfileHandoff, source, archive, key)
	require.NoError(t, err)

	home := t.TempDir()
	_, err = templates.PrepareProviders(filepath.Join(home, "providers"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "node.yaml"), []byte("node:\n  name: dest-1\n  port: 9090\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".env"), []byte("ZZROUTER_ADMIN_API_KEY=the destination's own\n"), 0o600))
	_, err = clusterid.LoadOrCreateCA(filepath.Join(home, "ca"))
	require.NoError(t, err)
	_, err = clusterid.LoadOrCreateIdentity(filepath.Join(home, "identity"))
	require.NoError(t, err)
	first, _ := startNodeAt(t, home)
	cleanupTestNode(first) // the destination ran once and stopped
	lock, err := pkgConfig.LockInstallation(home)
	require.NoError(t, errors.Join(err, lock.Close()))
	own := []string{"node.yaml", ".env", "ca/ca.key", "ca/ca.pem", "identity/node.key", "identity/node.pem"}
	before := readFiles(t, home, own...)
	staging := filepath.Join(t.TempDir(), "staged")
	_, err = recovery.Restore(t.Context(), archive, key, staging)
	require.NoError(t, err)
	a, err := recovery.Activate(t.Context(), home, staging, recovery.ActivateOptions{SourceStopped: true, Replace: true})
	require.NoError(t, err)
	require.Contains(t, a.Replaced, "store")
	require.Len(t, a.References, 1, "the route pinned to the source")

	s, endpoint := startNodeAt(t, home)
	browser := anonymousPeopleBrowser(t, endpoint)
	status, body := browser.call(t, http.MethodPost, "/zzrouter/v1/session/password", map[string]string{"username": "owner", "password": personPassword}, nil)
	require.Equal(t, http.StatusOK, status, "the owner signs in with the old password: %s", body)
	require.Equal(t, http.StatusOK, makeAuthRequest(t, s, http.MethodGet, "/v1/models", raw, nil).Code, "the raw app key authenticates")
	_, err = s.access.core.AuthenticateReference(session)
	require.ErrorIs(t, err, control.ErrInvalidKey, "the old session does not resolve")
	_, err = s.access.core.AuthenticateReference(app)
	require.ErrorIs(t, err, control.ErrInvalidKey, "the old app-key reference does not resolve")
	record, err := s.providers.connectionStore.Snapshot("openai")
	require.NoError(t, err)
	require.Equal(t, "the source's provider secret", record.Values["api_key"], "the source's connection decrypts")
	require.NotNil(t, s.model.Groups.Get("assistant"), "the source's route")
	spend, err := quota.LoadSpend(filepath.Join(home, "spend.json"))
	require.NoError(t, err)
	require.EqualValues(t, 1750000, spend.States["team:workspace:"].SpendMicro, "the source's spend total")
	require.NotContains(t, spend.States, "key:ghost:", "the spend of a key the image lacks is gone")
	require.Equal(t, before, readFiles(t, home, own...), "the node's own node.yaml, static keys, CA and identity")
}
