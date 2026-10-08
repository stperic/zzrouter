package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func callAgentTool(t *testing.T, d dispatcher, name string, args map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": name, "arguments": args, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}},
	}})
	require.NoError(t, err)
	r := httptest.NewRequest("POST", "http://coordinator/zzrouter/v1/mcp", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	r.Header.Set("Mcp-Method", "tools/call")
	r.Header.Set("Mcp-Name", name)
	r.Header.Set("X-API-Key", testKey)
	r.RemoteAddr = "198.51.100.8:47821"
	w := httptest.NewRecorder()
	New(d.cfg).ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.NotContains(t, response, "error", w.Body.String())
	return response["result"].(map[string]any)
}

// Synthetic fixtures mirror controller DTOs, including unwrapped resolved/cancel replies.
func TestAgentToolsRESTContracts(t *testing.T) {
	for _, tc := range []struct{ name, method, path, args, query, body, response, want string }{
		{"zz_nodes", "GET", "/zzrouter/v1/nodes", "{}", "limit=50&offset=0", "", `{"data":[{"name":"coord","os":"Linux","cluster_role":"coordinator","health_status":"healthy","disk":{"available_gb":10},"config_path":"drop"}],"total":1}`, `[{"name":"coord","os":"Linux","cluster_role":"coordinator","health_status":"healthy","disk":{"available_gb":10}}]`},
		{"zz_nodes", "GET", "/zzrouter/v1/nodes/worker one", `{"node":"worker one"}`, "", "", `{"data":{"name":"worker one","health_status":"down","config_path":"drop"}}`, `{"name":"worker one","health_status":"down"}`},
		{"zz_models", "GET", "/zzrouter/v1/models", `{"node":"worker","provider":"vllm","limit":2}`, "limit=2&offset=0&node=worker&provider=vllm&model=", "", `{"data":[{"name":"m","assigned_app":"vllm","node":"worker","source_id":"org/M","details":{"drop":"huge","wire_endpoints":["embeddings"],"features":{"vision":"unsupported"}}}],"total":1}`, `[{"name":"m","source_id":"org/M","provider":"vllm","node":"worker","details":{"wire_endpoints":["embeddings"],"features":{"vision":"unsupported"}}}]`},
		{"zz_model_search", "GET", "/zzrouter/v1/search", `{"query":"tiny"}`, "limit=50&offset=0&type=models&q=tiny&provider=", "", `{"data":{"results":[{"id":"org/model","downloads":5,"tags":["drop"]}],"total":1,"pagination":{"has_next":true}}}`, `[{"model":"org/model","downloads":5}]`},
		{"zz_model_files", "GET", "/zzrouter/v1/registries/huggingface/variants", `{"model":"org/repo","provider":"llamacpp"}`, "id=org%2Frepo&provider=llamacpp", "", `{"data":{"model_id":"org/repo","variants":[{"file":"b-00001-of-00002.gguf","shards":2,"quantization":"Q4_K_M","bits":4,"size_bytes":123,"size_human":"123 B","recommended":true,"drop":1}],"features":[{"feature":"vision","file":"mmproj-F16.gguf","size_bytes":9,"size_human":"9 B"}],"total_size_bytes":222}}`, `{"variants":[{"file":"b-00001-of-00002.gguf","shards":2,"quantization":"Q4_K_M","bits":4,"size_bytes":123,"size_human":"123 B","recommended":true}],"features":[{"feature":"vision","file":"mmproj-F16.gguf","size_bytes":9,"size_human":"9 B"}]}`},
		{"zz_providers", "GET", "/zzrouter/v1/providers", `{"node":"worker"}`, "node=worker&limit=50&offset=0", "", `{"data":[{"name":"llamacpp","node":"worker","kind":"on-demand","enabled":false,"installed":true,"managed":false,"version":"b123"}],"total":1}`, `[{"name":"llamacpp","node":"worker","kind":"on-demand","enabled":false,"installed":true,"managed":false,"version":"b123"}]`},
		{"zz_runs", "GET", "/zzrouter/v1/runs", "{}", "limit=50&offset=0&node=&status=", "", `{"data":[{"id":"r","status":"failed","error_message":"bad parameters","environment":{"drop":"private"}}],"total":1}`, `[{"run_id":"r","status":"failed","error_message":"bad parameters"}]`},
		{"zz_runs", "GET", "/zzrouter/v1/runs/r", `{"run_id":"r","node":"worker"}`, "limit=50&offset=0&node=worker&status=", "", `{"data":{"id":"r","status":"running","process_id":42}}`, `{"run_id":"r","status":"running"}`},
		{"zz_run_preview", "POST", "/zzrouter/v1/runs/preview", `{"model":"m","provider":"vllm","node":"worker","environment":{"A":"1"}}`, "", `{"model":"m","provider":"vllm","node":"worker","environment":{"A":"1"}}`, `{"data":{"full_command":"engine --model m","parameters":{"ctx":"32"},"parameter_sources":{"ctx":"request"},"working_dir":"drop"}}`, `{"full_command":"engine --model m","parameters":{"ctx":"32"},"parameter_sources":{"ctx":"request"}}`},
		{"zz_run_start", "POST", "/zzrouter/v1/runs", `{"model":"m","provider":"vllm","node":"worker","env_vars":{"A":"1"},"idempotency_key":"retry"}`, "", `{"model":"m","provider":"vllm","node":"worker","env_vars":{"A":"1"}}`, `{"data":{"id":"new","job_id":"j","node":"worker","health_url":"drop"}}`, `{"run_id":"new","job_id":"j","node":"worker"}`},
		{"zz_run_restart", "POST", "/zzrouter/v1/runs/r/restart", `{"run_id":"r","node":"worker","idempotency_key":"retry"}`, "node=worker", "", `{"data":{"id":"new","old_id":"r","job_id":"j","node":"worker"}}`, `{"run_id":"new","old_run_id":"r","job_id":"j","node":"worker"}`},
		{"zz_run_stop", "DELETE", "/zzrouter/v1/runs/r", `{"run_id":"r","node":"worker"}`, "node=worker", "", `{"data":{"id":"r","status":"stopped"}}`, `{"run_id":"r","status":"stopped"}`},
		{"zz_model_download", "POST", "/zzrouter/v1/deployments", `{"model":"org/model","node":"worker","registry":"huggingface","features":["vision"],"idempotency_key":"retry"}`, "", `{"model":"org/model","nodes":["worker"],"registry":"huggingface","provider":"","file":"","format":"","features":["vision"]}`, `{"data":{"id":"d","model":"org/model","file":"q4.gguf","status":"downloading","nodes":[{"node":"worker","job_id":"j","status":"downloading","bytes_total":999999}]}}`, `{"deployment_id":"d","model":"org/model","file":"q4.gguf","status":"downloading","nodes":[{"node":"worker","job_id":"j","status":"downloading"}]}`},
		{"zz_job_cancel", "DELETE", "/zzrouter/v1/jobs/j", `{"job_id":"j","node":"worker"}`, "node=worker", "", `{"accepted":true,"job_id":"j"}`, `{"accepted":true,"job_id":"j"}`},
		{"zz_job_cancel", "DELETE", "/zzrouter/v1/deployments/d", `{"deployment_id":"d"}`, "node=", "", `{"data":{"id":"d","status":"cancelled"}}`, `{"id":"d","status":"cancelled"}`},
		{"zz_providers", "GET", "/zzrouter/v1/providers", "{}", "limit=50&offset=0&node=", "", `{"data":[{"name":"llamacpp","kind":"on-demand","enabled":false,"installed":false,"managed":false,"version":"unknown","node":"coord","description":"drop"}],"total":1}`, `[{"name":"llamacpp","kind":"on-demand","enabled":false,"installed":false,"managed":false,"version":"unknown","node":"coord"}]`},
		{"zz_providers", "GET", "/zzrouter/v1/providers/vllm/status", `{"provider":"vllm","node":"worker"}`, "node=worker", "", `{"data":{"name":"vllm","installed":true,"enabled":true,"managed":true,"managed_process":"running","mode":"on-demand","instances":[{"drop":"huge"}]}}`, `{"name":"vllm","installed":true,"enabled":true,"managed":true,"managed_process":"running","mode":"on-demand"}`},
		{"zz_provider_install", "POST", "/zzrouter/v1/providers/vllm/install/preflight", `{"provider":"vllm","preflight_only":true,"node":"worker"}`, "node=worker", "{}", `{"data":{"provider":"vllm","all_ok":false,"results":[{"check":"gpu","passed":false,"hint":"detect a GPU"}]}}`, `{"provider":"vllm","all_ok":false,"results":[{"check":"gpu","passed":false,"hint":"detect a GPU"}]}`},
		{"zz_provider_install", "POST", "/zzrouter/v1/providers/vllm/install", `{"provider":"vllm","version":"1.0","runtime":"vllm","node":"worker","idempotency_key":"retry"}`, "node=worker", `{"version":"1.0","runtime":"vllm"}`, `{"data":{"accepted":true,"job_id":"j","provider":"vllm","node":"worker","stream_url":"drop"}}`, `{"accepted":true,"job_id":"j","provider":"vllm","node":"worker"}`},
		{"zz_params_set", "PATCH", "/zzrouter/v1/providers/vllm/parameters", `{"provider":"vllm","restart_affected":true,"patch":{"defaults":{"parameters":{"ctx":8192,"old":null}}}}`, "node=&model=&restart=affected", `{"defaults":{"parameters":{"ctx":8192,"old":null}}}`, `{"parameters":{"ctx":{"value":8192,"tier":"operator"}},"runs":{"restart_job_id":"j","stale":[{"id":"old","node":"worker","model":"m","changed":["ctx"]}],"overridden":[{"id":"kept","overridden":["ctx"]}],"unknown":[{"id":"unsynced","reason":"sync pending"}]}}`, `{"parameters":{"ctx":{"value":8192,"tier":"operator"}},"runs":{"restart_job_id":"j","stale":[{"run_id":"old","node":"worker","model":"m","changed":["ctx"]}],"overridden":[{"run_id":"kept","overridden":["ctx"]}],"unknown":[{"run_id":"unsynced","reason":"sync pending"}]}}`},
		{"zz_logs", "GET", "/zzrouter/v1/runs/r/logs", `{"run_id":"r"}`, "follow=false&lines=200", "", `{"data":{"lines":["old","last"],"total_lines":12,"count":2,"log_file":"run.log"}}`, `{"lines":["old","last"],"total_lines":12,"log_file":"run.log"}`},
		{"zz_logs", "GET", "/zzrouter/v1/runs/r/logs", `{"run_id":"r","grep":"offloaded"}`, "follow=false&lines=200&grep=offloaded", "", `{"data":{"lines":["offloaded 10/29 layers to GPU"],"total_lines":900,"count":1,"filtered":true,"log_file":"run.log"}}`, `{"lines":["offloaded 10/29 layers to GPU"],"total_lines":900,"filtered":true,"log_file":"run.log"}`},
		{"zz_logs", "GET", "/zzrouter/v1/server/logs", "{}", "limit=50&offset=0", "", `{"data":[{"instance_id":"r","size":12,"path":"drop"}],"total":1}`, `[{"run_id":"r","size":12}]`},
		{"zz_update_status", "GET", "/zzrouter/v1/update/status", "{}", "", "", `{"data":{"state":"idle","update_available":true,"current_version":"1.0"}}`, `{"state":"idle","update_available":true,"current_version":"1.0"}`},
		{"zz_update_check", "POST", "/zzrouter/v1/update/check", `{}`, "", "", `{"data":{"current_version":"1.0","update_available":false,"skipped_reason":"offline"}}`, `{"current_version":"1.0","update_available":false,"skipped_reason":"offline"}`},
		{"zz_update_apply", "POST", "/zzrouter/v1/update/apply", `{"version":"1.1","nodes":["worker"],"idempotency_key":"retry"}`, "", `{"version":"1.1","nodes":["worker"]}`, `{"data":{"job_id":"j","version":"1.1","state":"queued"}}`, `{"job_id":"j","version":"1.1","state":"queued"}`},
		{"zz_update_apply", "POST", "/zzrouter/v1/update/apply", "{}", "", "{}", `{"data":{"delegated_to":"private path","action":"apply"}}`, `{"action":"apply"}`},
	} {
		t.Run(tc.name+tc.path, func(t *testing.T) {
			var args map[string]any
			require.NoError(t, json.Unmarshal([]byte(tc.args), &args))
			calls := 0
			d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, tc.method, r.Method)
				require.Equal(t, tc.path, r.URL.Path)
				query, err := url.ParseQuery(tc.query)
				require.NoError(t, err)
				require.Equal(t, query, r.URL.Query())
				require.Equal(t, testKey, r.Header.Get("X-API-Key"))
				if key, ok := args["idempotency_key"]; ok {
					require.Equal(t, key, r.Header.Get("Idempotency-Key"))
				}
				if tc.body != "" {
					got, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.JSONEq(t, tc.body, string(got))
					ct := "application/json"
					if tc.method == "PATCH" {
						ct = "application/merge-patch+json"
					}
					require.Equal(t, ct, r.Header.Get("Content-Type"))
				}
				writeFixture(t, w, []byte(tc.response))
			})
			result := callAgentTool(t, d, tc.name, args)
			require.NotEqual(t, true, result["isError"], result)
			require.Equal(t, 1, calls)
			output := result["structuredContent"]
			if strings.HasPrefix(tc.want, "[") {
				key := map[string]string{"zz_nodes": "nodes", "zz_models": "models", "zz_model_search": "matches", "zz_model_files": "files", "zz_runs": "runs", "zz_providers": "providers", "zz_logs": "log_files"}[tc.name]
				output = output.(map[string]any)[key]
			}
			data, err := json.Marshal(output)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(data))
			content := result["content"].([]any)
			require.Len(t, content, 1)
			require.NotContains(t, content[0].(map[string]any)["text"], "\n")
		})
	}
}

func TestAgentChatSmokeBounds(t *testing.T) {
	for _, tc := range []struct {
		tokens, want int
		node         string
	}{{0, DefaultChatTokens, ""}, {5, 5, "worker"}, {99999, MaxChatTokens, ""}} {
		t.Run(fmt.Sprint(tc.tokens), func(t *testing.T) {
			d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/v1/chat/completions", r.URL.Path)
				require.Equal(t, "POST", r.Method)
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				expectedModel := "m"
				if tc.node != "" {
					expectedModel += "@" + tc.node
				}
				require.Equal(t, expectedModel, body["model"])
				require.EqualValues(t, tc.want, body["max_tokens"])
				require.Equal(t, false, body["stream"])
				require.Equal(t, []any{map[string]any{"role": "user", "content": "say hello"}}, body["messages"])
				writeFixture(t, w, []byte(`{"model":"m","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4},"drop":"extra"}`))
			})
			result := callAgentTool(t, d, "zz_chat", map[string]any{"model": "m", "message": "say hello", "max_tokens": tc.tokens, "node": tc.node})
			require.NotEqual(t, true, result["isError"])
			data, err := json.Marshal(result["structuredContent"])
			require.NoError(t, err)
			require.JSONEq(t, `{"reply":"hello","model":"m","usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`, string(data))
		})
	}
}

func TestAgentListBoundsAndPaging(t *testing.T) {
	for _, tool := range []string{"zz_nodes", "zz_runs", "zz_models", "zz_model_search", "zz_providers", "zz_logs"} {
		t.Run(tool, func(t *testing.T) {
			d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
				if tool == "zz_model_search" {
					writeFixture(t, w, []byte(`{"data":{"results":[{"id":"a"},{"id":"b"},{"id":"c"}],"total":3}}`))
					return
				}
				writeFixture(t, w, []byte(`{"data":[{"name":"a"},{"name":"b"},{"name":"c"}],"total":3}`))
			})
			args := map[string]any{"limit": 1}
			if tool == "zz_model_search" {
				args["query"] = "tiny"
			}
			result := callAgentTool(t, d, tool, args)["structuredContent"].(map[string]any)
			key := map[string]string{"zz_nodes": "nodes", "zz_models": "models", "zz_model_search": "matches", "zz_model_files": "files", "zz_runs": "runs", "zz_providers": "providers", "zz_logs": "log_files"}[tool]
			require.Len(t, result[key], 1)
			require.Equal(t, true, result["has_more"])
			if tool == "zz_nodes" || tool == "zz_runs" {
				args["offset"] = 99
				last := callAgentTool(t, d, tool, args)["structuredContent"].(map[string]any)
				require.Empty(t, last[key])
				require.EqualValues(t, 3, last["total"])
				require.Equal(t, false, last["has_more"])
			}
		})
	}
}

func TestAgentInvalidInputsNeverDispatch(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"zz_nodes", `{"node":"../health"}`}, {"zz_runs", `{"run_id":"a/b"}`},
		{"zz_run_stop", `{"run_id":"r?follow=true"}`}, {"zz_run_restart", `{"run_id":".."}`},
		{"zz_job_cancel", `{"job_id":"a","deployment_id":"b"}`}, {"zz_job_cancel", "{}"},
		{"zz_job_cancel", `{"deployment_id":"d","node":"worker"}`}, {"zz_job_cancel", `{"job_id":"%2f"}`},
		{"zz_providers", `{"provider":"vllm/logs"}`},
		{"zz_provider_install", `{"provider":".."}`}, {"zz_params_get", `{"provider":"vllm#"}`},
		{"zz_params_set", `{"provider":"vllm","patch":null}`}, {"zz_models", `{"limit":-1}`},
		{"zz_models", `{"limit":1001}`}, {"zz_logs", `{"run_id":"r","lines":-1}`},
		{"zz_chat", `{"model":"m","message":"hi","max_tokens":-1}`},
		{"zz_update_status", `{"check":true,"nodes":["worker"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var args map[string]any
			require.NoError(t, json.Unmarshal([]byte(tc.args), &args))
			d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid input dispatched") })
			require.Equal(t, true, callAgentTool(t, d, tc.name, args)["isError"])
		})
	}
}

func TestAgentLogCap(t *testing.T) {
	d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "false", r.URL.Query().Get("follow"))
		require.Equal(t, "1000", r.URL.Query().Get("lines"))
		writeFixture(t, w, []byte(`{"data":{"lines":[]}}`))
	})
	_, err := d.logs(context.Background(), logsInput{RunID: "r", Lines: 99999})
	require.NoError(t, err)
}

func TestAgentNestedProjections(t *testing.T) {
	t.Run("node providers", func(t *testing.T) {
		d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
			writeFixture(t, w, []byte(`{"data":[{"name":"worker","providers":[{"key":"engine","name":"Engine","type":"service","running":true,"service":{"command":"drop","arguments":["huge"]}}]}],"total":1}`))
		})
		result := callAgentTool(t, d, "zz_nodes", map[string]any{})
		data, err := json.Marshal(result["structuredContent"].(map[string]any)["nodes"])
		require.NoError(t, err)
		require.JSONEq(t, `[{"name":"worker","providers":[{"key":"engine","name":"Engine","type":"service","running":true}]}]`, string(data))
	})
	t.Run("selected update nodes", func(t *testing.T) {
		d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "worker", r.URL.Query().Get("nodes"))
			writeFixture(t, w, []byte(`{"data":{"nodes":[{"node":"worker","status":{"current_version":"1.0","latest_release":{"tag_name":"v1.1","body":"drop","assets":[{"drop":"huge"}]},"skipped_reason":"offline","pending_release":{"drop":"huge"}}}],"rollouts":[{"drop":"huge"}]}}`))
		})
		result := callAgentTool(t, d, "zz_update_status", map[string]any{"nodes": []string{"worker"}})
		data, err := json.Marshal(result["structuredContent"])
		require.NoError(t, err)
		require.JSONEq(t, `{"nodes":[{"node":"worker","status":{"current_version":"1.0","latest_release":{"tag_name":"v1.1"},"skipped_reason":"offline"}}]}`, string(data))
	})
}

func TestAgentSchemaDescriptions(t *testing.T) {
	d := testDispatcher(nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	r := httptest.NewRequest("POST", "http://coordinator/zzrouter/v1/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	r.Header.Set("Mcp-Method", "tools/list")
	w := httptest.NewRecorder()
	New(d.cfg).ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response struct {
		Result struct {
			Tools []struct {
				Name, Description string
				InputSchema       struct {
					Properties map[string]struct{ Description string }
				}
			}
		}
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	descriptions := map[string]string{}
	for _, tool := range response.Result.Tools {
		descriptions[tool.Name] = tool.Description
		require.NotEmpty(t, tool.Description)
		require.NotContains(t, tool.Description, "—")
		for field, schema := range tool.InputSchema.Properties {
			require.NotEmpty(t, schema.Description, tool.Name+"."+field)
		}
	}
	for _, name := range []string{"zz_run_preview", "zz_run_start"} {
		require.Contains(t, descriptions[name], "Check zz_models first")
		require.Contains(t, descriptions[name], "absent")
	}
	for _, phrase := range []string{"source_id", "case-insensitively", "exact (QWEN2.5 matches qwen2.5)", "final /", "QWEN2.5 matches org/qwen2.5", "*qwen2.5* matches org/qwen2.5-instruct", "does not suppress suffix hits"} {
		require.Contains(t, descriptions["zz_models"], phrase)
	}
	require.Contains(t, descriptions["zz_run_stop"], "confirms the stop")
	require.Contains(t, descriptions["zz_run_stop"], "404 by design")
	require.Contains(t, descriptions["zz_run_start"], "job_id for zz_job")
	require.Contains(t, descriptions["zz_job"], "Pass job_id here, not run_id")
	require.Less(t, strings.Index(instructions, "Check zz_models first"), strings.Index(instructions, "zz_model_search"))

}

func TestProviderSummaryUsesReportedState(t *testing.T) {
	for _, tc := range []struct {
		installed, enabled bool
	}{
		{true, true},
		{true, false},
		{false, false},
		{false, true},
	} {
		t.Run(fmt.Sprintf("%t/%t", tc.installed, tc.enabled), func(t *testing.T) {
			d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"installed": tc.installed, "enabled": tc.enabled}}))
			})
			result := callAgentTool(t, d, "zz_providers", map[string]any{"provider": "engine"})
			summary := result["content"].([]any)[0].(map[string]any)["text"].(string)
			require.Equal(t, fmt.Sprintf("Provider status: installed=%t, enabled=%t.", tc.installed, tc.enabled), summary)
			if tc.installed {
				require.NotContains(t, summary, "prerequisites")
				require.NotContains(t, summary, "zz_provider_install")
			}
		})
	}
}

func TestModelProjectionKeepsMissingEvidenceMissing(t *testing.T) {
	d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
		writeFixture(t, w, []byte(`{"data":[{"name":"embedding-looking-name"},{"name":"other","details":{"format":"gguf"}}],"total":2}`))
	})
	result := callAgentTool(t, d, "zz_models", map[string]any{})["structuredContent"].(map[string]any)
	raw, err := json.Marshal(result["models"])
	require.NoError(t, err)
	require.JSONEq(t, `[{"name":"embedding-looking-name"},{"name":"other","details":{}}]`, string(raw))
}

// zz_params_get answers what is set and what can be set, so an agent never
// searches the spec for a parameter name.
func TestParamsGetIncludesAccepts(t *testing.T) {
	var paths []string
	d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/zzrouter/v1/providers/llamacpp/resolved":
			require.Equal(t, "worker", r.URL.Query().Get("node"))
			require.Equal(t, "org/model", r.URL.Query().Get("model"))
			writeFixture(t, w, []byte(`{"parameters":{"ctx-size":{"value":"4096","tier":"default"}},"install":{"drop":"huge"}}`))
		case "/zzrouter/v1/providers/llamacpp/schema":
			require.Empty(t, r.URL.RawQuery, "the schema route takes no selector")
			writeFixture(t, w, []byte(`{"data":{"parameters":{"n-gpu-layers":{"type":"int","description":"Number of layers to offload to GPU"}},"diagnostics":{"drop":1}}}`))
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	})
	result := callAgentTool(t, d, "zz_params_get", map[string]any{"provider": "llamacpp", "node": "worker", "model": "org/model"})
	require.NotEqual(t, true, result["isError"], result)
	require.Equal(t, []string{"/zzrouter/v1/providers/llamacpp/resolved", "/zzrouter/v1/providers/llamacpp/schema"}, paths)
	data, err := json.Marshal(result["structuredContent"])
	require.NoError(t, err)
	require.JSONEq(t, `{"parameters":{"ctx-size":{"value":"4096","tier":"default"}},"accepts":{"n-gpu-layers":{"type":"int","description":"Number of layers to offload to GPU"}}}`, string(data))
}
