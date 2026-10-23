package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/internal/server/mcpserver"
)

func (s *Server) registerMCPServer(router *gin.RouterGroup) {
	handler := mcpserver.New(mcpserver.Config{Engine: s.engine, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath, SnapshotAllowed: snapshotAllowed, SearchAPI: searchOpenAPI, EligibleAPIRoute: s.eligibleAPIRoute})
	s.mountMCP(router, "/mcp", handler)
}

func (s *Server) registerInferenceMCPServer() {
	handler := mcpserver.NewInference(mcpserver.Config{Engine: s.engine, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath, SnapshotAllowed: snapshotAllowed})
	router := s.engine.Group("/inference", RequestIDMiddleware(), APIVersionMiddleware(), s.auth.AuthMiddleware(RoleUser))
	s.mountMCP(router, "/mcp", handler)
}

func (s *Server) mountMCP(router *gin.RouterGroup, path string, handler http.Handler) {
	serve := func(c *gin.Context) {
		if c.Request.Context().Value(internalRequestKey) != nil {
			Forbidden(c, "MCP cannot be called from a trusted internal request; use the public endpoint with an API key")
			return
		}
		handler.ServeHTTP(c.Writer, c.Request)
	}
	key := s.canonicalMCPKey()
	router.POST(path, key, serve)
	router.GET(path, key, serve)
	router.DELETE(path, key, serve)
}

// Forward the credential accepted at ingress, since management and inference
// APIs otherwise select different headers when a client sends both schemes.
func (s *Server) canonicalMCPKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := extractCredential(c, s.config).key
		for _, name := range nodeAPIKeyHeaders {
			c.Request.Header.Del(name)
		}
		c.Request.Header.Set("X-API-Key", key)
		c.Next()
	}
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
