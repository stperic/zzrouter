package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

// Registered test runs exercise real stop cleanup without starting a provider process.
func TestMCPStopConfirmsRemovalAndRetainsFailures(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, id := range []string{"live", "failed"} {
		inst := instance.NewInstance(id, "llamacpp", id, 0, 0, 0)
		if id == "live" {
			inst.MarkRunning(false, nil)
		} else {
			inst.MarkFailed("test failure")
		}
		require.NoError(t, s.providers.appMgr.Instances().Register(inst))
	}
	result := callMCPWireTool(t, s, "zz_run_stop", map[string]any{"run_id": "live"})
	require.NotEqual(t, true, result["isError"], result)
	require.Equal(t, "live", result["structuredContent"].(map[string]any)["run_id"])
	require.Equal(t, "stopped", result["structuredContent"].(map[string]any)["status"])
	lookup := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/runs/live", TestAdminKey, nil)
	require.Equal(t, http.StatusNotFound, lookup.Code, string(lookup.Body))
	for _, status := range []string{"", "starting", "running", "unhealthy", "stopping", "failed", "restarting", "stopped", "typo"} {
		t.Run(status, func(t *testing.T) {
			resp := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/runs?status="+status, TestAdminKey, nil)
			if status == "stopped" || status == "typo" {
				require.Equal(t, http.StatusBadRequest, resp.Code)
				require.Contains(t, string(resp.Body), "stopped runs are removed")
				return
			}
			require.Equal(t, http.StatusOK, resp.Code, string(resp.Body))
			var body struct{ Data []instance.InstanceInfo }
			require.NoError(t, json.Unmarshal(resp.Body, &body))
			if status == "" || status == "failed" {
				require.Len(t, body.Data, 1)
				require.Equal(t, "failed", body.Data[0].ID)
			} else {
				require.Empty(t, body.Data)
			}
		})
	}
}

func TestMCPModelsFilterUsesCatalogNamesAndSourceAliases(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	// Synthetic worker detail is explicitly unsupported; catalog filtering remains asserted.
	s.services.Model.router = &mockRouter{response: &routing.Response{StatusCode: 501, Body: []byte(`{"title":"Not Implemented","status":501,"code":"model_detail_unsupported","detail":"synthetic worker has no detail reader"}`)}}
	// Synthetic worker reports exercise the real catalog filter and MCP projection.
	routingCatalog(t, s, []map[string]any{
		{"name": "qwen2.5", "node": "worker", "assigned_app": "llamacpp", "source_id": "vendor/original", "details": map[string]any{"wire_endpoints": []string{"chat_completions"}, "features": map[string]string{"vision": "unsupported"}}},
		{"name": "unrelated-local-name", "node": "worker", "assigned_app": "vllm", "source_id": "vendor/qwen2.5"},
		{"name": "org/qwen2.5-instruct", "node": "worker", "assigned_app": "llamacpp"},
	})
	for _, tc := range []struct {
		filter string
		names  []string
	}{
		{"QWEN2.5", []string{"qwen2.5", "unrelated-local-name"}},
		{"VENDOR/QWEN2.5", []string{"unrelated-local-name"}},
		{"*QWEN2.5*", []string{"qwen2.5", "unrelated-local-name", "org/qwen2.5-instruct"}},
		{"QWEN", nil},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			result := callMCPWireTool(t, s, "zz_models", map[string]any{"model": tc.filter})
			require.NotEqual(t, true, result["isError"], result)
			var names []string
			for _, value := range result["structuredContent"].(map[string]any)["models"].([]any) {
				m := value.(map[string]any)
				names = append(names, m["name"].(string))
				if m["name"] == "qwen2.5" {
					require.Equal(t, map[string]any{"wire_endpoints": []any{"chat_completions", "messages_compat", "responses_compat"}, "features": map[string]any{"vision": "unsupported"}}, m["details"])
				}
				if m["name"] == "unrelated-local-name" {
					require.Equal(t, "vendor/qwen2.5", m["source_id"])
				}
			}
			require.ElementsMatch(t, tc.names, names)
		})
	}
}

func TestRunPolishSpecContract(t *testing.T) {
	spec := loadSpecTree(t)
	paths := spec["paths"].(map[string]any)
	get := paths["/zzrouter/v1/runs"].(map[string]any)["get"].(map[string]any)
	foundStatus := false
	for _, value := range get["parameters"].([]any) {
		param := value.(map[string]any)
		if param["name"] == "status" {
			foundStatus = true
			require.Equal(t, []any{"starting", "running", "unhealthy", "stopping", "failed", "restarting"}, param["schema"].(map[string]any)["enum"])
		}
	}
	require.True(t, foundStatus, "status parameter must remain documented")
	require.Contains(t, get["responses"].(map[string]any), "400")
	post := paths["/zzrouter/v1/runs"].(map[string]any)["post"].(map[string]any)
	response := post["responses"].(map[string]any)["202"].(map[string]any)
	schema := response["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	data := schema["allOf"].([]any)[1].(map[string]any)["properties"].(map[string]any)["data"].(map[string]any)
	require.Contains(t, data["properties"].(map[string]any)["health_url"].(map[string]any)["description"], "Node-local")
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	for _, tc := range []struct{ schema, property string }{{"JobEvent", "meta"}, {"RunHealth", "health_url"}} {
		property := schemas[tc.schema].(map[string]any)["properties"].(map[string]any)[tc.property].(map[string]any)
		require.Contains(t, property["description"], "node")
		require.Contains(t, property["description"], "loopback")
	}
}
