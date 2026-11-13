package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

// The actual constructor, OAuth routes and HTML renderer serve this fixture.
// The separate HTTPS gateway origin is simulated on local test requests.
func sharedBrowserAuthorityFixture(t *testing.T, lostGrant string, settings ...TestNodeConfig) (*Server, *providerBrowserGateway, *oauth.Server[control.CredentialReference], *httptest.Server, string) {
	t.Helper()
	endpoint := httptest.NewUnstartedServer(nil)
	t.Cleanup(endpoint.Close)
	issuer := "http://" + endpoint.Listener.Addr().String()
	const origin = "https://comfy.example.com"
	var cfg TestNodeConfig
	if len(settings) > 0 {
		cfg = settings[0]
	}
	cfg.AdminKey, cfg.UserKey, cfg.OAuthIssuer = TestAdminKey, TestUserKey, issuer
	cfg.ProviderBrowser = config.ProviderBrowserConfig{Origin: origin}
	s := createTestNode(t, cfg)
	authority := s.access.oauth
	require.NotNil(t, authority)
	require.NotNil(t, s.providerBrowser.authority)
	state := filepath.Join(s.auditConfigDir, "oauth.json")
	endpoint.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		if lostGrant != "" && r.URL.Path == "/oauth/token" && r.ParseForm() == nil && r.PostForm.Get("grant_type") == lostGrant {
			result := httptest.NewRecorder()
			s.engine.ServeHTTP(result, r)
			if result.Code != http.StatusOK {
				t.Errorf("authority must commit before losing response: status %d", result.Code)
				http.Error(w, "Fixture exchange failed", http.StatusInternalServerError)
				return
			}
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		s.engine.ServeHTTP(w, r)
	})
	endpoint.Start()
	gateway, err := newProviderBrowserGateway(t.Context(), s, origin, issuer, s.providerBrowser.authority, endpoint.Client())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, gateway.close(ctx))
	})
	return s, gateway, authority, endpoint, state
}

func approveSharedBrowser(t *testing.T, endpoint *httptest.Server, location *url.URL, key string) *url.URL {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	browser := *endpoint.Client()
	browser.Jar = jar
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := browser.Get(location.String())
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	field := func(name string) string {
		match := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindSubmatch(body)
		require.Len(t, match, 2)
		return string(match[1])
	}
	form := url.Values{"pending": {field("pending")}, "csrf": {field("csrf")}, "action": {"allow"}, "key": {key}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", endpoint.URL)
	response, err = browser.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	callback, err := url.Parse(response.Header.Get("Location"))
	require.NoError(t, err)
	return callback
}

func completeSharedBrowser(t *testing.T, g *providerBrowserGateway, endpoint *httptest.Server, key string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	location, proof := beginBrowserAuthorizationAt(t, g, "/runs/"+g.server.GetNodename()+"/run-a/")
	callback := approveSharedBrowser(t, endpoint, location, key)
	request := httptest.NewRequest(http.MethodGet, callback.String(), nil)
	request.AddCookie(proof)
	result := httptest.NewRecorder()
	g.ServeHTTP(result, request)
	return result, location.Query().Get("client_id")
}

func requireSharedBrowserRetired(t *testing.T, path, clientID string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state struct {
		Clients map[string]json.RawMessage `json:"clients"`
		Grants  map[string]json.RawMessage `json:"grants"`
		Tokens  map[string]json.RawMessage `json:"tokens"`
	}
	require.NoError(t, json.Unmarshal(data, &state))
	require.NotContains(t, state.Clients, clientID)
	require.Empty(t, state.Grants)
	require.Empty(t, state.Tokens)
}

func TestProviderBrowserSharedAuthorityLiveKeyRevocation(t *testing.T) {
	s, g, authority, endpoint, state := sharedBrowserAuthorityFixture(t, "")
	key := loadTestKey(t, s, "provider-browser-reader", keys.VirtualKey{Name: "Provider browser reader"})
	response, clientID := completeSharedBrowser(t, g, endpoint, key)
	require.Equal(t, http.StatusSeeOther, response.Code, response.Body.String())
	request := httptest.NewRequest(http.MethodGet, g.origin.String()+"/runs/"+s.GetNodename()+"/run-a/prompt", nil)
	request.AddCookie(response.Result().Cookies()[0])
	session := g.session(request)
	require.NotNil(t, session)
	ac, err := g.authenticate(session)
	require.NoError(t, err)
	require.Equal(t, "provider-browser-reader", ac.Key.ID)
	require.True(t, ac.Key.IsVirtual)
	token, err := session.tokens.Token()
	require.NoError(t, err)
	for _, path := range []string{adminMCPPath, inferenceMCPPath} {
		_, err = authority.Authenticate(t.Context(), token.AccessToken, endpoint.URL+path)
		require.Error(t, err, "a provider token cannot become an MCP credential")
		result := makeRequest(t, s, TestRequest{Method: http.MethodPost, Path: path, Headers: map[string]string{"Authorization": "Bearer " + token.AccessToken}, Body: map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}})
		require.Equal(t, http.StatusUnauthorized, result.Code)
	}
	require.NotContains(t, response.Body.String()+response.Header().Get("Location"), token.AccessToken)
	require.NotContains(t, response.Body.String()+response.Header().Get("Location"), token.RefreshToken)
	_, err = s.keyStore.RotateKey("provider-browser-reader")
	require.NoError(t, err)
	result := httptest.NewRecorder()
	g.ServeHTTP(result, request)
	require.Equal(t, http.StatusUnauthorized, result.Code)
	require.ErrorIs(t, session.ctx.Err(), context.Canceled)
	requireSharedBrowserRetired(t, state, clientID)
}

func TestProviderBrowserAuthorityWiringAndRoleBoundaries(t *testing.T) {
	s, g, _, endpoint, _ := sharedBrowserAuthorityFixture(t, "")
	response, err := endpoint.Client().Get(endpoint.URL + "/.well-known/oauth-protected-resource" + providerBrowserResource)
	require.NoError(t, err)
	var metadata struct {
		Resource string `json:"resource"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&metadata))
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, endpoint.URL+providerBrowserResource, metadata.Resource)
	for _, test := range []struct {
		key  string
		role control.UserRole
	}{{TestAdminKey, control.RoleAdmin}, {TestUserKey, control.RoleUser}} {
		result, _ := completeSharedBrowser(t, g, endpoint, test.key)
		require.Equal(t, http.StatusSeeOther, result.Code)
		request := httptest.NewRequest(http.MethodGet, g.origin.String()+"/runs/"+s.GetNodename()+"/run-a/", nil)
		request.AddCookie(result.Result().Cookies()[0])
		ac, err := g.authenticate(g.session(request))
		require.NoError(t, err)
		require.Equal(t, test.role, ac.Key.Role)
	}
}

func TestProviderBrowserAuthorityDisabledAndWorkerIsolation(t *testing.T) {
	for _, worker := range []bool{false, true} {
		cfg := TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey}
		if worker {
			cfg.ClusterMode = config.ClusterModeWorker
			cfg.ProviderBrowser.Origin = "https://comfy.example.com"
		}
		s := createTestNode(t, cfg)
		require.Nil(t, s.providerBrowser.authority)
		result := httptest.NewRecorder()
		s.engine.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource"+providerBrowserResource, nil))
		require.Equal(t, http.StatusNotFound, result.Code)
		if worker {
			require.Nil(t, s.access.oauth)
		} else {
			_, err := s.access.oauth.CreateOwnedClient(t.Context(), providerBrowserOwner)
			require.ErrorIs(t, err, oauth.ErrOwnedClient)
		}
	}
}

func TestProviderBrowserSharedAuthorityLostCommittedTokenResponse(t *testing.T) {
	for _, grant := range []string{"authorization_code", "refresh_token"} {
		t.Run(grant, func(t *testing.T) {
			_, g, _, endpoint, state := sharedBrowserAuthorityFixture(t, grant)
			response, clientID := completeSharedBrowser(t, g, endpoint, TestUserKey)
			if grant == "refresh_token" {
				require.Equal(t, http.StatusSeeOther, response.Code)
				request := httptest.NewRequest(http.MethodGet, g.origin.String()+"/runs/"+g.server.GetNodename()+"/run-a/prompt", nil)
				request.AddCookie(response.Result().Cookies()[0])
				session := g.session(request)
				require.NotNil(t, session)
				session.mu.Lock()
				token, err := session.tokens.Token()
				if err == nil {
					// Expire only the consumer's cache; the real refresh family remains live.
					expired := *token
					expired.Expiry = time.Unix(1, 0)
					configuration := g.oauth(session.clientID)
					session.tokens = configuration.TokenSource(session.ctx, &expired)
				}
				session.mu.Unlock()
				require.NoError(t, err)
				response = httptest.NewRecorder()
				g.ServeHTTP(response, request)
				require.ErrorIs(t, session.ctx.Err(), context.Canceled)
			} else {
				require.Empty(t, response.Result().Cookies())
			}
			require.Equal(t, http.StatusUnauthorized, response.Code)
			require.Empty(t, g.states)
			requireSharedBrowserRetired(t, state, clientID)
		})
	}
}

func TestProviderBrowserSharedAuthorityLiveTeamPolicy(t *testing.T) {
	for _, policy := range []string{"suspended", "models"} {
		t.Run(policy, func(t *testing.T) {
			s, g, _, endpoint, state := sharedBrowserAuthorityFixture(t, "")
			key := loadTestKey(t, s, "provider-browser-team-reader", keys.VirtualKey{Name: "Provider team reader"})
			response, clientID := completeSharedBrowser(t, g, endpoint, key)
			require.Equal(t, http.StatusSeeOther, response.Code)
			request := httptest.NewRequest(http.MethodGet, g.origin.String()+"/runs/"+s.GetNodename()+"/run-a/prompt", nil)
			request.AddCookie(response.Result().Cookies()[0])
			session := g.session(request)
			require.NotNil(t, session)
			ac, err := g.authenticate(session)
			require.NoError(t, err)
			require.NotNil(t, ac.Team)
			team := *s.teamStore.Get(ac.Team.ID)
			if policy == "suspended" {
				team.Suspended = true
			} else {
				team.AllowedModels = []string{"approved-model"}
			}
			require.NoError(t, s.teamStore.Update(team.ID, &team))
			result := httptest.NewRecorder()
			g.ServeHTTP(result, request)
			require.Equal(t, http.StatusUnauthorized, result.Code)
			require.ErrorIs(t, session.ctx.Err(), context.Canceled)
			requireSharedBrowserRetired(t, state, clientID)
		})
	}
}
