package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/common/atomicfile"
	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

type sessionBrowser struct {
	client   *http.Client
	endpoint string
	csrf     string
}

func TestIntegrationBrowserRouteCatalogAuthority(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeDisabled)
	response := makeAuthRequest(t, s, http.MethodGet, apipath.ServerRoutes, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, response.Code)
	var catalog struct {
		Routes []RouteSpec `json:"routes"`
	}
	require.NoError(t, json.Unmarshal(response.Body, &catalog))
	want := map[string]string{
		"GET " + apipath.Session: "optional", "POST " + apipath.Session: "optional", "DELETE " + apipath.Session: "optional",
		"GET " + apipath.Me: "user", "GET " + apipath.MeConnections: "user", "DELETE " + apipath.MeConnections + "/:id": "user",
	}
	for _, route := range catalog.Routes {
		key := route.Method + " " + route.Path
		if auth, ok := want[key]; ok {
			require.Equal(t, auth, route.Auth, key)
			_, err := s.eligibleAPIRoute(route.Method, route.Path)
			require.Error(t, err, "browser/self routes must not enter the MCP admin escape hatch")
			delete(want, key)
		}
	}
	require.Empty(t, want, "all browser/self routes must be catalogued")
}

func TestIntegrationBrowserAuthoritySurvivesRoleCycleAndClosesOnStop(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	browser := newSessionBrowser(t, endpoint, TestAdminKey)
	require.NoError(t, s.role.Set(t.Context(), role.RoleWorker, "test demotion"))
	status, _ := browser.call(t, http.MethodGet, "/zzrouter/v1/session", nil, nil)
	require.Equal(t, http.StatusMisdirectedRequest, status)
	status, _ = browser.call(t, http.MethodGet, "/oauth/connections", nil, nil)
	require.Equal(t, http.StatusNotFound, status)
	status, _ = browser.call(t, http.MethodGet, "/zzrouter/v1/nodes", nil, nil)
	require.Equal(t, http.StatusUnauthorized, status, "mounted management routes must not accept browser authority after demotion")
	require.NoError(t, s.role.Set(t.Context(), role.RoleCoordinator, "test re-promotion"))
	status, body := browser.call(t, http.MethodGet, "/zzrouter/v1/me", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	require.NoError(t, s.role.Set(t.Context(), role.RoleWorker, "stop as worker"))
	require.NoError(t, s.Stop())
	_, err := s.access.oauth.Bootstrap(t.Context(), httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, endpoint.URL+"/session", nil))
	require.ErrorIs(t, err, oauth.ErrClosed)
	lock, err := atomicfile.Lock(filepath.Join(s.auditConfigDir, "oauth.json.lock"))
	require.NoError(t, err, "final stop must release the authority's exclusive lock regardless of role")
	require.NoError(t, lock.Close())
}

func newSessionBrowser(t *testing.T, endpoint *httptest.Server, key string) *sessionBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := *endpoint.Client()
	client.Jar = jar
	browser := &sessionBrowser{client: &client, endpoint: endpoint.URL}
	status, body := browser.call(t, http.MethodGet, "/zzrouter/v1/session", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var response struct {
		Data oauth.SessionInfo `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &response))
	require.False(t, response.Data.Authenticated)
	browser.csrf = response.Data.CSRF
	require.NotEmpty(t, browser.csrf)
	status, body = browser.call(t, http.MethodPost, "/zzrouter/v1/session", map[string]string{"key": key}, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NotContains(t, string(body), key)
	oldProof := browser.csrf
	require.NoError(t, json.Unmarshal(body, &response))
	require.True(t, response.Data.Authenticated)
	browser.csrf = response.Data.CSRF
	require.NotEmpty(t, browser.csrf)
	require.NotEqual(t, oldProof, browser.csrf)
	origin, err := url.Parse(endpoint.URL)
	require.NoError(t, err)
	for _, cookie := range jar.Cookies(origin) {
		if cookie.Name == browserSessionCookie {
			require.NotContains(t, string(body), cookie.Value)
		}
	}
	return browser
}

func (b *sessionBrowser) call(t *testing.T, method, path string, value any, headers map[string]string) (int, []byte) {
	t.Helper()
	var body []byte
	if value != nil {
		var err error
		body, err = json.Marshal(value)
		require.NoError(t, err)
	}
	r, err := http.NewRequestWithContext(t.Context(), method, b.endpoint+path, bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	if !safeBrowserMethod(method) {
		r.Header.Set("Origin", b.endpoint)
		r.Header.Set("X-CSRF-Token", b.csrf)
	}
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	response, err := b.client.Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, body
}

func TestIntegrationBrowserSessionRolesAndCredentialPrecedence(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	admin := newSessionBrowser(t, endpoint, TestAdminKey)
	status, body := admin.call(t, "GET", "/zzrouter/v1/nodes", nil, nil)
	require.Equal(t, 200, status, string(body))
	for _, headers := range []map[string]string{
		{"X-API-Key": "invalid-explicit-key"},
		{"Authorization": "Bearer"},
		{"Authorization": "Bearer\tinvalid"},
		{"Authorization": ""},
		{"X-API-Key": ""},
	} {
		status, _ := admin.call(t, "GET", "/zzrouter/v1/nodes", nil, headers)
		require.Equal(t, 401, status, headers)
	}
	status, _ = admin.call(t, "GET", "/zzrouter/v1/me", nil, map[string]string{"Authorization": "Basic proxy-proof"})
	require.Equal(t, 200, status, "an unrelated proxy scheme must not suppress the browser session")
	status, _ = admin.call(t, "POST", "/zzrouter/v1/teams", map[string]string{"id": "basic-proxy-team", "name": "Proxy team"}, map[string]string{"Authorization": "Basic proxy-proof", "X-CSRF-Token": ""})
	require.Equal(t, 403, status, "proxy schemes never bypass cookie CSRF protection")
	status, _ = admin.call(t, "GET", "/zzrouter/v1/nodes", nil, map[string]string{"X-API-Key": TestUserKey})
	require.Equal(t, 403, status, "explicit user identity must not borrow the admin cookie")
	for _, path := range []string{adminMCPPath, inferenceMCPPath} {
		status, _ := admin.call(t, "POST", path, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, nil)
		require.Equal(t, 401, status, path)
	}
	s.config.Auth.RequireCompatAuth = true
	status, _ = admin.call(t, "GET", "/v1/models", nil, nil)
	require.Equal(t, 401, status, "cookie cannot satisfy compatibility authentication")
	s.config.Auth.RequireCompatAuth = false
	// An explicitly anonymous compatibility route may ignore ambient cookies.
	// Its existing policy remains authoritative and attaches no browser identity.
	s.engine.GET("/test-anonymous-browser", s.auth.OptionalAuthMiddleware(), func(c *gin.Context) {
		require.Nil(t, GetAccessContext(c))
		c.Status(204)
	})
	status, _ = admin.call(t, "GET", "/test-anonymous-browser", nil, nil)
	require.Equal(t, 204, status)
	user := newSessionBrowser(t, endpoint, TestUserKey)
	status, body = user.call(t, "GET", "/zzrouter/v1/me", nil, nil)
	require.Equal(t, 200, status, string(body))
	require.Contains(t, string(body), `"role":"user"`)
	require.NotContains(t, string(body), "version")
	status, _ = user.call(t, "GET", "/zzrouter/v1/nodes", nil, nil)
	require.Equal(t, 403, status)
}

func TestIntegrationBrowserMutationOwnershipAndLogout(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	admin := newSessionBrowser(t, endpoint, TestAdminKey)
	for _, headers := range []map[string]string{{"Origin": ""}, {"Origin": "null"}, {"Origin": "https://attacker.example"}, {"X-CSRF-Token": "incorrect"}} {
		status, _ := admin.call(t, "POST", "/zzrouter/v1/teams", map[string]string{"id": "browser-team", "name": "Browser team"}, headers)
		require.Equal(t, 403, status, headers)
	}
	status, body := admin.call(t, "POST", "/zzrouter/v1/teams", map[string]string{"id": "browser-team", "name": "Browser team"}, nil)
	require.Equal(t, 201, status, string(body))
	access := browserToken(t, endpoint, TestUserKey, inferenceMCPPath)
	user := newSessionBrowser(t, endpoint, TestUserKey)
	status, body = user.call(t, "GET", "/zzrouter/v1/me/connections", nil, nil)
	require.Equal(t, 200, status)
	var listing struct {
		Data []oauth.Connection `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &listing))
	require.Len(t, listing.Data, 1)
	id := listing.Data[0].ID
	status, _ = admin.call(t, "DELETE", "/zzrouter/v1/me/connections/"+id, nil, nil)
	require.Equal(t, 200, status)
	_, err := s.access.oauth.Authenticate(t.Context(), access, endpoint.URL+inferenceMCPPath)
	require.NoError(t, err, "a foreign owner cannot revoke this grant")
	status, _ = user.call(t, "DELETE", "/zzrouter/v1/me/connections/"+id, nil, nil)
	require.Equal(t, 200, status)
	_, err = s.access.oauth.Authenticate(t.Context(), access, endpoint.URL+inferenceMCPPath)
	require.Error(t, err)
	status, _ = admin.call(t, "DELETE", "/zzrouter/v1/session", nil, map[string]string{"X-CSRF-Token": "incorrect"})
	require.Equal(t, 403, status)
	status, _ = admin.call(t, "GET", "/zzrouter/v1/me", nil, nil)
	require.Equal(t, 200, status)
	status, _ = admin.call(t, "DELETE", "/zzrouter/v1/session", nil, nil)
	require.Equal(t, 200, status)
	status, _ = admin.call(t, "GET", "/zzrouter/v1/me", nil, nil)
	require.Equal(t, 401, status)
}

func TestIntegrationBrowserStaleTabCannotMutateNewIdentity(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	access := browserToken(t, endpoint, TestUserKey, inferenceMCPPath)
	browser := newSessionBrowser(t, endpoint, TestAdminKey)
	staleTab := *browser // Tabs share cookies, but each retains its rendered proof.
	status, body := browser.call(t, http.MethodPost, "/zzrouter/v1/session", map[string]string{"key": TestUserKey}, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var signedIn struct {
		Data oauth.SessionInfo `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &signedIn))
	browser.csrf = signedIn.Data.CSRF
	status, body = browser.call(t, http.MethodGet, "/zzrouter/v1/me", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), `"role":"user"`)
	status, body = browser.call(t, http.MethodGet, "/zzrouter/v1/me/connections", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var connections struct {
		Data []oauth.Connection `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &connections))
	require.Len(t, connections.Data, 1)
	path := "/zzrouter/v1/me/connections/" + connections.Data[0].ID
	status, body = staleTab.call(t, http.MethodDelete, path, nil, nil)
	require.Equal(t, http.StatusForbidden, status, string(body))
	_, err := s.access.oauth.Authenticate(t.Context(), access, endpoint.URL+inferenceMCPPath)
	require.NoError(t, err, "stale administrator tab must not revoke the replacement user's grant")
	status, body = browser.call(t, http.MethodDelete, path, nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	_, err = s.access.oauth.Authenticate(t.Context(), access, endpoint.URL+inferenceMCPPath)
	require.Error(t, err, "the replacement identity's current proof permits its own mutation")
}

func TestIntegrationBrowserSessionRechecksLiveKeyPolicy(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	key := loadTestKey(t, s, "browser-reader", keys.VirtualKey{Name: "Browser reader"})
	browser := newSessionBrowser(t, endpoint, key)
	stored := *s.keyStore.Get("browser-reader")
	stored.Suspended = true
	require.NoError(t, s.keyStore.Update("browser-reader", &stored))
	status, _ := browser.call(t, "GET", "/zzrouter/v1/me", nil, nil)
	require.Equal(t, 401, status)
	stored.Suspended = false
	require.NoError(t, s.keyStore.Update("browser-reader", &stored))
	status, _ = browser.call(t, "GET", "/zzrouter/v1/me", nil, nil)
	require.Equal(t, 200, status)
	_, err := s.keyStore.RotateKey("browser-reader")
	require.NoError(t, err)
	status, _ = browser.call(t, "GET", "/zzrouter/v1/me", nil, nil)
	require.Equal(t, 401, status)
}
