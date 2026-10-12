package server

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/internal/server/mcpserver"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

// Synthetic REST replies isolate exact-match enrichment through the real MCP wire.
func TestMCPModelDetailSelectionContract(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-06-18"} {
		for _, tc := range []struct {
			name, filter string
			total        int
			hasMore      bool
			show         bool
		}{
			{"name", "M", 1, false, true},
			{"source id", "ORG/WEIGHTS", 1, false, true},
			{"suffix", "weights", 1, false, false},
			{"glob", "*m*", 1, false, false},
			{"literal glob name", "m[1]", 1, false, false},
			{"one-entry page", "m", 2, true, false},
			{"duplicate replicas", "m", 2, false, false},
			{"empty filter", "", 1, false, false},
		} {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				engine := gin.New()
				shows := 0
				catalogName := "m"
				if tc.name == "literal glob name" {
					catalogName = tc.filter
				}
				engine.GET("/zzrouter/v1/models", func(c *gin.Context) {
					require.Equal(t, tc.filter, c.Query("model"))
					c.JSON(200, gin.H{"data": []any{gin.H{"name": catalogName, "source_id": "org/weights", "source_repo": "huggingface", "node": "worker", "assigned_app": "llamacpp", "details": gin.H{"wire_endpoints": []string{"chat_completions"}, "features": gin.H{"vision": true}}}}, "total": tc.total, "has_more": tc.hasMore})
				})
				engine.GET("/zzrouter/v1/models/show", func(c *gin.Context) {
					shows++
					require.Equal(t, "true", c.Query("compact"))
					require.Equal(t, "worker", c.Query("node"))
					require.Equal(t, "llamacpp", c.Query("provider"))
					require.Equal(t, "huggingface", c.Query("registry"))
					c.JSON(200, gin.H{"data": gin.H{"name": "m", "model_info": gin.H{"max_position_embeddings": 8192}, "template_present": true, "parameters": "temperature 0.6", "capabilities": []string{"completion"}}})
				})
				engine.POST("/zzrouter/v1/mcp", gin.WrapH(mcpserver.New(mcpserver.Config{Engine: engine, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath, SnapshotAllowed: snapshotAllowed})))
				result := mcpWireResponse(t, &Server{engine: engine}, mcpWireRequest(t, "tools/call", version, map[string]any{"name": "zz_models", "arguments": map[string]any{"model": tc.filter, "limit": 1}}), 200)["result"].(map[string]any)
				require.NotEqual(t, true, result["isError"], result)
				fields, ok := result["structuredContent"].(map[string]any)
				require.True(t, ok, result)
				entry := fields["models"].([]any)[0].(map[string]any)
				require.Contains(t, entry["details"], "wire_endpoints")
				require.Contains(t, entry["details"], "features")
				if tc.show {
					require.Equal(t, 1, shows)
					require.Equal(t, true, entry["show"].(map[string]any)["template_present"])
					require.Contains(t, entry["show"], "model_info")
				} else {
					require.Zero(t, shows)
					require.NotContains(t, entry, "show")
				}
			})
		}
	}
}

func TestMCPModelDetailFailureContract(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-06-18"} {
		for _, tc := range []struct {
			code   string
			status int
			retain bool
		}{
			{"model_detail_unsupported", 501, true},
			{"not_implemented", 501, false},
			{"model_detail_unsupported", 502, false},
			{"unauthorized", 401, false},
			{"cluster_unavailable", 503, false},
			{"upstream_shape", 502, false},
		} {
			t.Run(version+"/"+tc.code+http.StatusText(tc.status), func(t *testing.T) {
				engine := gin.New()
				engine.GET("/zzrouter/v1/models", func(c *gin.Context) {
					c.JSON(200, gin.H{"data": []any{gin.H{"name": "m", "assigned_app": "p", "node": "worker"}}, "total": 1})
				})
				engine.GET("/zzrouter/v1/models/show", func(c *gin.Context) {
					problem := utils.NewProblemDetails(tc.status, http.StatusText(tc.status), "synthetic detail refusal", c.Request.URL.Path)
					problem.Code, problem.Node = tc.code, "worker"
					c.Header("Content-Type", "application/problem+json")
					c.JSON(tc.status, problem)
				})
				engine.POST("/zzrouter/v1/mcp", gin.WrapH(mcpserver.New(mcpserver.Config{Engine: engine, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath, SnapshotAllowed: snapshotAllowed})))
				result := mcpWireResponse(t, &Server{engine: engine}, mcpWireRequest(t, "tools/call", version, map[string]any{"name": "zz_models", "arguments": map[string]any{"model": "m"}}), 200)["result"].(map[string]any)
				fields, ok := result["structuredContent"].(map[string]any)
				require.True(t, ok, result)
				if tc.retain {
					require.NotEqual(t, true, result["isError"], result)
					entry := fields["models"].([]any)[0].(map[string]any)
					require.NotContains(t, entry, "show")
					require.Contains(t, entry, "show_unavailable")
					require.Equal(t, tc.code, entry["show_unavailable"].(map[string]any)["code"])
				} else {
					require.Equal(t, true, result["isError"], result)
					require.Equal(t, tc.code, fields["code"])
					require.EqualValues(t, tc.status, fields["status"])
					require.Equal(t, "worker", fields["node"])
				}
			})
		}
	}
}
