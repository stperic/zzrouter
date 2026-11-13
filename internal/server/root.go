package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/version"
)

// ollamaRootProbeBody is a compatibility handshake, not application copy.
const ollamaRootProbeBody = "Ollama is running"

func (s *Server) registerRootRoutes() {
	s.engine.GET("/", s.handleRoot)
	s.engine.HEAD("/", s.handleRoot)
}

func (s *Server) handleRoot(c *gin.Context) {
	c.Writer.Header().Add("Vary", "Accept")
	c.Header("Cache-Control", "no-store")
	if !clientAcceptsJSON(c) {
		if c.NegotiateFormat("text/plain", "text/html") != "text/html" {
			c.String(http.StatusOK, ollamaRootProbeBody)
			return
		}
		if s.access.oauth != nil {
			s.browserCoordinatorOnly(c)
			if !c.IsAborted() {
				c.Redirect(http.StatusTemporaryRedirect, "/console/")
			}
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"service":     "zzrouter",
		"version":     version.Current.String(),
		"api_version": "v1",
		"links": gin.H{
			"health":    "/health",
			"discovery": apiDiscoveryPrefix,
			"openapi":   "/openapi.json",
		},
	})
}

// An explicit JSON media type retains precedence over wildcard and text offers.
func clientAcceptsJSON(c *gin.Context) bool {
	return strings.Contains(c.GetHeader("Accept"), "application/json")
}
