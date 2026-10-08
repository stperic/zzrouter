package server

import (
	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/internal/server/mcpserver"
)

func (s *Server) registerMCPServer(router *gin.RouterGroup) {
	handler := mcpserver.New(mcpserver.Config{Engine: s.engine, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath})
	serve := func(c *gin.Context) {
		if c.Request.Context().Value(internalRequestKey) != nil {
			Forbidden(c, "MCP cannot be called from a trusted internal request; use the public endpoint with an admin key")
			return
		}
		handler.ServeHTTP(c.Writer, c.Request)
	}
	router.POST("/mcp", serve)
	router.GET("/mcp", serve)
	router.DELETE("/mcp", serve)
}

// skipMCPAccounting leaves the outer HTTP request as the accounting unit.
func skipMCPAccounting(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if mcpserver.IsDispatch(c.Request.Context()) {
			c.Next()
			return
		}
		next(c)
	}
}
