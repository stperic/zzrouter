package server

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/internal/server/mcpserver"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationOperatorSchemaMCP(t *testing.T) {
	e, _ := operatorExecutor(t)
	engine := gin.New()
	engine.GET("/zzrouter/v1/providers/:name/resolved", e.HandleResolved)
	engine.GET("/zzrouter/v1/providers/:name/schema", e.HandleSchema)
	engine.PATCH("/zzrouter/v1/providers/:name/parameters", e.HandleParametersPatch)
	engine.POST("/zzrouter/v1/mcp", gin.WrapH(mcpserver.New(mcpserver.Config{Engine: engine, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath, SnapshotAllowed: snapshotAllowed})))
	s := &Server{engine: engine}
	result := callMCPWireTool(t, s, "zz_params_set", map[string]any{"provider": "ollama", "patch": map[string]any{
		"schema":   map[string]any{"future_flag": map[string]any{"type": "int", "description": "experimental tuning"}},
		"defaults": map[string]any{"parameters": map[string]any{"future-flag": 2}},
	}})
	require.NotEqual(t, true, result["isError"], result)
	require.NoError(t, e.configStore.ApplyParameterPatch("ollama", func(sc *config.ServiceConfig) error {
		sc.Schema["bad"] = config.ParameterDeclaration{Type: new("invented")}
		return nil
	}))
	result = callMCPWireTool(t, s, "zz_params_get", map[string]any{"provider": "ollama", "grep": "experimental"})
	require.NotEqual(t, true, result["isError"], result)
	fields := result["structuredContent"].(map[string]any)
	assert.Equal(t, true, fields["accepts"].(map[string]any)["future-flag"].(map[string]any)["operator"])
	assert.Equal(t, "schema.bad", fields["problems"].([]any)[0].(map[string]any)["key"])
}
