package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func TestIntegrationBrowserChatCredentialForwarding(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	s.config.Auth.RequireCompatAuth = true
	browser := newSessionBrowser(t, endpoint, TestUserKey)
	s.engine.POST("/test-browser-forwarding", s.auth.optionalAuthMiddleware(true), func(c *gin.Context) {
		_, acceptedBrowser := browserBinding(c.Request.Context())
		headers := make(map[string]http.Header)
		for name, up := range map[string]backend.Upstream{
			"engine": backend.Engine(), "caller": backend.CallerKey(), "cluster": backend.Cluster(),
			"provider": backend.Credential(&config.APIConfig{AuthType: config.AuthTypeAPIKey, AuthHeader: "X-API-Key", Token: "provider-token"}),
		} {
			headers[name] = upstreamHeaders(c.Request.Context(), c.Request.Header, up)
		}
		c.JSON(http.StatusOK, gin.H{"browser": acceptedBrowser, "headers": headers})
	})
	for _, tc := range []struct {
		name, authorization string
		browser             bool
	}{
		{"basic proxy", "Basic fixture-proxy-secret", true},
		{"digest proxy", "Digest fixture-proxy-secret", true},
		{"explicit native", "Bearer " + TestUserKey, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := browser.call(t, "POST", "/test-browser-forwarding", nil, map[string]string{"Authorization": tc.authorization})
			require.Equal(t, http.StatusOK, status, string(body))
			var received struct {
				Browser bool                   `json:"browser"`
				Headers map[string]http.Header `json:"headers"`
			}
			require.NoError(t, json.Unmarshal(body, &received))
			require.Equal(t, tc.browser, received.Browser)
			for name, header := range received.Headers {
				expected := ""
				if !tc.browser && (name == "caller" || name == "cluster") {
					expected = tc.authorization
				}
				require.Equal(t, expected, header.Get("Authorization"), name)
				require.Empty(t, header.Get("Cookie"), name)
				require.Empty(t, header.Get("X-CSRF-Token"), name)
				if name == "provider" {
					require.Equal(t, "provider-token", header.Get("X-API-Key"))
				}
			}
		})
	}
}

func TestIntegrationBrowserChatAuthority(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	s.config.Auth.RequireCompatAuth = true
	browser := newSessionBrowser(t, endpoint, TestUserKey)
	status, body := browser.call(t, "GET", "/v1/models", nil, nil)
	require.Equal(t, 200, status, string(body))
	status, _ = browser.call(t, "POST", "/v1/chat/completions", map[string]any{}, nil)
	require.Equal(t, 400, status, "authenticated request reaches existing body validation")
	for _, headers := range []map[string]string{
		{"Origin": "https://other.example"}, {"Origin": "null"}, {"Origin": ""},
		{"X-CSRF-Token": ""}, {"X-CSRF-Token": "stale-proof"},
	} {
		status, _ = browser.call(t, "POST", "/v1/chat/completions", map[string]any{}, headers)
		require.Equal(t, 403, status, headers)
	}
	status, _ = browser.call(t, "GET", "/v1/models", nil, map[string]string{"Origin": "https://other.example"})
	require.Equal(t, 403, status)
	for _, headers := range []map[string]string{
		{"Authorization": "Bearer invalid"}, {"Authorization": "Bearer"},
		{"X-API-Key": "invalid"}, {"X-API-Key": ""}, {"X-API-Key": "null"},
	} {
		status, _ = browser.call(t, "GET", "/v1/models", nil, headers)
		require.Equal(t, 401, status, "explicit credentials never borrow browser authority: %v", headers)
	}
	status, _ = browser.call(t, "GET", "/v1/models", nil, map[string]string{"Authorization": "Bearer " + TestUserKey, "X-API-Key": "invalid", "Origin": "https://other.example"})
	require.Equal(t, 200, status, "compat Bearer precedence remains unchanged")
	for _, path := range []string{"/v1/completions", "/v1/embeddings", "/v1/responses", "/v1/chat/completions"} {
		method := "POST"
		if path == "/v1/chat/completions" {
			method = "GET"
		}
		status, _ = browser.call(t, method, path, map[string]any{}, nil)
		require.Equal(t, 401, status, path)
	}
	require.NoError(t, s.role.Set(t.Context(), role.RoleWorker, "browser chat test demotion"))
	status, _ = browser.call(t, "GET", "/v1/models", nil, nil)
	require.Equal(t, 401, status, "cookie authority cannot survive coordinator demotion")
}

func TestIntegrationBrowserChatRevocationNeverBecomesAnonymous(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	s.config.Auth.RequireCompatAuth = false
	key := loadTestKey(t, s, "chat-client", keys.VirtualKey{Name: "Chat client", Role: "user"})
	browser := newSessionBrowser(t, endpoint, key)
	require.NoError(t, s.services.Keys.DeleteKey("chat-client", "admin"))
	for _, request := range []struct{ method, path string }{{"GET", "/v1/models"}, {"POST", "/v1/chat/completions"}} {
		status, body := browser.call(t, request.method, request.path, map[string]any{}, nil)
		require.Equal(t, 401, status, string(body))
	}
	response, err := endpoint.Client().Get(endpoint.URL + "/v1/models")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, 200, response.StatusCode, "genuinely anonymous discovery retains its existing contract")
	request, err := http.NewRequestWithContext(t.Context(), "GET", endpoint.URL+"/v1/models", nil)
	require.NoError(t, err)
	request.Header.Set("X-CSRF-Token", "stale-proof-without-cookie")
	response, err = endpoint.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, 401, response.StatusCode)
}

func TestIntegrationBrowserChatUsesInferenceAccessAndQuota(t *testing.T) {
	endpoint := httptest.NewUnstartedServer(nil)
	t.Cleanup(endpoint.Close)
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord",
		OAuthIssuer: "http://" + endpoint.Listener.Addr().String(), SeedProvidersDir: true})
	_, err := s.configStore.SetProviderEnabled("vllm", "", true)
	require.NoError(t, err)
	_, err = s.services.Teams.CreateTeam(&CreateTeamRequest{ID: "chat-team", Name: "Chat team", AllowedModels: []string{"qwen"},
		ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"vllm"}}, RPMLimit: 1}, "admin")
	require.NoError(t, err)
	key := loadTestKey(t, s, "chat-client", keys.VirtualKey{Name: "Chat client", Role: "user", TeamID: "chat-team"})
	routingCatalog(t, s, []map[string]any{replicaItem("coord", "vllm")})
	run := instance.NewInstance("chat-fixture", "vllm", "qwen", 8100, 0, 0)
	run.WireModel = "qwen"
	run.MarkRunning(false, nil)
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	t.Cleanup(func() { _ = s.providers.appMgr.Instances().Remove(run.ID) })
	var calls atomic.Int32
	s.httpStreamingClient = &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		require.Empty(t, r.Header.Get("Cookie"))
		require.Empty(t, r.Header.Get("X-CSRF-Token"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "qwen", body["model"])
		require.Equal(t, true, body["stream"])
		stream := "data: {\"choices\":[{\"delta\":{\"content\":\"Browser reply\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"total_tokens\":4}}\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})}
	endpoint.Config.Handler = s.engine
	endpoint.Start()
	browser := newSessionBrowser(t, endpoint, key)
	request := map[string]any{"model": "forbidden", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hello"}}}
	status, body := browser.call(t, "POST", "/v1/chat/completions", request, nil)
	require.Equal(t, 403, status, string(body))
	require.Zero(t, calls.Load())
	request["model"] = "qwen"
	status, body = browser.call(t, "POST", "/v1/chat/completions", request, nil)
	require.Equal(t, 200, status, string(body))
	require.Contains(t, string(body), "Browser reply")
	require.Equal(t, int32(1), calls.Load())
	status, body = browser.call(t, "POST", "/v1/chat/completions", request, nil)
	require.Equal(t, 429, status, string(body))
	require.Equal(t, int32(1), calls.Load(), "browser chat shares the team's inference quota")
}
