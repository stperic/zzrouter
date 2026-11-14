package server

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/internal/server/mcpserver"
	"github.com/stperic/zzrouter/pkg/observability"
	"github.com/stperic/zzrouter/pkg/utils"
)

// observeClient wraps only authenticated external traffic. Console polling and
// nested MCP/cluster dispatch must not create clients or inflate request counts.
func (a *AuthHandlers) observeClient(c *gin.Context) {
	_, browser := browserBinding(c.Request.Context())
	trusted, _ := c.Get(string(CtxKeyClusterTrusted))
	ac := GetAccessContext(c)
	if a.clients == nil || browser || trusted == true || c.Request.Context().Value(internalRequestKey) != nil ||
		mcpserver.IsDispatch(c.Request.Context()) || ac == nil || ac.Key == nil || ac.Key.ID == "" || c.Request.URL.Path == "/zzrouter/v1/clients" {
		c.Next()
		return
	}
	protocol := "inference-api"
	switch path := c.Request.URL.Path; {
	case path == adminMCPPath:
		protocol = "management-mcp"
	case path == inferenceMCPPath:
		protocol = "inference-mcp"
	case strings.HasPrefix(path, "/zzrouter/v1/"):
		protocol = "management-api"
	}
	source, authentication := "startup", "api-key"
	if ac.Key.IsVirtual {
		source = "managed"
	}
	if isOAuthRequest(c.Request.Context()) {
		authentication = "oauth"
	}
	finish := a.clients.Start(observability.ClientIdentity{
		KeyID: ac.Key.ID, Name: ac.Key.Name, Source: source, Protocol: protocol, Authentication: authentication,
	}, utils.NowUTC())
	defer func() { finish(utils.NowUTC()) }()
	c.Next()
}

func (s *Server) handleClientActivity(c *gin.Context) {
	if s.auth.clients == nil {
		ServiceUnavailable(c, "Client activity observation is unavailable")
		return
	}
	rows, since, truncated := s.auth.clients.Snapshot(utils.NowUTC())
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Cookie, Authorization, X-API-Key")
	respondListWithMetadata(c, rows, len(rows), false, map[string]any{
		"observed_since": since, "retention_seconds": observability.ClientActivityRetention.Seconds(),
		"truncated": truncated, "grouped_by": "key_source_key_id_protocol_authentication",
	})
}

func (s *Server) registerClientActivity(router *gin.RouterGroup) {
	router.GET("/clients", s.browserCoordinatorOnly, s.handleClientActivity)
}

// registerBrowserPairing opts in only enrollment, not decommission or revocation.
func (s *Server) registerBrowserPairing() {
	s.engine.POST("/zzrouter/v1/cluster/pairing/accept", RequestIDMiddleware(), APIVersionMiddleware(),
		s.browserCoordinatorOnly, browserResponseHeaders, s.auth.browserAuthMiddleware(RoleAdmin),
		s.clusterHandlers.handleClusterPairingAccept)
}
