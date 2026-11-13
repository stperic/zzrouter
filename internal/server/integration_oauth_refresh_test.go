package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestIntegrationOAuthRefreshRetainsAudienceAndScope(t *testing.T) {
	for _, tc := range []struct {
		name, key, path, scope, otherPath string
	}{
		{"management", TestAdminKey, "/zzrouter/v1/mcp", "zzrouter:manage", "/inference/mcp"},
		{"inference", TestUserKey, "/inference/mcp", "zzrouter:inference", "/zzrouter/v1/mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
			clientID, _, refresh := browserOAuthTokens(t, endpoint, tc.key, tc.path)
			form := url.Values{
				"grant_type": {"refresh_token"}, "client_id": {clientID},
				"refresh_token": {refresh}, "scope": {tc.scope + " offline_access"},
				"resource": {endpoint.URL + tc.otherPath},
			}
			response, err := endpoint.Client().PostForm(endpoint.URL+"/oauth/token", form)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusBadRequest, response.StatusCode)

			// A refused audience change must not consume the refresh token.
			// Ordinary clients omit resource and retain its original audience.
			form.Del("resource")
			response, err = endpoint.Client().PostForm(endpoint.URL+"/oauth/token", form)
			require.NoError(t, err)
			var tokens struct {
				Access  string `json:"access_token"`
				Refresh string `json:"refresh_token"`
				Scope   string `json:"scope"`
			}
			require.NoError(t, json.NewDecoder(response.Body).Decode(&tokens))
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.NotEmpty(t, tokens.Access)
			require.NotEmpty(t, tokens.Refresh)
			require.NotEqual(t, refresh, tokens.Refresh)
			require.Equal(t, tc.scope, tokens.Scope)

			session := oauthMCPSession(t, endpoint, tc.path, tokens.Access)
			_, err = session.ListTools(t.Context(), &mcp.ListToolsParams{})
			require.NoError(t, err)
			wrongAudience := makeRequest(t, s, TestRequest{
				Method: "GET", Path: tc.otherPath,
				Headers: map[string]string{"Authorization": "Bearer " + tokens.Access},
			})
			require.Equal(t, http.StatusUnauthorized, wrongAudience.Code)
		})
	}
}
