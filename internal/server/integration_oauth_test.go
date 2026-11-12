package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func oauthTestNode(t *testing.T, mode config.ClusterMode) (*Server, *httptest.Server) {
	t.Helper()
	endpoint := httptest.NewUnstartedServer(nil)
	t.Cleanup(endpoint.Close)
	issuer := "http://" + endpoint.Listener.Addr().String()
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, ClusterMode: mode, OAuthIssuer: issuer})
	endpoint.Config.Handler = s.engine
	endpoint.Start()
	return s, endpoint
}
func browserToken(t *testing.T, endpoint *httptest.Server, key, path string) string {
	t.Helper()
	_, access, _ := browserOAuthTokens(t, endpoint, key, path)
	return access
}

func browserOAuthTokens(t *testing.T, endpoint *httptest.Server, key, path string) (string, string, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	browser := *endpoint.Client()
	browser.Jar = jar
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := browser.Post(endpoint.URL+"/oauth/register", "application/json", strings.NewReader(`{"client_name":"Integration client","redirect_uris":["http://127.0.0.1/callback"]}`))
	require.NoError(t, err)
	var registration struct {
		ID string `json:"client_id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&registration))
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 201, resp.StatusCode)
	verifier := strings.Repeat("c", 64)
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {registration.ID}, "redirect_uri": {"http://127.0.0.1:44552/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "resource": {endpoint.URL + path}}
	resp, err = browser.Get(endpoint.URL + "/oauth/authorize?" + q.Encode())
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 200, resp.StatusCode, string(data))
	field := func(name string) string {
		match := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindSubmatch(data)
		require.Len(t, match, 2)
		return string(match[1])
	}
	form := url.Values{"key": {key}, "csrf": {field("csrf")}, "pending": {field("pending")}, "action": {"allow"}}
	req, err := http.NewRequestWithContext(t.Context(), "POST", endpoint.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", endpoint.URL)
	resp, err = browser.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 303, resp.StatusCode)
	target, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	form = url.Values{"client_id": {registration.ID}, "redirect_uri": {q.Get("redirect_uri")}, "grant_type": {"authorization_code"}, "resource": {q.Get("resource")}, "code": {target.Query().Get("code")}, "code_verifier": {verifier}}
	resp, err = browser.PostForm(endpoint.URL+"/oauth/token", form)
	require.NoError(t, err)
	var tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&tokens))
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 200, resp.StatusCode)
	require.NotEmpty(t, tokens.Access)
	require.NotEmpty(t, tokens.Refresh)
	return registration.ID, tokens.Access, tokens.Refresh
}
func oauthMCPSession(t *testing.T, endpoint *httptest.Server, path, token string) *mcp.ClientSession {
	t.Helper()
	httpClient := *endpoint.Client()
	base := httpClient.Transport
	httpClient.Transport = mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("Authorization", "Bearer "+token)
		return base.RoundTrip(r)
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "oauth-integration", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: endpoint.URL + path, HTTPClient: &httpClient}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}
func TestIntegrationOAuthAdminDispatchAndAudience(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	missing := makeAuthRequest(t, s, "POST", "/zzrouter/v1/mcp", "", nil)
	require.Equal(t, 401, missing.Code)
	require.Contains(t, missing.Headers.Get("WWW-Authenticate"), "oauth-protected-resource/zzrouter/v1/mcp")
	access := browserToken(t, endpoint, TestAdminKey, "/zzrouter/v1/mcp")
	session := oauthMCPSession(t, endpoint, "/zzrouter/v1/mcp", access)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "zz_nodes", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result)
	for _, path := range []string{"/zzrouter/v1/nodes", "/inference/mcp"} {
		result := makeRequest(t, s, TestRequest{Method: "GET", Path: path, Headers: map[string]string{"Authorization": "Bearer " + access}})
		require.Equal(t, 401, result.Code, path)
	}
	wrongScheme := makeAuthRequest(t, s, "POST", "/zzrouter/v1/mcp", access, nil)
	require.Equal(t, 401, wrongScheme.Code)
}
func TestIntegrationOAuthUserPreservesPolicyAndScrubsToken(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			calls.Add(1)
		}
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("X-API-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat","choices":[{"message":{"role":"assistant","content":"ready"}}]}`))
	}))
	t.Cleanup(backend.Close)
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	s.configStore = config.NewMemoryAppsConfigStore(&config.AppsConfig{})
	require.NoError(t, s.appsConfig().AddApp("mcp-provider", config.ServiceConfig{Enabled: new(true), Mode: "external", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: backend.URL, API: &config.APIConfig{AuthType: config.AuthTypeCaller}}, Capabilities: &config.AppCapabilities{Models: []string{"allowed-model", "blocked-model"}, WireEndpoints: []string{"chat_completions"}}}))
	s.model.Cache.Invalidate()
	s.model.Resolver = staticModelResolver{provider: "mcp-provider"}
	team := makeAuthRequest(t, s, "POST", "/zzrouter/v1/teams", TestAdminKey, map[string]any{"id": "oauth-team", "name": "OAuth team", "allowed_models": []string{"allowed-model"}})
	require.Equal(t, 201, team.Code, string(team.Body))
	key := loadTestKey(t, s, "oauth-reader", keys.VirtualKey{Name: "OAuth reader", TeamID: "oauth-team", QuotaConfig: quota.QuotaConfig{RPMLimit: 1}})
	access := browserToken(t, endpoint, key, "/inference/mcp")
	session := oauthMCPSession(t, endpoint, "/inference/mcp", access)
	chat := func(model string) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "zzrouter_chat", Arguments: map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hello"}}}})
		require.NoError(t, err)
		return result
	}
	blocked := chat("blocked-model")
	require.True(t, blocked.IsError)
	require.Contains(t, blocked.Content[0].(*mcp.TextContent).Text, "HTTP 403")
	allowed := chat("allowed-model")
	require.False(t, allowed.IsError, "%+v", allowed)
	limited := chat("allowed-model")
	require.True(t, limited.IsError)
	require.Contains(t, limited.Content[0].(*mcp.TextContent).Text, "HTTP 429")
	require.EqualValues(t, 1, calls.Load())
	_, err := s.keyStore.RotateKey("oauth-reader")
	require.NoError(t, err)
	after := makeRequest(t, s, TestRequest{Method: "POST", Path: "/inference/mcp", Headers: map[string]string{"Authorization": "Bearer " + access}, Body: map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}})
	require.Equal(t, 401, after.Code)
}
func TestIntegrationOAuthWorkerHasNoLoginSurface(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeWorker)
	for _, path := range []string{"/oauth/authorize", "/oauth/connections", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/inference/mcp"} {
		r := makeAuthRequest(t, s, "GET", path, TestAdminKey, nil)
		require.Equal(t, 404, r.Code, path)
	}
	for _, path := range []string{"/zzrouter/v1/session", "/zzrouter/v1/me"} {
		r := makeAuthRequest(t, s, "GET", path, TestAdminKey, nil)
		// The existing worker management guard sends callers to the coordinator
		// before route lookup. No session handler is mounted on this worker.
		require.Equal(t, http.StatusMisdirectedRequest, r.Code, path)
		for _, route := range s.engine.Routes() {
			require.NotEqual(t, path, route.Path)
		}
	}
}

func TestIntegrationOAuthDefaultLocalhost(t *testing.T) {
	endpoint := httptest.NewUnstartedServer(nil)
	t.Cleanup(endpoint.Close)
	_, portText, err := net.SplitHostPort(endpoint.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, Port: port})
	endpoint.Config.Handler = s.engine
	endpoint.Start()
	endpoint.URL = "http://localhost:" + portText
	require.Empty(t, s.config.Auth.OAuth.IssuerURL, "resolving a default must not mutate persisted config")
	require.Equal(t, endpoint.URL, s.access.oauth.Issuer())
	metadata := makeRequest(t, s, TestRequest{Method: "GET", Path: "/.well-known/oauth-authorization-server", Headers: map[string]string{"Host": "evil.example", "X-Forwarded-Host": "evil.example"}})
	require.Equal(t, 200, metadata.Code)
	require.Contains(t, string(metadata.Body), endpoint.URL)
	require.NotContains(t, string(metadata.Body), "evil.example")
	page := makeAuthRequest(t, s, "GET", "/oauth/connections", "", nil)
	require.Equal(t, 200, page.Code)
	access := browserToken(t, endpoint, TestAdminKey, "/zzrouter/v1/mcp")
	session := oauthMCPSession(t, endpoint, "/zzrouter/v1/mcp", access)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "zz_nodes", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	userAccess := browserToken(t, endpoint, TestUserKey, "/inference/mcp")
	userSession := oauthMCPSession(t, endpoint, "/inference/mcp", userAccess)
	tools, err := userSession.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, tools.Tools, 3)
}

func TestIntegrationOAuthOptOutKeepsMCP(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, OAuthEnabled: new(false)})
	for _, path := range []string{"/oauth/connections", "/zzrouter/v1/session", "/zzrouter/v1/me", "/.well-known/oauth-authorization-server"} {
		resp := makeAuthRequest(t, s, "GET", path, "", nil)
		require.Equal(t, 404, resp.Code)
	}
	endpoint := httptest.NewServer(s.engine)
	t.Cleanup(endpoint.Close)
	session := oauthMCPSession(t, endpoint, "/zzrouter/v1/mcp", TestAdminKey)
	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, tools.Tools)
}

func TestIntegrationOAuthLikeNativeKey(t *testing.T) {
	const key = "zzoa_B9vQ4x7pD2mR8tK3nH6wS1cF5jL0zA9uE4yG7dN2"
	for _, enabled := range []bool{true, false} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			s := createTestNode(t, TestNodeConfig{AdminKey: key, OAuthEnabled: new(enabled)})
			response := makeRequest(t, s, TestRequest{Method: "GET", Path: "/zzrouter/v1/nodes", Headers: map[string]string{"Authorization": "Bearer " + key}})
			require.Equal(t, 200, response.Code, string(response.Body))
			endpoint := httptest.NewServer(s.engine)
			t.Cleanup(endpoint.Close)
			session := oauthMCPSession(t, endpoint, "/zzrouter/v1/mcp", key)
			tools, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
			require.NotEmpty(t, tools.Tools)
		})
	}
}
