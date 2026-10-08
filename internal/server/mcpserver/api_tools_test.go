package mcpserver

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIPathGuardNeverDispatches(t *testing.T) {
	paths := []string{
		"/v1/models",
		"/zzrouter/v1/../health", "/zzrouter/v1/models/..", "/zzrouter/v1//models", `/zzrouter/v1/models\child`,
		"/zzrouter/v1/%2finternal/version", "/zzrouter/v1/%2Finternal/version", "/zzrouter/v1/%2e%2e/health", "/zzrouter/v1/%2E%2E/health",
		"/zzrouter/v1/models%5cother", "/zzrouter/v1/models%5Cother", "/zzrouter/v1/%252e%252e/health", "/zzrouter/v1/%252Finternal",
		"/zzrouter/v1/models?node=worker", "/zzrouter/v1/models#fragment", "https://remote/zzrouter/v1/models",
	}
	for _, name := range []string{"zz_api_get", "zz_api"} {
		for _, path := range paths {
			t.Run(name+path, func(t *testing.T) {
				d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("unsafe path dispatched") })
				d.cfg.EligibleAPIRoute = func(string, string) error { t.Fatal("unsafe path reached catalog"); return nil }
				args := map[string]any{"path": path}
				if name == "zz_api" {
					args["method"] = "POST"
				}
				require.Equal(t, true, callAgentTool(t, d, name, args)["isError"])
			})
		}
	}
}

func TestAPISurfaceRefusalsComeFromCatalog(t *testing.T) {
	for _, path := range []string{"/zzrouter/v1/internal", "/zzrouter/v1/internal/version", "/zzrouter/v1/mcp", "/zzrouter/v1/mcp/child"} {
		for _, name := range []string{"zz_api_get", "zz_api"} {
			t.Run(name+path, func(t *testing.T) {
				d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("catalog-refused path dispatched") })
				called := false
				d.cfg.EligibleAPIRoute = func(method, actual string) error {
					called = true
					require.Equal(t, path, actual)
					require.Contains(t, []string{"GET", "POST"}, method)
					return fmt.Errorf("catalog refused surface")
				}
				args := map[string]any{"path": path}
				if name == "zz_api" {
					args["method"] = "POST"
				}
				result := callAgentTool(t, d, name, args)
				require.True(t, called, "surface decisions belong to the catalog")
				require.Equal(t, true, result["isError"])
				require.Contains(t, result["content"].([]any)[0].(map[string]any)["text"], "catalog refused surface")
			})
		}
	}
}

func TestAPIMethodAndCatalogRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"zz_api", map[string]any{"path": "/zzrouter/v1/models", "method": "GET"}},
		{"zz_api_get", map[string]any{"path": "/zzrouter/v1/models", "method": "POST"}},
		{"zz_api_get", map[string]any{"path": "/zzrouter/v1/jobs/j/stream"}},
	} {
		d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("refused call dispatched") })
		d.cfg.EligibleAPIRoute = func(string, string) error { return fmt.Errorf("catalog refuses route") }
		require.Equal(t, true, callAgentTool(t, d, tc.name, tc.args)["isError"])
	}
}

func TestAPIJSONRequestsUseDispatcher(t *testing.T) {
	for _, tc := range []struct {
		contentType, body string
		value             any
	}{
		{"application/merge-patch+json", `{"defaults":{"parameters":{"old":null}}}`, map[string]any{"defaults": map[string]any{"parameters": map[string]any{"old": nil}}}},
		{"application/json", "null", nil},
		{"application/json", "true", true},
		{"application/json", "1.5", 1.5},
		{"application/json", `"value"`, "value"},
		{"application/json", `[1,true,"value"]`, []any{1, true, "value"}},
	} {
		t.Run(tc.body, func(t *testing.T) {
			d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPatch, r.Method)
				require.Equal(t, "/zzrouter/v1/providers/llamacpp/parameters", r.URL.Path)
				require.Equal(t, "worker", r.URL.Query().Get("node"))
				require.Equal(t, tc.contentType, r.Header.Get("Content-Type"))
				require.Equal(t, "retry", r.Header.Get("Idempotency-Key"))
				require.Equal(t, "revision", r.Header.Get("If-Match"))
				require.Equal(t, testKey, r.Header.Get("X-API-Key"))
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.JSONEq(t, tc.body, string(body))
				writeFixture(t, w, []byte(`{"runs":{"restart_job_id":"j"}}`))
			})
			d.cfg.EligibleAPIRoute = func(method, path string) error {
				require.Equal(t, "PATCH", method)
				require.Equal(t, "/zzrouter/v1/providers/llamacpp/parameters", path)
				return nil
			}
			args := map[string]any{"method": "PATCH", "path": "/zzrouter/v1/providers/llamacpp/parameters", "query": map[string]string{"node": "worker"}, "body": tc.value, "idempotency_key": "retry", "if_match": "revision"}
			if tc.contentType != "application/json" {
				args["content_type"] = tc.contentType
			}
			result := callAgentTool(t, d, "zz_api", args)
			require.NotEqual(t, true, result["isError"], result)
			require.Equal(t, map[string]any{"response": map[string]any{"runs": map[string]any{"restart_job_id": "j"}}}, result["structuredContent"])
		})
	}
}
