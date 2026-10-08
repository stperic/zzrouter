package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func mcpWireRequest(t *testing.T, method, version string, params map[string]any) *http.Request {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": version, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "http://coordinator/zzrouter/v1/mcp", bytes.NewReader(body))
	r.RemoteAddr = "198.51.100.9:55121"
	r.Header.Set("X-API-Key", TestAdminKey)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", version)
	r.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		r.Header.Set("Mcp-Name", name)
	}
	return r
}

func mcpWireResponse(t *testing.T, s *Server, r *http.Request, status int) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	require.Equal(t, status, w.Code, w.Body.String())
	require.Contains(t, w.Header().Get("Content-Type"), "application/json")
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	require.Equal(t, "2.0", body["jsonrpc"])
	require.EqualValues(t, 1, body["id"])
	return body
}

// https://modelcontextprotocol.io/specification/2026-07-28/server/discover
func TestMCPWireDiscovery(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	result := mcpWireResponse(t, s, mcpWireRequest(t, "server/discover", "2026-07-28", nil), http.StatusOK)["result"].(map[string]any)
	require.Contains(t, result["supportedVersions"], "2026-07-28")
	require.Contains(t, result["supportedVersions"], "2025-06-18")
	capabilities := result["capabilities"].(map[string]any)
	require.Contains(t, capabilities, "tools")
	for _, absent := range []string{"resources", "prompts", "completions"} {
		require.NotContains(t, capabilities, absent)
	}
	meta := result["_meta"].(map[string]any)
	require.Equal(t, "zzrouter", meta["io.modelcontextprotocol/serverInfo"].(map[string]any)["name"])
	require.Contains(t, result["instructions"], "zz_api_search")
}

// https://modelcontextprotocol.io/specification/2026-07-28/basic
func TestMCPWireResultTypes(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, tc := range []struct {
		method, name string
		args         map[string]any
		toolError    bool
	}{
		{method: "server/discover"}, {method: "tools/list"},
		{method: "tools/call", name: "zz_overview", args: map[string]any{}},
		{method: "tools/call", name: "zz_job", args: map[string]any{"job_id": "missing"}, toolError: true},
	} {
		t.Run(tc.method+tc.name, func(t *testing.T) {
			params := map[string]any{}
			if tc.name != "" {
				params["name"], params["arguments"] = tc.name, tc.args
			}
			result := mcpWireResponse(t, s, mcpWireRequest(t, tc.method, "2026-07-28", params), http.StatusOK)["result"].(map[string]any)
			require.Equal(t, "complete", result["resultType"])
			if tc.toolError {
				require.Equal(t, true, result["isError"])
			}
		})
	}
}

// https://modelcontextprotocol.io/specification/2026-07-28/server/utilities/caching; https://modelcontextprotocol.io/specification/2026-07-28/server/tools
func TestMCPWireListContract(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	expected := map[string]struct{ readOnly, destructive, idempotent bool }{
		"zz_overview": {readOnly: true}, "zz_job": {readOnly: true}, "zz_nodes": {readOnly: true},
		"zz_models": {readOnly: true}, "zz_model_search": {readOnly: true}, "zz_model_download": {},
		"zz_runs": {readOnly: true}, "zz_run_preview": {readOnly: true}, "zz_run_start": {}, "zz_run_restart": {},
		"zz_run_stop": {destructive: true}, "zz_chat": {}, "zz_logs": {readOnly: true}, "zz_job_cancel": {destructive: true},
		"zz_providers": {readOnly: true}, "zz_provider_install": {}, "zz_params_get": {readOnly: true}, "zz_params_set": {},
		"zz_model_files": {readOnly: true}, "zz_api_search": {readOnly: true}, "zz_api_get": {readOnly: true}, "zz_api": {destructive: true},
		"zz_update_status": {readOnly: true}, "zz_update_check": {idempotent: true}, "zz_update_apply": {destructive: true},
	}
	expectedNames := make([]string, 0, len(expected))
	for name := range expected {
		expectedNames = append(expectedNames, name)
	}
	sort.Strings(expectedNames)
	var previous []string
	for range 2 {
		result := mcpWireResponse(t, s, mcpWireRequest(t, "tools/list", "2026-07-28", nil), http.StatusOK)["result"].(map[string]any)
		require.EqualValues(t, 300000, result["ttlMs"])
		require.Contains(t, []string{"public", "private"}, result["cacheScope"])
		tools := result["tools"].([]any)
		require.Len(t, tools, len(expected))
		names := make([]string, 0, len(tools))
		for _, value := range tools {
			tool := value.(map[string]any)
			name := tool["name"].(string)
			names = append(names, name)
			hint, exists := expected[name]
			require.True(t, exists, "unexpected tool %s", name)
			require.Equal(t, "object", tool["inputSchema"].(map[string]any)["type"], name)
			annotations := tool["annotations"].(map[string]any)
			read, _ := annotations["readOnlyHint"].(bool)
			require.Equal(t, hint.readOnly, read, name)
			require.Equal(t, hint.destructive, annotations["destructiveHint"], name)
			idempotent, _ := annotations["idempotentHint"].(bool)
			require.Equal(t, hint.idempotent, idempotent, name)
			require.NotContains(t, tool["description"], "—", name)
		}
		require.Equal(t, expectedNames, names, "exact tool set in deterministic order")
		if previous != nil {
			require.Equal(t, previous, names)
		}
		previous = names
	}
}

// https://modelcontextprotocol.io/specification/2026-07-28/server/tools#output-schema (clients and the official suite reject boolean subschemas).
func TestMCPWireObjectSubschemas(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	result := mcpWireResponse(t, s, mcpWireRequest(t, "tools/list", "2026-07-28", nil), http.StatusOK)["result"].(map[string]any)
	stable := map[string]bool{"zz_overview": true, "zz_chat": true, "zz_run_start": true, "zz_run_restart": true, "zz_model_download": true}
	for _, value := range result["tools"].([]any) {
		tool := value.(map[string]any)
		t.Run(tool["name"].(string), func(t *testing.T) {
			assertObjectSubschemas(t, tool["inputSchema"], "inputSchema")
			output, present := tool["outputSchema"]
			require.Equal(t, stable[tool["name"].(string)], present)
			if present {
				assertObjectSubschemas(t, output, "outputSchema")
				require.NotContains(t, output.(map[string]any)["properties"], "data")
			}
		})
	}
}

func assertObjectSubschemas(t *testing.T, value any, path string) {
	t.Helper()
	schema, ok := value.(map[string]any)
	require.True(t, ok, "%s must be an object subschema, got %T", path, value)
	for key, child := range schema {
		switch key {
		case "properties", "$defs", "definitions", "patternProperties", "dependentSchemas":
			for name, sub := range child.(map[string]any) {
				assertObjectSubschemas(t, sub, path+"."+key+"."+name)
			}
		case "items", "additionalProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems", "propertyNames", "contains", "not", "if", "then", "else", "contentSchema":
			assertObjectSubschemas(t, child, path+"."+key)
		case "allOf", "anyOf", "oneOf", "prefixItems":
			for _, sub := range child.([]any) {
				assertObjectSubschemas(t, sub, path+"."+key)
			}
		}
	}
}

// https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http
func TestMCPWireProtocolErrors(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, tc := range []struct {
		name, version, header string
		code                  int
	}{
		{"method mismatch", "2026-07-28", "server/discover", -32020},
		{"unsupported version", "2099-01-01", "tools/list", -32022},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := mcpWireRequest(t, "tools/list", tc.version, nil)
			r.Header.Set("Mcp-Method", tc.header)
			err := mcpWireResponse(t, s, r, http.StatusBadRequest)["error"].(map[string]any)
			require.EqualValues(t, tc.code, err["code"])
			if tc.code == -32022 {
				require.Contains(t, err["data"].(map[string]any)["supported"], "2026-07-28")
			}
		})
	}
}

// https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http
func TestMCPWireHTTPRefusals(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, tc := range []struct {
		method, origin string
		status         int
	}{
		{http.MethodGet, "", http.StatusMethodNotAllowed},
		{http.MethodDelete, "", http.StatusMethodNotAllowed},
		{http.MethodPost, "https://untrusted.example", http.StatusForbidden},
	} {
		t.Run(tc.method+tc.origin, func(t *testing.T) {
			r := mcpWireRequest(t, "tools/list", "2026-07-28", nil)
			r.Method = tc.method
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, r)
			require.Equal(t, tc.status, w.Code, w.Body.String())
		})
	}
}

func TestIntegrationMCPLegacySmoke(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	client := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("X-API-Key", TestAdminKey)
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		return w.Result(), nil
	})}
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "legacy-smoke", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://coordinator/zzrouter/v1/mcp", HTTPClient: client}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	require.NoError(t, err)
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, tools.Tools, 25)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "zz_overview", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(data), "endpoint_count")
	require.NotContains(t, string(data), `"endpoints"`)
	require.Contains(t, string(data), "/openapi.yaml")
}
