package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func providerMCPSession(t *testing.T, s *Server, path string, headers http.Header, secure ...bool) *mcp.ClientSession {
	t.Helper()
	var endpoint *httptest.Server
	if len(secure) > 0 && secure[0] {
		endpoint = httptest.NewTLSServer(s.engine)
	} else {
		endpoint = httptest.NewServer(s.engine)
	}
	t.Cleanup(endpoint.Close)
	httpClient := endpoint.Client()
	base := httpClient.Transport
	httpClient.Transport = mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		for key, values := range headers {
			r.Header[key] = append([]string(nil), values...)
		}
		return base.RoundTrip(r)
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "provider-extensibility-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: endpoint.URL + path, HTTPClient: httpClient}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	return session
}

func TestIntegrationConnectionURLsUseActualIngressScheme(t *testing.T) {
	s := operationTestNode(t, "coordinator", config.ClusterModeDisabled)
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	seedOperationRun(t, s, "llamacpp", "model", upstream.URL)
	request := httptest.NewRequest(http.MethodGet, "https://forged.invalid/inference/v1/connections", nil)
	request.TLS = nil // Actual HTTP with an absolute-form request target claiming HTTPS.
	request.Header.Set("X-API-Key", TestUserKey)
	response := httptest.NewRecorder()
	s.engine.ServeHTTP(response, request)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"routed_urls":["http://forged.invalid/v1"]`)
	require.NotContains(t, response.Body.String(), "https://forged.invalid")
	for _, secure := range []bool{false, true} {
		session := providerMCPSession(t, s, "/inference/mcp", http.Header{"X-API-Key": {TestUserKey}}, secure)
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "zzrouter_connections", Arguments: map[string]any{}})
		require.NoError(t, err)
		require.False(t, result.IsError, "%+v", result)
		rows := result.StructuredContent.(map[string]any)["data"].([]any)
		routed := rows[0].(map[string]any)["routed_urls"].([]any)[0].(string)
		if secure {
			require.True(t, strings.HasPrefix(routed, "https://"), routed)
		} else {
			require.True(t, strings.HasPrefix(routed, "http://"), routed)
		}
	}
}

func providerMCPNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestIntegrationMCPProviderToolsKeepRESTContractsAndFixedInventory(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/inspect", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), "9007199254740993")
		for _, header := range []string{"X-API-Key", "Authorization", "X-Cluster-API-Key"} {
			require.Empty(t, r.Header.Get(header))
		}
		_, _ = w.Write([]byte(`{"tokens":[9007199254740993]}`))
	}))
	defer upstream.Close()
	s := operationTestNode(t, "coordinator", config.ClusterModeDisabled)
	run := seedOperationRun(t, s, "llamacpp", "model", upstream.URL)
	admin := providerMCPSession(t, s, "/zzrouter/v1/mcp", http.Header{"X-API-Key": {TestAdminKey}, "Authorization": {"Bearer " + TestUserKey}})
	user := providerMCPSession(t, s, "/inference/mcp", http.Header{"X-API-Key": {TestUserKey}, "Authorization": {"Bearer " + TestAdminKey}})
	before := providerMCPNames(t, admin)
	require.Contains(t, before, "zz_operation_execute")
	userNames := providerMCPNames(t, user)
	require.Len(t, userNames, 7)
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(svc *config.ServiceConfig) error {
		operation := svc.Operations["tokenize"]
		operation.Binding.Path = "/inspect"
		operation.Description = "Inspect nested JSON with a config-only diagnostic binding"
		operation.InputSchema = config.OperationSchema{"type": "object", "required": []any{"payload"}, "additionalProperties": false, "properties": map[string]any{"payload": map[string]any{"type": "object", "required": []any{"numbers"}, "properties": map[string]any{"numbers": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}}}}}
		svc.Operations["inspect"] = operation
		return nil
	}))
	require.Equal(t, before, providerMCPNames(t, admin))
	require.Equal(t, userNames, providerMCPNames(t, user))
	operation, found := s.appsConfig().Operation("llamacpp.inspect")
	require.True(t, found)
	request := operationTestRequest(t, s, run)
	args := map[string]any{"operation_id": "llamacpp.inspect", "version": operation.Definition().Version, "definition_revision": operation.Revision(), "target": request.Target, "preconditions": request.Preconditions, "input": json.RawMessage(`{"payload":{"numbers":[9007199254740993]}}`)}
	for _, call := range []struct {
		name  string
		input any
	}{{"zz_operations", map[string]any{"provider": "llamacpp"}}, {"zz_operation", map[string]any{"operation_id": "llamacpp.inspect"}}, {"zz_connections", map[string]any{}}, {"zz_operation_execute", args}} {
		result, err := admin.CallTool(t.Context(), &mcp.CallToolParams{Name: call.name, Arguments: call.input})
		require.NoError(t, err)
		require.False(t, result.IsError, "%s: %+v", call.name, result)
		if call.name == "zz_operation_execute" {
			require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "9007199254740993")
		}
		if call.name == "zz_connections" {
			rows := result.StructuredContent.(map[string]any)["data"].([]any)
			require.NotEmpty(t, rows)
			require.NotEmpty(t, rows[0].(map[string]any)["routed_urls"])
		}
	}
	require.EqualValues(t, 1, calls.Load())
	args["input"] = map[string]any{"payload": "bad"}
	invalid, err := admin.CallTool(t.Context(), &mcp.CallToolParams{Name: "zz_operation_execute", Arguments: args})
	require.NoError(t, err)
	require.True(t, invalid.IsError)
	require.Contains(t, invalid.Content[0].(*mcp.TextContent).Text, "operation_input_invalid")
	require.EqualValues(t, 1, calls.Load())
	for _, call := range []struct {
		name    string
		input   any
		failure bool
	}{{"zzrouter_operations", map[string]any{}, false}, {"zzrouter_connections", map[string]any{}, false}, {"zzrouter_operation", map[string]any{"operation_id": "llamacpp.inspect"}, true}, {"zzrouter_operation_execute", args, true}} {
		result, err := user.CallTool(t.Context(), &mcp.CallToolParams{Name: call.name, Arguments: call.input})
		require.NoError(t, err)
		require.Equal(t, call.failure, result.IsError, "%s: %+v", call.name, result)
		if call.failure {
			require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "operation_not_found")
		}
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestIntegrationInferenceMCPPreservesRichNativeJSON(t *testing.T) {
	requests := make(chan []byte, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"rich-model"}]}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		requests <- body
		if r.URL.Path == "/v1/embeddings" {
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1],"index":0}],"model":"rich-model"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chat-rich","choices":[{"message":{"role":"assistant","content":"ready"}}],"native":{"large":9007199254740993}}`))
	}))
	defer upstream.Close()
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey})
	s.config.Auth.RequireCompatAuth = true
	s.configStore = config.NewMemoryAppsConfigStore(&config.AppsConfig{})
	require.NoError(t, s.appsConfig().AddApp("rich-provider", config.ServiceConfig{Enabled: new(true), Mode: "cloud", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: upstream.URL, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "fixture-upstream-token"}}, Capabilities: &config.AppCapabilities{Models: []string{"rich-model"}, WireEndpoints: []string{"chat_completions", "embeddings"}}}))
	s.model.Cache.Invalidate()
	s.model.Resolver = staticModelResolver{provider: "rich-provider"}
	session := providerMCPSession(t, s, "/inference/mcp", http.Header{"X-API-Key": {TestUserKey}, "Authorization": {"Bearer " + TestAdminKey}})
	messages := json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"result"}]}]`)
	options := json.RawMessage(`{"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"count":{"type":"integer","maximum":9007199254740993}}}}}],"response_format":{"type":"json_schema","json_schema":{"name":"result","schema":{"type":"object"}}},"provider_option":{"nested":[9007199254740993,true,null]},"parallel_tool_calls":false}`)
	args := map[string]any{"model": "rich-model", "messages": messages, "max_tokens": 32, "temperature": 0, "options": options}
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "zzrouter_chat", Arguments: args})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "9007199254740993")
	body := <-requests
	for _, field := range []string{`"tool_calls"`, `"tool_call_id"`, `"response_format"`, `"tools"`, `"temperature":0`, `"parallel_tool_calls":false`, `9007199254740993`} {
		require.Contains(t, string(body), field)
	}
	var direct map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &direct))
	rest := makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", TestUserKey, direct)
	require.Equal(t, 200, rest.Code, string(rest.Body))
	require.JSONEq(t, string(body), string(<-requests), "MCP native JSON has the same REST payload")
	embedding, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "zzrouter_embeddings", Arguments: map[string]any{"model": "rich-model", "input": json.RawMessage(`[[1,2],[3,4]]`), "options": map[string]any{"dimensions": 64, "encoding_format": "float", "provider_option": json.RawMessage(`{"large":9007199254740993}`)}}})
	require.NoError(t, err)
	require.False(t, embedding.IsError, "%+v", embedding)
	embedBody := <-requests
	for _, field := range []string{`"input":[[1,2],[3,4]]`, `"dimensions":64`, `"encoding_format":"float"`, `9007199254740993`} {
		require.Contains(t, string(embedBody), field)
	}
	for _, field := range []string{"model", "messages", "max_tokens", "temperature", "stream", "stream_options", "idempotency_key"} {
		args["options"] = map[string]any{field: true}
		refused, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "zzrouter_chat", Arguments: args})
		require.NoError(t, err)
		require.True(t, refused.IsError, "options.%s", field)
		require.Contains(t, refused.Content[0].(*mcp.TextContent).Text, "reserved")
	}
	select {
	case extra := <-requests:
		t.Fatalf("invalid options dispatched: %s", strings.TrimSpace(string(extra)))
	default:
	}
}
