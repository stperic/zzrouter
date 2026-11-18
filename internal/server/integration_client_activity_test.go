package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/observability"
)

func TestIntegrationClientActivityAuthorityAndBrowserExclusion(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	admin := newSessionBrowser(t, endpoint, TestAdminKey)
	user := newSessionBrowser(t, endpoint, TestUserKey)
	status, body := admin.call(t, http.MethodGet, "/zzrouter/v1/clients", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var reply struct {
		Data []observability.ClientObservation `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &reply))
	require.Empty(t, reply.Data, "browser bootstrap and activity reads are not client activity")
	status, _ = user.call(t, http.MethodGet, "/zzrouter/v1/clients", nil, nil)
	require.Equal(t, http.StatusForbidden, status)
	response := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/clients", "", nil)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	response = makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/nodes", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, response.Code)
	status, body = admin.call(t, http.MethodGet, "/zzrouter/v1/clients", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &reply))
	require.Len(t, reply.Data, 1)
	require.Equal(t, "management-api", reply.Data[0].Protocol)
	require.EqualValues(t, 1, reply.Data[0].RequestCount)
	require.Zero(t, reply.Data[0].ActiveRequests)
	require.NotContains(t, string(body), TestAdminKey)
	crossRole(t, s, role.RoleWorker)
	response = makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/clients", TestAdminKey, nil)
	require.NotEqual(t, http.StatusOK, response.Code)
}

func TestIntegrationClientActivityInflightAndMCPDispatch(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	admin := newSessionBrowser(t, endpoint, TestAdminKey)
	s.engine.GET("/zzrouter/v1/test-client-active", s.auth.adminAuthMiddleware(), func(c *gin.Context) {
		status, body := admin.call(t, http.MethodGet, "/zzrouter/v1/clients", nil, nil)
		require.Equal(t, http.StatusOK, status)
		var reply struct {
			Data []observability.ClientObservation
		}
		require.NoError(t, json.Unmarshal(body, &reply))
		require.Len(t, reply.Data, 1)
		require.Equal(t, 1, reply.Data[0].ActiveRequests)
		c.Status(http.StatusNoContent)
	})
	response := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/test-client-active", TestAdminKey, nil)
	require.Equal(t, http.StatusNoContent, response.Code)
	_, token, _ := browserOAuthTokens(t, endpoint, TestAdminKey, adminMCPPath)
	session := oauthMCPSession(t, endpoint, adminMCPPath, token)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "zz_nodes", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	status, body := admin.call(t, http.MethodGet, "/zzrouter/v1/clients", nil, nil)
	require.Equal(t, http.StatusOK, status)
	var reply struct {
		Data []observability.ClientObservation
	}
	require.NoError(t, json.Unmarshal(body, &reply))
	require.Len(t, reply.Data, 2)
	for _, observation := range reply.Data {
		require.Zero(t, observation.ActiveRequests)
		if observation.Protocol == "management-api" {
			require.EqualValues(t, 1, observation.RequestCount, "MCP inner requests must not inflate management API observations")
		} else {
			require.Equal(t, "management-mcp", observation.Protocol)
			require.Equal(t, "oauth", observation.Authentication)
		}
	}
}

func TestIntegrationBrowserPairingOptInIsNarrowAndRequiresCSRF(t *testing.T) {
	_, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	admin := newSessionBrowser(t, endpoint, TestAdminKey)
	for _, headers := range []map[string]string{{"Origin": ""}, {"Origin": "https://attacker.example"}, {"X-CSRF-Token": "incorrect"}} {
		status, _ := admin.call(t, http.MethodPost, "/zzrouter/v1/cluster/pairing/accept", map[string]string{}, headers)
		require.Equal(t, http.StatusForbidden, status)
	}
	status, _ := admin.call(t, http.MethodPost, "/zzrouter/v1/cluster/pairing/accept", map[string]string{}, nil)
	require.Contains(t, []int{http.StatusBadRequest, http.StatusServiceUnavailable}, status, "valid browser authority reaches pairing validation")
	for _, path := range []string{"/zzrouter/v1/cluster/decommission-worker", "/zzrouter/v1/cluster/revoke-worker"} {
		status, _ := admin.call(t, http.MethodPost, path, map[string]string{}, nil)
		require.Equal(t, http.StatusUnauthorized, status, "only pairing acceptance gains browser authority")
	}
	user := newSessionBrowser(t, endpoint, TestUserKey)
	status, _ = user.call(t, http.MethodPost, "/zzrouter/v1/cluster/pairing/accept", map[string]string{}, nil)
	require.Equal(t, http.StatusForbidden, status)
}
