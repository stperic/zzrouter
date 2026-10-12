package server

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/internal/server/mcpserver"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

// Synthetic inference replies isolate task dispatch through the real gin engine.
func TestMCPProbeDispatchContract(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-06-18"} {
		for _, probe := range []string{"chat", "tools", "embed", "vision"} {
			t.Run(version+"/"+probe, func(t *testing.T) {
				engine := gin.New()
				calls := 0
				handler := func(c *gin.Context) {
					calls++
					require.Equal(t, TestAdminKey, c.GetHeader("X-API-Key"))
					var body map[string]any
					require.NoError(t, c.ShouldBindJSON(&body))
					require.Equal(t, "m@worker", body["model"])
					if probe == "embed" {
						require.Equal(t, "/v1/embeddings", c.Request.URL.Path)
						c.JSON(200, gin.H{"model": "m", "data": []any{gin.H{"embedding": []float64{1, 2, 3}}}})
						return
					}
					require.Equal(t, "/v1/chat/completions", c.Request.URL.Path)
					message := gin.H{"content": "hello"}
					if probe == "tools" {
						message["tool_calls"] = []any{gin.H{"function": gin.H{"name": "lookup_weather", "arguments": `{"city":"Paris"}`}}}
					}
					if probe == "vision" {
						message["content"] = "red"
					}
					c.JSON(200, gin.H{"model": "m", "choices": []any{gin.H{"message": message}}})
				}
				engine.POST("/v1/chat/completions", handler)
				engine.POST("/v1/embeddings", handler)
				engine.POST("/zzrouter/v1/mcp", gin.WrapH(mcpserver.New(mcpserver.Config{Engine: engine, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath, SnapshotAllowed: snapshotAllowed})))
				result := mcpWireResponse(t, &Server{engine: engine}, mcpWireRequest(t, "tools/call", version, map[string]any{"name": "zz_chat", "arguments": map[string]any{"model": "m", "node": "worker", "message": "hello", "probe": probe}}), http.StatusOK)["result"].(map[string]any)
				require.NotEqual(t, true, result["isError"], result)
				fields, ok := result["structuredContent"].(map[string]any)
				require.True(t, ok, result)
				want := "verified"
				if probe == "vision" {
					want = "inconclusive"
					require.Equal(t, 2, calls)
				} else {
					require.Equal(t, 1, calls)
				}
				require.Equal(t, want, fields["verdict"])
				require.Equal(t, probe, fields["probe"])
			})
		}
	}
}

func TestMCPProbeRefusalWireContract(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-06-18"} {
		t.Run(version, func(t *testing.T) {
			engine := gin.New()
			problem := utils.NewProblemDetails(400, "Unsupported Input", "synthetic typed refusal", "/v1/chat/completions")
			problem.Code, problem.Node, problem.RequestID = "unsupported_input", "worker", "inner-1"
			problem.Errors = []utils.ParamError{{Key: "images", Code: "unsupported_input", Want: "supported input"}}
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Header("Content-Type", "application/problem+json")
				c.JSON(400, problem)
			})
			engine.POST("/zzrouter/v1/mcp", gin.WrapH(mcpserver.New(mcpserver.Config{Engine: engine, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath, SnapshotAllowed: snapshotAllowed})))
			result := mcpWireResponse(t, &Server{engine: engine}, mcpWireRequest(t, "tools/call", version, map[string]any{"name": "zz_chat", "arguments": map[string]any{"model": "m", "probe": "vision"}}), http.StatusOK)["result"].(map[string]any)
			require.Equal(t, true, result["isError"])
			fields, ok := result["structuredContent"].(map[string]any)
			require.True(t, ok, result)
			require.Equal(t, "not_supported", fields["verdict"])
			require.Equal(t, "vision", fields["probe"])
			require.Equal(t, problem.Type, fields["type"])
			require.EqualValues(t, problem.Status, fields["status"])
			require.Equal(t, problem.Code, fields["code"])
			require.Equal(t, problem.Node, fields["node"])
			require.Equal(t, problem.RequestID, fields["request_id"])
			require.Equal(t, problem.Instance, fields["instance"])
			require.Len(t, fields["errors"], 1)
		})
	}
}
