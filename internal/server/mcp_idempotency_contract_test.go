package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/internal/server/mcpserver"
	"github.com/stretchr/testify/require"
)

type mcpBoundOperation struct {
	method, path string
	ID           string `yaml:"operationId"`
	Parameters   []struct {
		Ref string `yaml:"$ref"`
	} `yaml:"parameters"`
	Responses map[string]any `yaml:"responses"`
}

func mcpBoundOperations(t *testing.T) map[string]mcpBoundOperation {
	t.Helper()
	operations := map[string]mcpBoundOperation{}
	for path, methods := range loadOpenAPI(t).Paths {
		for method, node := range methods {
			if method == "parameters" {
				continue
			}
			operation := mcpBoundOperation{method: strings.ToUpper(method), path: path}
			require.NoError(t, node.Decode(&operation))
			if operation.ID != "" {
				operations[operation.ID] = operation
			}
		}
	}
	return operations
}

func TestMCPIdempotencyContract(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	request := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`))
	request.Header.Set("X-API-Key", TestAdminKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/list")
	recorder := httptest.NewRecorder()
	s.engine.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		Result struct {
			Tools []struct {
				Name        string
				InputSchema struct {
					Properties map[string]json.RawMessage
				}
			}
		}
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.NotEmpty(t, response.Result.Tools)
	idempotent := map[routeKey]struct{}{}
	for route := range s.idempotency.routes {
		method, path, ok := strings.Cut(route, " ")
		require.True(t, ok)
		idempotent[rk(method, normalizeGinPath(path))] = struct{}{}
	}
	operations, bindings := mcpBoundOperations(t), mcpserver.OperationBindings()
	checked := 0
	for _, tool := range response.Result.Tools {
		if _, retries := tool.InputSchema.Properties["idempotency_key"]; !retries {
			continue
		}
		t.Run(tool.Name, func(t *testing.T) {
			require.Contains(t, bindings, tool.Name)
			for _, id := range bindings[tool.Name] {
				operation, exists := operations[id]
				require.True(t, exists, "missing operationId %s", id)
				if operation.method != http.MethodGet {
					checked++
					require.Contains(t, idempotent, rk(operation.method, operation.path), "%s advertises retries for %s", tool.Name, id)
					refs := []string{}
					for _, parameter := range operation.Parameters {
						refs = append(refs, parameter.Ref)
					}
					require.Contains(t, refs, "#/components/parameters/IdempotencyKey", id)
					for _, status := range []string{"422", "503", "504"} {
						require.Contains(t, operation.Responses, status, id)
					}
				}
			}
		})
	}
	require.Positive(t, checked)

	// The spec and the mounts name the same routes, so zz_api's spec-driven promise holds too.
	declared := map[routeKey]struct{}{}
	for _, operation := range operations {
		for _, parameter := range operation.Parameters {
			if parameter.Ref == "#/components/parameters/IdempotencyKey" {
				declared[rk(operation.method, operation.path)] = struct{}{}
			}
		}
	}
	require.Equal(t, declared, idempotent)
}
