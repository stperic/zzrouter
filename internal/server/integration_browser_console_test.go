package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationBrowserConsoleFixedAssetsAndRoleGate(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeDisabled)
	for _, path := range []string{"/console/", "/console/console.css", "/console/console.mjs", "/console/api.mjs", "/console/dom.mjs", "/console/providers.mjs", "/console/access.mjs"} {
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
		assert.Equal(t, "same-origin", rec.Header().Get("Referrer-Policy"))
		assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "script-src 'self'")
		assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
		assert.NotContains(t, rec.Header().Get("Content-Security-Policy"), "unsafe-inline")
		assert.NotContains(t, rec.Body.String(), TestAdminKey)
		assert.NotContains(t, rec.Body.String(), "innerHTML")
		assert.NotContains(t, rec.Body.String(), "localStorage")
		assert.NotContains(t, rec.Body.String(), "sessionStorage")
		if strings.HasSuffix(path, ".mjs") {
			assert.Equal(t, "text/javascript; charset=utf-8", rec.Header().Get("Content-Type"))
		}
	}
	for _, path := range []string{"/console/.env", "/console/index.html", "/console/tests/api.test.mjs", "/console/unknown.mjs"} {
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, path)
	}
	require.NoError(t, s.role.Set(t.Context(), role.RoleWorker, "console gate fixture"))
	rec := httptest.NewRecorder()
	s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	require.NoError(t, s.role.Set(t.Context(), role.RoleCoordinator, "console gate restored"))
	rec = httptest.NewRecorder()
	s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestIntegrationBrowserConsoleUnavailableWithoutAuthority(t *testing.T) {
	for _, cfg := range []TestNodeConfig{
		{AdminKey: TestAdminKey, OAuthEnabled: new(false)},
		{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker},
	} {
		s := createTestNode(t, cfg)
		for _, path := range []string{"/console/", "/console/api.mjs"} {
			rec := httptest.NewRecorder()
			s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			assert.Equal(t, http.StatusNotFound, rec.Code)
		}
	}
}

func TestIntegrationBrowserConsoleSafeSelfProjection(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	admin := newSessionBrowser(t, endpoint, TestAdminKey)
	status, body := admin.call(t, http.MethodGet, "/zzrouter/v1/session", nil, map[string]string{"X-Forwarded-Host": "wrong.example"})
	require.Equal(t, http.StatusOK, status)
	var bootstrap struct{ Data browserSessionState }
	require.NoError(t, json.Unmarshal(body, &bootstrap))
	assert.Equal(t, endpoint.URL, bootstrap.Data.Issuer)
	assert.Equal(t, s.GetNodename(), bootstrap.Data.NodeName)
	assert.NotContains(t, string(body), "wrong.example")
	status, body = admin.call(t, http.MethodPost, "/zzrouter/v1/teams", map[string]any{
		"id": "console-readers", "name": "Console readers", "allowed_models": []string{"fixture-model"}, "rpm_limit": 20,
		"metadata": map[string]string{"private": "team-metadata-must-stay-private"},
	}, nil)
	require.Equal(t, http.StatusCreated, status, string(body))
	status, body = admin.call(t, http.MethodPost, "/zzrouter/v1/keys", map[string]any{
		"id": "console-reader", "name": "Console reader", "role": "user", "team_id": "console-readers", "rpm_limit": 10,
		"expires_at": utils.NowUTC().Add(time.Hour).Format(time.RFC3339),
		"metadata":   map[string]string{"private": "key-metadata-must-stay-private"},
	}, nil)
	require.Equal(t, http.StatusCreated, status, string(body))
	var created struct{ Data CreateKeyResponse }
	require.NoError(t, json.Unmarshal(body, &created))
	reader := newSessionBrowser(t, endpoint, created.Data.RawKey)
	status, body = reader.call(t, http.MethodGet, "/zzrouter/v1/me", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var self struct{ Data browserIdentity }
	require.NoError(t, json.Unmarshal(body, &self))
	assert.Equal(t, "console-reader", self.Data.ID)
	assert.True(t, self.Data.IsVirtual)
	assert.Equal(t, 10, self.Data.Limits.RPMLimit)
	require.NotNil(t, self.Data.ExpiresAt)
	require.NotNil(t, self.Data.Team)
	assert.Equal(t, 20, self.Data.Team.Limits.RPMLimit)
	assert.Equal(t, []string{"fixture-model"}, self.Data.Team.AllowedModels)
	require.NotNil(t, self.Data.Usage)
	assert.Equal(t, "console-reader", self.Data.Usage.KeyID)
	for _, forbidden := range []string{created.Data.RawKey, "metadata", "fingerprint", "hashed_key", "team-metadata-must-stay-private", "key-metadata-must-stay-private"} {
		assert.NotContains(t, string(body), forbidden)
	}
	status, _ = reader.call(t, http.MethodGet, "/zzrouter/v1/keys", nil, nil)
	assert.Equal(t, http.StatusForbidden, status)
}

func TestIntegrationBrowserConsoleManagedConnectionRoundTrip(t *testing.T) {
	// Override each declared external source before constructing the real
	// server, so no developer credential or HF fallback file is read.
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	declared, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	declared.RangeApps(func(_ string, provider config.ServiceConfig) bool {
		if provider.Connection != nil {
			for _, field := range provider.Connection.Fields {
				for _, name := range append([]string{field.Env}, field.EnvAliases...) {
					t.Setenv(name, "console-external-fixture")
				}
			}
		}
		return true
	})
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, "Bearer console-managed-fixture", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(upstream.Close)
	keyFile := filepath.Join(t.TempDir(), "protection.key")
	require.NoError(t, os.WriteFile(keyFile, bytes.Repeat([]byte{42}, 32), 0o600))
	endpoint := httptest.NewUnstartedServer(nil)
	t.Cleanup(endpoint.Close)
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, OAuthIssuer: "http://" + endpoint.Listener.Addr().String(), SeedProvidersDir: true, CredentialsKeyFile: keyFile})
	require.NoError(t, s.configStore.ApplyParameterPatch("openai", func(provider *config.ServiceConfig) error { provider.Runtime.Endpoint = upstream.URL; return nil }))
	endpoint.Config.Handler = s.engine
	endpoint.Start()
	browser := newSessionBrowser(t, endpoint, TestAdminKey)
	native := client.NewClient(config.ClientNodeConfig{Address: endpoint.URL, APIKey: TestAdminKey})
	before, err := native.GetProviderConnection(t.Context(), "openai")
	require.NoError(t, err)
	values := map[string]string{"api_key": "console-managed-fixture"}
	status, body := browser.call(t, http.MethodPost, "/zzrouter/v1/providers/openai/connection/test", map[string]any{"values": values}, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.EqualValues(t, 1, requests.Load())
	afterTest, err := native.GetProviderConnection(t.Context(), "openai")
	require.NoError(t, err)
	assert.Equal(t, before.Revision, afterTest.Revision)
	assert.Equal(t, before.Mode, afterTest.Mode)
	status, body = browser.call(t, http.MethodPut, "/zzrouter/v1/providers/openai/connection", map[string]any{"values": values, "expected_revision": before.Revision}, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.EqualValues(t, 1, requests.Load(), "saving never tests the upstream")
	afterSave, err := native.GetProviderConnection(t.Context(), "openai")
	require.NoError(t, err)
	assert.Equal(t, "managed", afterSave.Mode)
	assert.Greater(t, afterSave.Revision, before.Revision)
	assert.NotContains(t, string(body), "console-managed-fixture")
	assert.NotContains(t, afterSave.Values, "api_key")
}

func TestBrowserConsoleProjectionOpenAPIFields(t *testing.T) {
	schemas, ok := mapAt(loadSpecTree(t)["components"], "schemas")
	require.True(t, ok)
	for name, typ := range map[string]reflect.Type{
		"BrowserSessionState": reflect.TypeOf(browserSessionState{}), "BrowserIdentity": reflect.TypeOf(browserIdentity{}),
		"BrowserLimits": reflect.TypeOf(browserLimits{}), "BrowserTeam": reflect.TypeOf(browserTeam{}),
	} {
		spec, ok := mapAt(schemas, name)
		require.True(t, ok, name)
		properties, ok := mapAt(spec, "properties")
		require.True(t, ok, name)
		want := goJSONTypeMap(typ)
		assert.Len(t, properties, len(want), name)
		for field := range want {
			assert.Contains(t, properties, field, name)
		}
	}
}
