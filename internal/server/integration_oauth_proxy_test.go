package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func remoteProxyNode(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	public := httptest.NewUnstartedServer(nil)
	t.Cleanup(public.Close)
	issuer := "https://" + public.Listener.Addr().String()
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, ClusterMode: config.ClusterModeDisabled, OAuthIssuer: issuer})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			t.Error("Router origin must receive plaintext behind TLS termination")
		}
		s.engine.ServeHTTP(w, r)
	}))
	t.Cleanup(origin.Close)
	target, err := url.Parse(origin.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	public.Config.Handler = proxy
	public.Config.ReadHeaderTimeout = 10 * time.Second
	public.Config.WriteTimeout = 10 * time.Second
	public.StartTLS()
	public.Client().Timeout = 10 * time.Second
	require.Equal(t, issuer, public.URL)
	require.NotEqual(t, public.Listener.Addr(), origin.Listener.Addr())
	return public, origin.URL
}

type remoteProxyReply struct {
	status  int
	header  http.Header
	cookies []*http.Cookie
}

func remoteProxyResponse(t *testing.T, client *http.Client, request *http.Request) (remoteProxyReply, []byte) {
	t.Helper()
	response, err := client.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	require.NoError(t, err)
	require.NoError(t, closeErr)
	return remoteProxyReply{status: response.StatusCode, header: response.Header, cookies: response.Cookies()}, body
}

func TestIntegrationRemoteProxyDiscovery(t *testing.T) {
	public, origin := remoteProxyNode(t)
	for _, tc := range []struct{ path, scope string }{
		{adminMCPPath, "zzrouter:manage"}, {inferenceMCPPath, "zzrouter:inference"},
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, public.URL+tc.path, nil)
		require.NoError(t, err)
		response, _ := remoteProxyResponse(t, public.Client(), request)
		require.Equal(t, http.StatusUnauthorized, response.status)
		challenge := response.header.Get("WWW-Authenticate")
		require.Equal(t, `Bearer resource_metadata="`+public.URL+"/.well-known/oauth-protected-resource"+tc.path+`", scope="`+tc.scope+`"`, challenge)
		match := regexp.MustCompile(`resource_metadata="([^"]+)"`).FindStringSubmatch(challenge)
		require.Len(t, match, 2)
		request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, match[1], nil)
		require.NoError(t, err)
		response, body := remoteProxyResponse(t, public.Client(), request)
		require.Equal(t, http.StatusOK, response.status)
		require.Contains(t, response.header.Get("Content-Type"), "application/json")
		var resource struct {
			Resource string   `json:"resource"`
			Issuers  []string `json:"authorization_servers"`
			Scopes   []string `json:"scopes_supported"`
		}
		require.NoError(t, json.Unmarshal(body, &resource))
		require.Equal(t, public.URL+tc.path, resource.Resource)
		require.Equal(t, []string{public.URL}, resource.Issuers)
		require.Equal(t, []string{tc.scope}, resource.Scopes)
		require.NotContains(t, string(body), origin)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, public.URL+"/.well-known/oauth-authorization-server", nil)
	require.NoError(t, err)
	request.Host = "misleading.example:444"
	request.Header.Set("X-Forwarded-Host", "misleading.example:444")
	request.Header.Set("X-Forwarded-Proto", "http")
	response, body := remoteProxyResponse(t, public.Client(), request)
	require.Equal(t, http.StatusOK, response.status)
	require.Contains(t, response.header.Get("Content-Type"), "application/json")
	var metadata struct {
		Issuer        string `json:"issuer"`
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
		Registration  string `json:"registration_endpoint"`
		Revocation    string `json:"revocation_endpoint"`
	}
	require.NoError(t, json.Unmarshal(body, &metadata))
	require.Equal(t, public.URL, metadata.Issuer)
	require.Equal(t, public.URL+"/oauth/authorize", metadata.Authorization)
	require.Equal(t, public.URL+"/oauth/token", metadata.Token)
	require.Equal(t, public.URL+"/oauth/register", metadata.Registration)
	require.Equal(t, public.URL+"/oauth/revoke", metadata.Revocation)
	require.NotContains(t, string(body), origin)
	require.NotContains(t, string(body), "misleading.example")
	for _, path := range []string{"/console/", "/zzrouter/v1/session"} {
		request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, public.URL+path, nil)
		require.NoError(t, err)
		response, body = remoteProxyResponse(t, public.Client(), request)
		require.Equal(t, http.StatusOK, response.status, path)
		require.Equal(t, "no-store", response.header.Get("Cache-Control"))
		if path == "/console/" {
			require.Contains(t, string(body), "<!doctype html>")
		} else {
			var session struct{ Data browserSessionState }
			require.NoError(t, json.Unmarshal(body, &session))
			require.Equal(t, public.URL, session.Data.Issuer)
			require.False(t, session.Data.Authenticated)
			require.NotEmpty(t, session.Data.CSRF)
		}
	}
}

func TestIntegrationRemoteProxyOAuth(t *testing.T) {
	for _, tc := range []struct{ name, key, path, scope, otherPath, tool string }{
		{"management", TestAdminKey, adminMCPPath, "zzrouter:manage", inferenceMCPPath, "zz_nodes"},
		{"inference", TestUserKey, inferenceMCPPath, "zzrouter:inference", adminMCPPath, "zzrouter_inference_models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			public, origin := remoteProxyNode(t)
			clientID, access, refresh := browserOAuthTokensWithConsent(t, public, tc.key, tc.path, func(page []byte) {
				for _, disclosure := range []string{"Client name (unverified):", "Integration client", "http://127.0.0.1:44552/callback", public.URL + tc.path} {
					require.True(t, strings.Contains(string(page), disclosure), "consent must disclose %s", disclosure)
				}
				require.False(t, strings.Contains(string(page), origin), "consent must not disclose the private origin")
			})
			callTool := func(token string) {
				t.Helper()
				session := oauthMCPSession(t, public, tc.path, token)
				result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.tool, Arguments: map[string]any{}})
				require.NoError(t, err)
				require.False(t, result.IsError)
			}
			callTool(access)
			callTool(tc.key)
			for _, path := range []string{tc.otherPath, "/zzrouter/v1/nodes", "/v1/models"} {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, public.URL+path, nil)
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer "+access)
				response, _ := remoteProxyResponse(t, public.Client(), request)
				require.Equal(t, http.StatusUnauthorized, response.status, path)
			}
			wire := func(token, requestOrigin string, status int) {
				t.Helper()
				request := mcpWireRequest(t, "tools/call", "2026-07-28", map[string]any{"name": tc.tool, "arguments": map[string]any{}})
				var err error
				request.URL, err = url.Parse(public.URL + tc.path)
				require.NoError(t, err)
				request.Host, request.RequestURI = request.URL.Host, ""
				request = request.WithContext(t.Context())
				request.Header.Del("X-API-Key")
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Origin", requestOrigin)
				response, body := remoteProxyResponse(t, public.Client(), request)
				require.Equal(t, status, response.status, "modern MCP through TLS termination")
				if status == http.StatusOK {
					var message struct {
						Result struct {
							Type    string `json:"resultType"`
							IsError bool   `json:"isError"`
						} `json:"result"`
					}
					require.NoError(t, json.Unmarshal(body, &message))
					require.Equal(t, "complete", message.Result.Type)
					require.False(t, message.Result.IsError)
				}
			}
			wire(access, public.URL, http.StatusOK)
			wire(access, "https://hostile.example", http.StatusForbidden)
			form := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refresh}, "scope": {tc.scope + " offline_access"}}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, public.URL+"/oauth/token", strings.NewReader(form.Encode()))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response, body := remoteProxyResponse(t, public.Client(), request)
			require.Equal(t, http.StatusOK, response.status)
			var renewed struct {
				Access  string `json:"access_token"`
				Refresh string `json:"refresh_token"`
				Scope   string `json:"scope"`
			}
			require.NoError(t, json.Unmarshal(body, &renewed))
			require.NotEmpty(t, renewed.Access)
			require.NotEmpty(t, renewed.Refresh)
			require.False(t, refresh == renewed.Refresh, "refresh token must rotate")
			require.Equal(t, tc.scope, renewed.Scope)
			callTool(renewed.Access)

			browser := newSessionBrowser(t, public, tc.key)
			status, body := browser.call(t, http.MethodGet, "/zzrouter/v1/me", nil, map[string]string{"Origin": public.URL})
			require.Equal(t, http.StatusOK, status)
			require.NotContains(t, string(body), tc.key)
			login, err := json.Marshal(map[string]string{"key": tc.key})
			require.NoError(t, err)
			request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, public.URL+"/zzrouter/v1/session", strings.NewReader(string(login)))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", public.URL)
			request.Header.Set("X-CSRF-Token", browser.csrf)
			response, body = remoteProxyResponse(t, browser.client, request)
			require.Equal(t, http.StatusOK, response.status)
			var state struct{ Data browserSessionState }
			require.NoError(t, json.Unmarshal(body, &state))
			require.Equal(t, public.URL, state.Data.Issuer)
			require.True(t, state.Data.Authenticated)
			browser.csrf = state.Data.CSRF
			require.Len(t, response.cookies, 2)
			var names []string
			for _, cookie := range response.cookies {
				names = append(names, cookie.Name)
				require.True(t, cookie.Secure)
				require.True(t, cookie.HttpOnly)
				require.Equal(t, "/", cookie.Path)
				require.Empty(t, cookie.Domain)
				require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
			}
			require.ElementsMatch(t, []string{browserHostSessionCookie, browserHostCSRFCookie}, names)
			status, body = browser.call(t, http.MethodGet, "/zzrouter/v1/me/connections", nil, nil)
			require.Equal(t, http.StatusOK, status)
			var listing struct{ Data []oauth.Connection }
			require.NoError(t, json.Unmarshal(body, &listing))
			require.Len(t, listing.Data, 1)
			require.Equal(t, public.URL+tc.path, listing.Data[0].Resource)
			require.Equal(t, tc.scope, listing.Data[0].Scope)
			connectionPath := "/zzrouter/v1/me/connections/" + listing.Data[0].ID
			for _, headers := range []map[string]string{
				{"Origin": "https://hostile.example"}, {"X-CSRF-Token": "incorrect"},
			} {
				status, _ = browser.call(t, http.MethodDelete, connectionPath, nil, headers)
				require.Equal(t, http.StatusForbidden, status)
			}
			for _, omitted := range []string{"Origin", "X-CSRF-Token"} {
				request, err = http.NewRequestWithContext(t.Context(), http.MethodDelete, public.URL+connectionPath, nil)
				require.NoError(t, err)
				request.Header.Set("Origin", public.URL)
				request.Header.Set("X-CSRF-Token", browser.csrf)
				request.Header.Del(omitted)
				response, _ = remoteProxyResponse(t, browser.client, request)
				require.Equal(t, http.StatusForbidden, response.status, "missing %s must refuse cookie mutation", omitted)
			}
			status, _ = browser.call(t, http.MethodPost, tc.path, nil, nil)
			require.Equal(t, http.StatusUnauthorized, status, "cookies alone cannot authorize MCP")
			status, _ = browser.call(t, http.MethodDelete, "/zzrouter/v1/session", nil, nil)
			require.Equal(t, http.StatusOK, status)
			wire(renewed.Access, public.URL, http.StatusOK)
			browser = newSessionBrowser(t, public, tc.key)
			status, _ = browser.call(t, http.MethodDelete, connectionPath, nil, nil)
			require.Equal(t, http.StatusOK, status)
			wire(renewed.Access, public.URL, http.StatusUnauthorized)
			form.Set("refresh_token", renewed.Refresh)
			request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, public.URL+"/oauth/token", strings.NewReader(form.Encode()))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response, _ = remoteProxyResponse(t, public.Client(), request)
			require.Equal(t, http.StatusBadRequest, response.status)
		})
	}
}
