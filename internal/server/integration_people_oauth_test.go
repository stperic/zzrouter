package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestIntegrationPeopleOAuthContinuesWithFreshConsent(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	_, owner := createBrowserOwner(t, s, endpoint)
	browser := anonymousPeopleBrowser(t, endpoint)
	browser.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := browser.client.Post(endpoint.URL+"/oauth/register", "application/json", strings.NewReader(`{"client_name":"Fixture editor","redirect_uris":["http://127.0.0.1/callback"]}`))
	require.NoError(t, err)
	var registration struct {
		ID string `json:"client_id"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&registration))
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusCreated, response.StatusCode)
	verifier := strings.Repeat("v", 64)
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {registration.ID}, "redirect_uri": {"http://127.0.0.1:44552/callback"}, "response_type": {"code"}, "resource": {endpoint.URL + adminMCPPath}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}, "state": {"fixture &<> state"}, "unrecognized": {"discard this"}}
	readPage := func(path string) string {
		t.Helper()
		response, err := browser.client.Get(endpoint.URL + path)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode, string(data))
		return string(data)
	}
	field := func(page, name string) string {
		t.Helper()
		match := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindStringSubmatch(page)
		require.Len(t, match, 2)
		return html.UnescapeString(match[1])
	}
	page := readPage("/oauth/authorize?" + query.Encode())
	require.Contains(t, page, "Sign in to Router")
	require.Contains(t, page, "Advanced access")
	require.Contains(t, page, `name="key"`, "API-key compatibility stays available")
	match := regexp.MustCompile(`href="(/console/#authorize\?request=[^"]+)"`).FindStringSubmatch(page)
	require.Len(t, match, 2)
	link, err := url.Parse(html.UnescapeString(match[1]))
	require.NoError(t, err)
	parameters, err := url.ParseQuery(strings.TrimPrefix(link.EscapedFragment(), "authorize?"))
	require.NoError(t, err)
	continuation := parameters.Get("request")
	require.True(t, strings.HasPrefix(continuation, "/oauth/authorize?"))
	require.NotContains(t, continuation, "unrecognized")
	require.NotContains(t, continuation, "discard")
	oldPending, oldCSRF := field(page, "pending"), browser.csrf
	info := peopleCall[oauth.SessionInfo](t, browser, "POST", "/session/password", map[string]string{"username": owner.Username, "password": personPassword})
	browser.csrf = info.CSRF
	require.NotEqual(t, oldCSRF, info.CSRF)
	page = readPage(continuation)
	require.Contains(t, page, "Allow connection")
	require.NotContains(t, page, `name="key"`)
	require.NotEqual(t, oldPending, field(page, "pending"))
	require.Equal(t, browser.csrf, field(page, "csrf"))
	require.Empty(t, peopleCall[[]oauth.Connection](t, browser, "GET", "/me/connections", nil), "sign-in alone never approves application access")
	form := url.Values{"pending": {field(page, "pending")}, "csrf": {field(page, "csrf")}, "action": {"allow"}}
	request, err := http.NewRequestWithContext(t.Context(), "POST", endpoint.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", endpoint.URL)
	response, err = browser.client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	callback, err := url.Parse(response.Header.Get("Location"))
	require.NoError(t, err)
	require.Equal(t, query.Get("state"), callback.Query().Get("state"))
	response, err = browser.client.PostForm(endpoint.URL+"/oauth/token", url.Values{"client_id": {registration.ID}, "redirect_uri": {query.Get("redirect_uri")}, "grant_type": {"authorization_code"}, "resource": {query.Get("resource")}, "code": {callback.Query().Get("code")}, "code_verifier": {verifier}})
	require.NoError(t, err)
	var tokens struct {
		Access string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&tokens))
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	ref, err := s.access.oauth.Authenticate(t.Context(), tokens.Access, query.Get("resource"))
	require.NoError(t, err)
	require.Equal(t, control.KindPerson, ref.Kind)
	require.Equal(t, owner.ID, ref.ID)
}
