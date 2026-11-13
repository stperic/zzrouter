package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/common/atomicfile"
	"github.com/stperic/common/secrets"
	client "github.com/stperic/zzrouter/internal/client/utils"
	config "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
)

type connectionRegistrar struct {
	store   *config.AppsConfigStore
	failure error
	calls   int
}

func TestConnectionChangeKeepsRegistryDistinctFromCloudProvider(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	t.Setenv("CONNECTION_VERSION_TOKEN", "")
	previousRoot := fsroot.ProviderRootDir()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previousRoot) })
	cfg := &config.AppsConfig{}
	for _, name := range []string{"huggingface", "openai"} {
		mode := "registry"
		if name == "openai" {
			mode = "cloud"
		}
		require.NoError(t, cfg.AddApp(name, config.ServiceConfig{Mode: mode, Enabled: new(true), Protocol: config.ProtocolOpenAI,
			Runtime:      &config.AppRuntimeConfig{Endpoint: "https://fixture.invalid", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "fixture"}},
			Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
			Connection:   &config.ConnectionConfig{Fields: []config.ConnectionField{{ID: "token", Label: "Token", Type: "text", Secret: true, Env: "CONNECTION_VERSION_TOKEN"}}},
		}))
	}
	resolver := backend.NewResolver(func() *config.AppsConfig { return cfg })
	manager, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithConnectionResolver(resolver))
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.Stop(t.Context()) })
	s := &Server{shutdownCtx: t.Context(), backend: resolver}
	s.providers.appMgr = manager
	for _, name := range []string{"huggingface", "openai"} {
		s.connectionChanged(name)
	}
	require.Equal(t, "unknown", manager.ProviderVersion("huggingface"))
	require.Equal(t, "cloud", manager.ProviderVersion("openai"))
}

func (r *connectionRegistrar) FinalizeOnboarding(name string) error {
	r.calls++
	if r.failure != nil {
		return r.failure
	}
	_, err := r.store.SetProviderEnabled(name, "", true)
	return err
}
func (r *connectionRegistrar) FinalizeOffboarding(string) error { return nil }

type connectionHarness struct {
	service   *ProviderConnectionService
	vault     *secrets.Store
	config    *config.AppsConfigStore
	registrar *connectionRegistrar
	client    *client.Client
	engine    *gin.Engine
	path      string
	key       []byte
	published int
}

func newConnectionHarness(t *testing.T, endpoint string) *connectionHarness {
	t.Helper()
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("fixture", config.ServiceConfig{Mode: "cloud", Enabled: new(false), Protocol: config.ProtocolOpenAI,
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		Runtime:      &config.AppRuntimeConfig{Endpoint: "${CONNECTION_TEST_ENDPOINT}", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "${CONNECTION_TEST_TOKEN}"}},
		Connection: &config.ConnectionConfig{Fields: []config.ConnectionField{
			{ID: "endpoint", Label: "Endpoint", Type: "endpoint", Env: "CONNECTION_TEST_ENDPOINT", Default: endpoint, Required: true},
			{ID: "token", Label: "Token", Type: "text", Env: "CONNECTION_TEST_TOKEN", Secret: true, Required: true},
		}},
	}))
	t.Setenv("CONNECTION_TEST_ENDPOINT", endpoint)
	t.Setenv("CONNECTION_TEST_TOKEN", "external-connection-fixture")
	configDir := t.TempDir()
	require.NoError(t, cfg.SaveToDir(configDir))
	configStore, err := config.NewAppsConfigStore(configDir)
	require.NoError(t, err)
	h := &connectionHarness{config: configStore, path: filepath.Join(t.TempDir(), "connections.enc"), key: bytes.Repeat([]byte{4}, 32)}
	h.vault, err = secrets.Open(h.path, h.key)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.vault.Close() })
	h.registrar = &connectionRegistrar{store: h.config}
	h.service = &ProviderConnectionService{resolver: backend.NewResolver(h.config.Config, h.vault), store: h.vault, registrar: h.registrar, client: &http.Client{}, coordinator: func() bool { return true }, publish: func() error { h.published++; return nil }}
	h.engine = gin.New()
	(&ProviderConnectionController{service: h.service}).RegisterPublicRoutes(h.engine.Group("/zzrouter/v1"))
	server := httptest.NewServer(h.engine)
	t.Cleanup(server.Close)
	h.client = client.NewClient(config.ClientNodeConfig{Address: server.URL})
	return h
}

func TestConnectionHTTPWorkflowAndActualForwarding(t *testing.T) {
	var seen atomic.Value
	seen.Store("")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()
	h := newConnectionHarness(t, upstream.URL)
	candidate := map[string]string{"endpoint": upstream.URL, "token": "managed-connection-fixture"}
	before, err := h.client.GetProviderConnection(t.Context(), "fixture")
	require.NoError(t, err)
	require.Equal(t, "coordinator", before.Scope)
	require.Equal(t, "external", before.Mode)
	require.NotContains(t, before.Values, "token")
	tested, err := h.client.TestProviderConnection(t.Context(), "fixture", candidate)
	require.NoError(t, err)
	require.Equal(t, "ok", tested.Status)
	require.Equal(t, "Bearer managed-connection-fixture", seen.Load())
	after, err := h.client.GetProviderConnection(t.Context(), "fixture")
	require.NoError(t, err)
	require.Equal(t, before, after, "Test leaves all authoritative state alone")
	require.Zero(t, h.registrar.calls)
	require.Zero(t, h.published)
	saved, err := h.client.SaveProviderConnection(t.Context(), "fixture", 0, candidate, true)
	require.NoError(t, err)
	require.True(t, saved.Saved)
	require.True(t, saved.Durable)
	require.True(t, saved.Active)
	require.True(t, saved.Published)
	require.Equal(t, uint64(1), saved.Connection.Revision)
	require.Equal(t, 1, h.registrar.calls)
	// The actual proxy receives the same resolved upstream as the management workflow.
	proxy := NewProxyClient(upstream.Client(), nil, nil, nil, nil, nil, nil)
	for revision, token := range []string{"managed-connection-fixture", "replacement-connection-fixture"} {
		if revision > 0 {
			_, err = h.client.SaveProviderConnection(t.Context(), "fixture", 1, map[string]string{"endpoint": upstream.URL, "token": token}, false)
			require.NoError(t, err)
		}
		target, ok := h.service.resolver.Resolve("fixture")
		require.True(t, ok)
		response, err := proxy.ForwardDetached(t.Context(), http.MethodPost, target.Endpoint+"/v1/chat/completions", http.Header{"Authorization": {"Bearer inbound-admin"}}, target.Upstream, []byte(`{}`))
		require.NoError(t, err)
		_ = response.Body.Close()
		require.Equal(t, "Bearer "+token, seen.Load())
	}
	body, err := os.ReadFile(h.path)
	require.NoError(t, err)
	require.NotContains(t, string(body), "managed-connection-fixture")
	result, err := h.client.RemoveProviderConnection(t.Context(), "fixture", 2)
	require.NoError(t, err)
	require.True(t, result.Saved)
	require.False(t, result.Active)
	require.Equal(t, "disabled", result.Connection.Mode)
	_, ok := h.service.resolver.Resolve("fixture")
	require.False(t, ok, "removal suppresses the still-configured external token")
	require.NoError(t, h.vault.Close())
	reopened, err := secrets.Open(h.path, h.key)
	require.NoError(t, err)
	h.vault = reopened
	h.service.store = reopened
	h.service.resolver = backend.NewResolver(h.config.Config, reopened)
	state, err := h.client.GetProviderConnection(t.Context(), "fixture")
	require.NoError(t, err)
	require.Equal(t, "disabled", state.Mode)
	result, err = h.client.UseExternalProviderConnection(t.Context(), "fixture", 3)
	require.NoError(t, err)
	require.True(t, result.Active)
	target, ok := h.service.resolver.Resolve("fixture")
	require.True(t, ok)
	require.Equal(t, "Bearer external-connection-fixture", target.Upstream.Header().Get("Authorization"))
	_, err = h.client.SaveProviderConnection(t.Context(), "fixture", 4, map[string]string{"endpoint": "https://other.example"}, false)
	require.Error(t, err, "destination changes cannot silently reuse an existing credential")
}

func TestConnectionProbeRefusesRedirectAndRedactsBodies(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1); w.WriteHeader(200) }))
	defer destination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer redirect-token" {
			http.Redirect(w, r, destination.URL, http.StatusFound)
			return
		}
		w.WriteHeader(401)
		_, _ = io.WriteString(w, r.Header.Get("Authorization")+" secret upstream detail")
	}))
	defer upstream.Close()
	h := newConnectionHarness(t, upstream.URL)
	for _, token := range []string{"redirect-token", "rejected-token"} {
		result, err := h.client.TestProviderConnection(t.Context(), "fixture", map[string]string{"endpoint": upstream.URL, "token": token})
		require.NoError(t, err)
		require.Equal(t, "failed", result.Status)
		encoded, err := json.Marshal(result)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), token)
		require.NotContains(t, string(encoded), "secret upstream detail")
	}
	require.Zero(t, destinationCalls.Load())
	require.Zero(t, h.registrar.calls)
	state, err := h.vault.Snapshot("fixture")
	require.NoError(t, err)
	require.Zero(t, state.Revision)
}

type failedConnectionStore struct {
	connectionStore
	err         error
	afterCommit bool
}

func (s failedConnectionStore) Replace(ctx context.Context, name string, revision uint64, values map[string]string) (secrets.Metadata, error) {
	if s.afterCommit {
		meta, err := s.connectionStore.Replace(ctx, name, revision, values)
		if err != nil {
			return meta, err
		}
		return meta, s.err
	}
	return secrets.Metadata{}, s.err
}

func TestConnectionSaveTruthfulFailureOutcomes(t *testing.T) {
	h := newConnectionHarness(t, "https://fixture.example")
	values := map[string]string{"endpoint": "https://fixture.example", "token": "candidate-fixture"}
	h.service.store = failedConnectionStore{connectionStore: h.vault, err: errors.New("private path and secret details")}
	_, err := h.service.Save(t.Context(), "fixture", 0, values, true)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private path")
	require.Zero(t, h.registrar.calls)
	state, err := h.vault.Snapshot("fixture")
	require.NoError(t, err)
	require.Zero(t, state.Revision)
	h.service.store = h.vault
	h.registrar.failure = errors.New("activation failed")
	result, err := h.service.Save(t.Context(), "fixture", 0, values, true)
	require.NoError(t, err)
	require.True(t, result.Saved)
	require.False(t, result.Active)
	require.Equal(t, "saved_activation_failed", result.Outcome)
	h.registrar.failure = nil
	h.service.publish = func() error { return errors.New("publication failed") }
	result, err = h.service.Save(t.Context(), "fixture", 1, values, true)
	require.NoError(t, err)
	require.True(t, result.Saved)
	require.True(t, result.Active)
	require.False(t, result.Published)
	require.Equal(t, "saved_publication_failed", result.Outcome)
	h.service.store = failedConnectionStore{connectionStore: h.vault, err: atomicfile.ErrPostRenameDurability, afterCommit: true}
	result, err = h.service.Save(t.Context(), "fixture", 2, values, false)
	require.NoError(t, err)
	require.True(t, result.Saved)
	require.False(t, result.Durable)
	require.Equal(t, uint64(3), result.Connection.Revision)
	require.Equal(t, "saved_durability_uncertain", result.Outcome)
}

func TestConnectionConcurrentEditorsRequireCurrentRevision(t *testing.T) {
	h := newConnectionHarness(t, "https://fixture.example")
	var wg sync.WaitGroup
	errorsCh := make(chan error, 2)
	for _, token := range []string{"first-editor", "second-editor"} {
		wg.Go(func() {
			_, err := h.service.Save(t.Context(), "fixture", 0, map[string]string{"endpoint": "https://fixture.example", "token": token}, false)
			errorsCh <- err
		})
	}
	wg.Wait()
	close(errorsCh)
	saved, conflicts := 0, 0
	for err := range errorsCh {
		if err == nil {
			saved++
			continue
		}
		var problem httperr.Error
		require.ErrorAs(t, err, &problem)
		require.Equal(t, 409, problem.Status)
		conflicts++
	}
	require.Equal(t, 1, saved)
	require.Equal(t, 1, conflicts)
	require.Equal(t, 1, h.published)
}

func TestConnectionBoundsUnknownFieldsAndScope(t *testing.T) {
	h := newConnectionHarness(t, "https://fixture.example")
	for _, test := range []struct {
		body, path string
		status     int
	}{
		{`{"expected_revision":0,"values":{"token":"sensitive-fixture","unexpected":"ignored"}}`, "", 400},
		{`{"sensitive-fixture":"unknown"}`, "", 400},
		{`{"values":{}}`, "", 400},
		{`{"expected_revision":0,"values":{"token":"` + strings.Repeat("x", 70<<10) + `"}}`, "", 413},
		{`{"expected_revision":0,"values":{}}`, "?node=worker", 400},
		{`{"expected_revision":0,"values":{}} {}`, "", 400},
	} {
		recorder := httptest.NewRecorder()
		h.engine.ServeHTTP(recorder, httptest.NewRequest("PUT", "/zzrouter/v1/providers/fixture/connection"+test.path, strings.NewReader(test.body)))
		require.Equal(t, test.status, recorder.Code, recorder.Body.String())
		require.NotContains(t, recorder.Body.String(), "sensitive-fixture")
		require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	}
	h.service.store = nil
	_, err := h.client.SaveProviderConnection(t.Context(), "fixture", 0, map[string]string{"token": "value"}, false)
	require.Error(t, err)
	h.service.coordinator = func() bool { return false }
	_, err = h.client.GetProviderConnection(t.Context(), "fixture")
	require.Error(t, err)
}

func TestConnectionRoutesRequireCoordinatorAdmin(t *testing.T) {
	node := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, ClusterMode: config.ClusterModeCoordinator})
	for _, tc := range []struct{ method, path string }{{"GET", ""}, {"PUT", ""}, {"DELETE", ""}, {"POST", "/test"}, {"POST", "/use-external"}} {
		path := "/zzrouter/v1/providers/openai/connection" + tc.path
		for _, auth := range []struct {
			key    string
			status int
		}{{"", 401}, {TestUserKey, 403}} {
			result := makeAuthRequest(t, node, tc.method, path, auth.key, nil)
			require.Equal(t, auth.status, result.Code, string(result.Body))
		}
	}
	worker := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker})
	result := makeAuthRequest(t, worker, "GET", "/zzrouter/v1/providers/openai/connection", TestAdminKey, nil)
	require.Equal(t, http.StatusMisdirectedRequest, result.Code)
}

func TestConnectionProtectionStartup(t *testing.T) {
	dir := t.TempDir()
	store, err := openProviderConnectionStore(config.CredentialsConfig{}, dir)
	require.NoError(t, err)
	require.Nil(t, store)
	_, err = openProviderConnectionStore(config.CredentialsConfig{KeyFile: "missing-test-key"}, dir)
	require.Error(t, err)
	keyPath := filepath.Join(t.TempDir(), "test-key")
	require.NoError(t, os.WriteFile(keyPath, bytes.Repeat([]byte{3}, 32), 0600))
	store, err = openProviderConnectionStore(config.CredentialsConfig{KeyFile: keyPath}, dir)
	require.NoError(t, err)
	_, err = store.Remove(t.Context(), "fixture", 0)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	_, err = openProviderConnectionStore(config.CredentialsConfig{}, dir)
	require.ErrorContains(t, err, "existing protected connections")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), "key", "startup never generates a neighboring encryption key")
	}
}

func TestConnectionDeclaredDefaultsHTTPTestAndSave(t *testing.T) {
	for _, blank := range []bool{false, true} {
		name := "omitted"
		if blank {
			name = "blank"
		}
		t.Run(name, func(t *testing.T) {
			var probes atomic.Int32
			var seen atomic.Value
			seen.Store(http.Header{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				seen.Store(r.Header.Clone())
				_, _ = io.WriteString(w, `{"data":[]}`)
			}))
			defer upstream.Close()
			h := newConnectionHarness(t, upstream.URL)
			require.NoError(t, h.config.ApplyParameterPatch("fixture", func(svc *config.ServiceConfig) error {
				svc.Connection.Fields = append(svc.Connection.Fields, config.ConnectionField{ID: "account", Label: "Account", Type: "identifier", Env: "CONNECTION_TEST_ACCOUNT", Required: true, Default: "default_account"})
				svc.Runtime.API.CustomHeaders = map[string]string{"X-Account": "${CONNECTION_TEST_ACCOUNT}"}
				return nil
			}))
			t.Setenv("CONNECTION_TEST_ENDPOINT", "https://ambient.example")
			t.Setenv("CONNECTION_TEST_ACCOUNT", "ambient_account")
			candidate := map[string]string{"token": "candidate-default-token"}
			if blank {
				candidate["endpoint"], candidate["account"] = "", ""
			}
			tested, err := h.client.TestProviderConnection(t.Context(), "fixture", candidate)
			require.NoError(t, err)
			require.Equal(t, "ok", tested.Status)
			require.EqualValues(t, 1, probes.Load())
			require.Equal(t, "Bearer candidate-default-token", seen.Load().(http.Header).Get("Authorization"))
			require.Equal(t, "default_account", seen.Load().(http.Header).Get("X-Account"))
			state, err := h.vault.Snapshot("fixture")
			require.NoError(t, err)
			require.Zero(t, state.Revision, "Test must not persist candidate defaults or credentials")
			saved, err := h.client.SaveProviderConnection(t.Context(), "fixture", 0, candidate, true)
			require.NoError(t, err)
			require.True(t, saved.Saved)
			require.True(t, saved.Active)
			require.Equal(t, upstream.URL, saved.Connection.Values["endpoint"])
			require.Equal(t, "default_account", saved.Connection.Values["account"])
			target, ok := h.service.resolver.Resolve("fixture")
			require.True(t, ok)
			require.Equal(t, upstream.URL, target.Endpoint)
			require.Equal(t, "Bearer candidate-default-token", target.Upstream.Header().Get("Authorization"))
			require.Equal(t, "default_account", target.Upstream.Header().Get("X-Account"))
			require.Equal(t, "", candidate["endpoint"], "client input must stay unmodified")
			require.Equal(t, "", candidate["account"])
		})
	}
}

func TestExternalConnectionStatusSeparatesManagedDefaults(t *testing.T) {
	const managedDefault = "https://bedrock-runtime.us-east-1.amazonaws.com"
	const externalDefault = "https://bedrock-runtime.eu-west-1.amazonaws.com"
	h := newConnectionHarness(t, managedDefault)
	require.NoError(t, h.config.ApplyParameterPatch("fixture", func(svc *config.ServiceConfig) error {
		svc.Runtime.Endpoint = "${CONNECTION_TEST_ENDPOINT:-" + externalDefault + "}"
		return nil
	}))
	t.Setenv("CONNECTION_TEST_ENDPOINT", "")
	status, err := h.client.GetProviderConnection(t.Context(), "fixture")
	require.NoError(t, err)
	require.Equal(t, "external", status.Mode)
	require.True(t, status.Configured)
	require.Equal(t, managedDefault, status.Fields[0].Default)
	require.Contains(t, status.Values, "endpoint", "clients must receive an explicit empty external value")
	require.Empty(t, status.Values["endpoint"])
	require.NotContains(t, status.Present, "endpoint")
	target, ok := h.service.resolver.Target("fixture")
	require.True(t, ok)
	require.Equal(t, externalDefault, target.Endpoint)
	const explicit = "https://bedrock-runtime.ap-southeast-2.amazonaws.com"
	t.Setenv("CONNECTION_TEST_ENDPOINT", explicit)
	status, err = h.client.GetProviderConnection(t.Context(), "fixture")
	require.NoError(t, err)
	require.Equal(t, explicit, status.Values["endpoint"])
	require.Contains(t, status.Present, "endpoint")
}
