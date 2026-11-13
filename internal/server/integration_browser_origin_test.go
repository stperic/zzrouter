package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestIntegrationBrowserCookieReadsStayOnIssuerWithCredentialedCORS(t *testing.T) {
	const foreignOrigin = "http://127.0.0.1:3000"
	endpoint := httptest.NewUnstartedServer(nil)
	t.Cleanup(endpoint.Close)
	issuer := "http://" + endpoint.Listener.Addr().String()
	require.NotEqual(t, foreignOrigin, issuer)
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, OAuthIssuer: issuer})
	outer := gin.New()
	outer.Use(CORSMiddleware(config.CORSConfig{Enabled: true, AllowedOrigins: []string{foreignOrigin}, AllowCredentials: true}))
	outer.NoRoute(gin.WrapH(s.engine))
	endpoint.Config.Handler = outer
	endpoint.Start()
	browser := newSessionBrowser(t, endpoint, TestAdminKey)

	// Registration is a public OAuth protocol endpoint, not browser authority.
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.URL+"/oauth/register", strings.NewReader(`{"client_name":"Origin fixture","redirect_uris":["http://127.0.0.1/callback"]}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", foreignOrigin)
	response, err := browser.client.Do(request)
	require.NoError(t, err)
	var registered struct {
		ID string `json:"client_id"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&registered))
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusCreated, response.StatusCode)
	query := url.Values{"client_id": {registered.ID}, "redirect_uri": {"http://127.0.0.1/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {strings.Repeat("c", 43)}, "resource": {issuer + adminMCPPath}}
	paths := []string{"/zzrouter/v1/keys", "/zzrouter/v1/me", "/zzrouter/v1/session", "/oauth/connections", "/oauth/authorize?" + query.Encode()}
	read := func(t *testing.T, path string, origins []string, key string) (int, http.Header, string) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint.URL+path, nil)
		require.NoError(t, err)
		if origins != nil {
			request.Header["Origin"] = origins
		}
		// This metadata cannot override an explicit foreign or malformed Origin.
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		if key != "" {
			request.Header.Set("X-API-Key", key)
		}
		response, err := browser.client.Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, response.Header, string(body)
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			for _, origins := range [][]string{{foreignOrigin}, {"null"}, {""}, {issuer, foreignOrigin}} {
				status, headers, body := read(t, path, origins, "")
				require.Equal(t, http.StatusForbidden, status, "origins=%v body=%s", origins, body)
				require.Empty(t, headers.Values("Set-Cookie"), "foreign reads must not rotate browser proofs")
				require.NotContains(t, body, browser.csrf)
				require.NotContains(t, body, "Admin Key")
				if origins[0] == foreignOrigin {
					require.Equal(t, foreignOrigin, headers.Get("Access-Control-Allow-Origin"))
					require.Equal(t, "true", headers.Get("Access-Control-Allow-Credentials"))
				}
			}
			for _, origins := range [][]string{nil, {issuer}} {
				status, _, body := read(t, path, origins, "")
				require.Equal(t, http.StatusOK, status, body)
			}
		})
	}
	for _, path := range []string{"/zzrouter/v1/keys", "/zzrouter/v1/me"} {
		status, _, body := read(t, path, []string{foreignOrigin}, TestAdminKey)
		require.Equal(t, http.StatusOK, status, body)
	}
	status, _, body := read(t, "/.well-known/oauth-authorization-server", []string{foreignOrigin}, "")
	require.Equal(t, http.StatusOK, status, body)
}
