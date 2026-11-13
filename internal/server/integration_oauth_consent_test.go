package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestIntegrationOAuthConsentDisclosesUnverifiedClientDestination(t *testing.T) {
	for _, state := range []string{"first-login", "signed-in", "failed-login"} {
		t.Run(state, func(t *testing.T) {
			_, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
			browser := *endpoint.Client()
			jar, err := cookiejar.New(nil)
			require.NoError(t, err)
			browser.Jar = jar
			if state == "signed-in" {
				browser = *newSessionBrowser(t, endpoint, TestAdminKey).client
			}
			name := `Trusted Desktop <img src=x onerror=alert(1)>`
			callback := "https://untrusted.example/oauth/callback?mode=mcp&account=fixture"
			registration, err := json.Marshal(map[string]any{"client_name": name, "redirect_uris": []string{callback}})
			require.NoError(t, err)
			response, err := browser.Post(endpoint.URL+"/oauth/register", "application/json", strings.NewReader(string(registration)))
			require.NoError(t, err)
			var client struct {
				ID string `json:"client_id"`
			}
			require.NoError(t, json.NewDecoder(response.Body).Decode(&client))
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusCreated, response.StatusCode)
			challenge := sha256.Sum256([]byte(strings.Repeat("c", 64)))
			query := url.Values{"client_id": {client.ID}, "redirect_uri": {callback}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "resource": {endpoint.URL + adminMCPPath}}
			response, err = browser.Get(endpoint.URL + "/oauth/authorize?" + query.Encode())
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusOK, response.StatusCode, string(body))
			if state == "failed-login" {
				field := func(name string) string {
					match := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindSubmatch(body)
					require.Len(t, match, 2)
					return string(match[1])
				}
				form := url.Values{"key": {"invalid-fixture"}, "csrf": {field("csrf")}, "pending": {field("pending")}, "action": {"allow"}}
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
				require.NoError(t, err)
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				request.Header.Set("Origin", endpoint.URL)
				response, err = browser.Do(request)
				require.NoError(t, err)
				body, err = io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, http.StatusOK, response.StatusCode, string(body))
				require.Contains(t, string(body), "Sign-in failed")
			}
			require.Contains(t, string(body), html.EscapeString(name))
			require.NotContains(t, string(body), "<img src=x")
			require.Contains(t, string(body), "Client name (unverified)")
			require.Contains(t, string(body), "Callback address")
			require.Contains(t, string(body), html.EscapeString(callback))
			require.Less(t, strings.Index(string(body), html.EscapeString(callback)), strings.Index(string(body), `name="action" value="allow"`), "destination must be visible before approval")
		})
	}
}
