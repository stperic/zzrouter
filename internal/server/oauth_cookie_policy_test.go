package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestIntegrationHTTPSBrowserCookiePolicy(t *testing.T) {
	endpoint := httptest.NewUnstartedServer(nil)
	t.Cleanup(endpoint.Close)
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, ClusterMode: config.ClusterModeDisabled, OAuthIssuer: "https://" + endpoint.Listener.Addr().String()})
	endpoint.Config.Handler = s.engine
	endpoint.StartTLS()
	browser := newSessionBrowser(t, endpoint, TestAdminKey)
	origin, err := url.Parse(endpoint.URL)
	require.NoError(t, err)
	cookies := browser.client.Jar.Cookies(origin)
	require.Len(t, cookies, 2)
	wantNames := map[string]bool{"__Host-zzrouter_oauth_session": true, "__Host-zzrouter_oauth_browser": true}
	for _, cookie := range cookies {
		require.True(t, wantNames[cookie.Name], "HTTPS constructor must select host-prefixed private names")
	}
	checkFlags := func(response *http.Response, count int) {
		t.Helper()
		require.Len(t, response.Cookies(), count)
		for _, cookie := range response.Cookies() {
			require.True(t, wantNames[cookie.Name])
			require.True(t, cookie.Secure)
			require.True(t, cookie.HttpOnly)
			require.Equal(t, "/", cookie.Path)
			require.Empty(t, cookie.Domain)
			require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
		}
	}
	loginBody, err := json.Marshal(map[string]string{"key": TestAdminKey})
	require.NoError(t, err)
	login, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.URL+"/zzrouter/v1/session", strings.NewReader(string(loginBody)))
	require.NoError(t, err)
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", endpoint.URL)
	login.Header.Set("X-CSRF-Token", browser.csrf)
	response, err := browser.client.Do(login)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	checkFlags(response, 2)
	var signedIn struct{ Data oauth.SessionInfo }
	require.NoError(t, json.Unmarshal(body, &signedIn))
	require.True(t, signedIn.Data.Authenticated)
	browser.csrf = signedIn.Data.CSRF
	cookies = browser.client.Jar.Cookies(origin)
	response, err = browser.client.Get(endpoint.URL + "/zzrouter/v1/session")
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	checkFlags(response, 1)
	require.Equal(t, browserHostCSRFCookie, response.Cookies()[0].Name, "ordinary reads must not reissue a session that could overwrite a newer sign-in")

	// The same valid opaque values under old names cannot become HTTPS authority.
	old := httptest.NewRequest(http.MethodGet, endpoint.URL+"/zzrouter/v1/me", nil)
	for _, cookie := range cookies {
		old.AddCookie(&http.Cookie{
			Name: strings.TrimPrefix(cookie.Name, "__Host-"), Value: cookie.Value,
			Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Path: "/",
		})
	}
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, old)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	old.URL.Path = "/zzrouter/v1/session"
	w = httptest.NewRecorder()
	s.engine.ServeHTTP(w, old)
	require.Equal(t, http.StatusOK, w.Code)
	var bootstrap struct{ Data oauth.SessionInfo }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &bootstrap))
	require.False(t, bootstrap.Data.Authenticated)

	for _, duplicate := range cookies {
		r := httptest.NewRequest(http.MethodGet, endpoint.URL+"/zzrouter/v1/me", nil)
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		r.Header.Add("Cookie", duplicate.String())
		w = httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		require.Equal(t, http.StatusUnauthorized, w.Code, duplicate.Name)
		// Explicit native credentials retain precedence over unused cookies.
		r.Header.Set("X-API-Key", TestAdminKey)
		w = httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		require.Equal(t, http.StatusOK, w.Code)
		r.URL.Path = "/zzrouter/v1/session"
		w = httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		require.Equal(t, http.StatusForbidden, w.Code)
	}
	logout, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, endpoint.URL+"/zzrouter/v1/session", nil)
	require.NoError(t, err)
	logout.Header.Set("Origin", endpoint.URL)
	logout.Header.Set("X-CSRF-Token", browser.csrf)
	response, err = browser.client.Do(logout)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	checkFlags(response, 2)
	for _, cookie := range response.Cookies() {
		if cookie.Name == "__Host-zzrouter_oauth_session" {
			require.Equal(t, -1, cookie.MaxAge)
			require.Empty(t, cookie.Value)
		}
	}
}

func TestIntegrationLoopbackBrowserCookiesRemainSeparate(t *testing.T) {
	_, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	browser := newSessionBrowser(t, endpoint, TestAdminKey)
	response, err := browser.client.Get(endpoint.URL + "/zzrouter/v1/session")
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Len(t, response.Cookies(), 2, "ordinary reads retain CSRF cleanup without reissuing the session")
	var paths []string
	for _, cookie := range response.Cookies() {
		require.Equal(t, browserCSRFCookie, cookie.Name)
		require.False(t, cookie.Secure)
		require.True(t, cookie.HttpOnly)
		require.Empty(t, cookie.Domain)
		require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
		paths = append(paths, cookie.Path)
		if cookie.Path == "/oauth" {
			require.Equal(t, -1, cookie.MaxAge)
			require.Empty(t, cookie.Value)
		} else {
			require.NotEmpty(t, cookie.Value)
		}
	}
	require.ElementsMatch(t, []string{"/", "/oauth"}, paths)
}
