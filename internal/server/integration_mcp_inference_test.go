package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestIntegrationInferenceMCPAuthAndMount(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey})
	require.Equal(t, "mcp", surfaceForPath("/inference/mcp"))
	require.Equal(t, "user", authTierForPath("/inference/mcp"))
	for _, tc := range []struct {
		name, path, key string
		status          int
	}{
		{"missing key", "/inference/mcp", "", http.StatusUnauthorized},
		{"bad key", "/inference/mcp", "bad-key", http.StatusUnauthorized},
		{"admin on user MCP", "/inference/mcp", TestAdminKey, http.StatusForbidden},
		{"user on admin MCP", "/zzrouter/v1/mcp", TestUserKey, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := makeRequest(t, s, TestRequest{Method: http.MethodPost, Path: tc.path, Headers: map[string]string{
				"Content-Type": "application/json", "Accept": "application/json", "X-API-Key": tc.key,
			}, Body: map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}})
			require.Equal(t, tc.status, response.Code, string(response.Body))
		})
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		response := makeAuthRequest(t, s, method, "/inference/mcp", TestUserKey, nil)
		require.Equal(t, http.StatusMethodNotAllowed, response.Code)
	}
	worker := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, ClusterMode: config.ClusterModeWorker})
	response := makeAuthRequest(t, worker, http.MethodPost, "/inference/mcp", TestUserKey, nil)
	require.Equal(t, http.StatusNotFound, response.Code)

	endpoint := httptest.NewServer(s.engine)
	t.Cleanup(endpoint.Close)
	httpClient := endpoint.Client()
	base := httpClient.Transport
	httpClient.Transport = mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("X-API-Key", TestUserKey)
		return base.RoundTrip(r)
	})
	for _, protocol := range []string{"2025-06-18", "2026-07-28"} {
		t.Run(protocol, func(t *testing.T) {
			client := mcp.NewClient(&mcp.Implementation{Name: "zzrouter-user-protocol-test", Version: "1"}, nil)
			session, connectErr := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: endpoint.URL + "/inference/mcp", HTTPClient: httpClient}, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
			require.NoError(t, connectErr)
			defer session.Close()
			listed, listErr := session.ListTools(t.Context(), nil)
			require.NoError(t, listErr)
			require.Len(t, listed.Tools, 3)
		})
	}
}

func TestIntegrationInferenceMCPDispatchesThroughUserAPI(t *testing.T) {
	var chatCalls, embeddingCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEqual(t, TestUserKey, r.Header.Get("X-API-Key"))
		require.NotEqual(t, TestAdminKey, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"mcp-model"}]}`))
		case "/v1/chat/completions":
			chatCalls.Add(1)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "mcp-model", body["model"])
			require.Equal(t, false, body["stream"])
			_, _ = w.Write([]byte(`{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ready"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		case "/v1/embeddings":
			embeddingCalls.Add(1)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"mcp-model"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(backend.Close)

	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey})
	s.config.Auth.RequireCompatAuth = true
	s.configStore = config.NewMemoryAppsConfigStore(&config.AppsConfig{})
	require.NoError(t, s.appsConfig().AddApp("mcp-provider", config.ServiceConfig{
		Enabled: new(true), Mode: "cloud", Protocol: config.ProtocolOpenAI,
		Runtime:      &config.AppRuntimeConfig{Endpoint: backend.URL, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "upstream-token"}},
		Capabilities: &config.AppCapabilities{Models: []string{"mcp-model"}, WireEndpoints: []string{"chat_completions", "embeddings"}},
	}))
	s.model.Cache.Invalidate()
	s.model.Resolver = staticModelResolver{provider: "mcp-provider"}

	endpoint := httptest.NewServer(s.engine)
	t.Cleanup(endpoint.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "zzrouter-inference-test", Version: "1"}, nil)
	httpClient := endpoint.Client()
	base := httpClient.Transport
	httpClient.Transport = mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("X-API-Key", TestUserKey)
		return base.RoundTrip(r)
	})
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: endpoint.URL + "/inference/mcp", HTTPClient: httpClient}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	require.Len(t, names, 3)
	for _, name := range []string{"zzrouter_inference_models", "zzrouter_chat", "zzrouter_embeddings"} {
		require.True(t, slices.Contains(names, name), "%s missing", name)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"zzrouter_inference_models", map[string]any{}},
		{"zzrouter_chat", map[string]any{"model": "mcp-model", "messages": []map[string]string{{"role": "user", "content": "hello"}}, "idempotency_key": "mcp-chat-1"}},
		{"zzrouter_embeddings", map[string]any{"model": "mcp-model", "input": []string{"hello"}}},
	} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		require.NoError(t, err)
		require.False(t, result.IsError, "%s: %+v", tc.name, result.StructuredContent)
		require.Len(t, result.Content, 1)
		body, ok := result.Content[0].(*mcp.TextContent)
		require.True(t, ok)
		structured, err := json.Marshal(result.StructuredContent)
		require.NoError(t, err)
		require.JSONEq(t, string(structured), body.Text)
	}
	retried, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "zzrouter_chat", Arguments: map[string]any{
		"model": "mcp-model", "messages": []map[string]string{{"role": "user", "content": "hello"}}, "idempotency_key": "mcp-chat-1",
	}})
	require.NoError(t, err)
	require.False(t, retried.IsError, "%+v", retried.StructuredContent)
	require.EqualValues(t, 1, chatCalls.Load())
	require.EqualValues(t, 1, embeddingCalls.Load())
}

func TestIntegrationInferenceMCPKeepsUserPolicyWithBothKeyHeaders(t *testing.T) {
	var backendCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls.Add(1)
		require.NotEqual(t, TestAdminKey, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ready"}}]}`))
	}))
	t.Cleanup(backend.Close)

	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey})
	s.config.Auth.RequireCompatAuth = true
	s.configStore = config.NewMemoryAppsConfigStore(&config.AppsConfig{})
	require.NoError(t, s.appsConfig().AddApp("mcp-provider", config.ServiceConfig{
		Enabled: new(true), Mode: "cloud", Protocol: config.ProtocolOpenAI,
		Runtime:      &config.AppRuntimeConfig{Endpoint: backend.URL, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "upstream-token"}},
		Capabilities: &config.AppCapabilities{Models: []string{"allowed-model", "blocked-model"}, WireEndpoints: []string{"chat_completions"}},
	}))
	s.model.Cache.Invalidate()
	s.model.Resolver = staticModelResolver{provider: "mcp-provider"}
	team := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/teams", TestAdminKey,
		map[string]any{"id": "mcp-team", "name": "MCP team", "allowed_models": []string{"allowed-model"}})
	require.Equal(t, http.StatusCreated, team.Code, string(team.Body))
	userKey := loadTestKey(t, s, "mcp-caller", keys.VirtualKey{Name: "MCP caller", TeamID: "mcp-team", QuotaConfig: quota.QuotaConfig{RPMLimit: 1}})

	endpoint := httptest.NewServer(s.engine)
	t.Cleanup(endpoint.Close)
	httpClient := endpoint.Client()
	base := httpClient.Transport
	httpClient.Transport = mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("X-API-Key", userKey)
		r.Header.Set("Authorization", "Bearer "+TestAdminKey)
		return base.RoundTrip(r)
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "zzrouter-policy-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: endpoint.URL + "/inference/mcp", HTTPClient: httpClient}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	chat := func(model string) *mcp.CallToolResult {
		t.Helper()
		result, callErr := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "zzrouter_chat", Arguments: map[string]any{
			"model": model, "messages": []map[string]string{{"role": "user", "content": "hello"}},
		}})
		require.NoError(t, callErr)
		return result
	}
	blocked := chat("blocked-model")
	require.True(t, blocked.IsError)
	require.Contains(t, blocked.Content[0].(*mcp.TextContent).Text, "HTTP 403")
	require.Zero(t, backendCalls.Load())
	allowed := chat("allowed-model")
	require.False(t, allowed.IsError, "%+v", allowed.StructuredContent)
	require.EqualValues(t, 1, backendCalls.Load())
	limited := chat("allowed-model")
	require.True(t, limited.IsError)
	require.Contains(t, limited.Content[0].(*mcp.TextContent).Text, "HTTP 429")
	require.EqualValues(t, 1, backendCalls.Load())
}
