// MCP (Model Context Protocol) transport gateway wiring.
//
// Each configured server is mounted at /mcp/{name}/* and proxied
// end-to-end. Pure protocol logic (stdio bridging, config
// validation) lives in pkg/protocol/mcp; this file binds that
// package to the HTTP server and its routing primitives.
//
// Scope: transport only. No tool registry, no per-key ACLs, no
// OpenAPI→MCP conversion.
package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/protocol/mcp"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stperic/zzrouter/pkg/utils"
)

// mcpMount is the per-request handle the gateway dispatches to.
type mcpMount struct {
	srv   *mcp.Server
	stdio *mcp.StdioBridge // nil until first stdio request
}

// MCPGateway owns all MCP stdio state and request handling.
type MCPGateway struct {
	mu              sync.Mutex
	mounts          []*mcpMount
	shutdown        bool
	readBody        func(*gin.Context) ([]byte, bool)
	proxyToBackend  func(http.ResponseWriter, *http.Request, string, string, backend.Upstream, []byte)
	openaiResponder *openaiproto.Responder
}

// registerMCPRoutes wires every configured MCP mount onto the engine.
func (s *Server) registerMCPRoutes() int {
	servers := s.config.MCP.Servers
	if len(servers) == 0 {
		return 0
	}

	gw := &MCPGateway{
		readBody:        s.readPassthroughBody,
		proxyToBackend:  s.proxy.ForwardToBackend,
		openaiResponder: s.responders.openai,
	}
	s.mcpGateway = gw

	openaiResponder := s.responders.openai
	count := 0
	for name, raw := range servers {
		name = strings.Trim(name, "/")
		if name == "" || strings.ContainsRune(name, '/') {
			utils.LogDebugf("[Routes] Skipping MCP mount %q (invalid name)", name)
			continue
		}
		protoSrv, err := mcp.Build(name, raw)
		if err != nil {
			utils.LogDebugf("[Routes] Skipping MCP mount %q: %v", name, err)
			continue
		}
		mount := &mcpMount{srv: protoSrv}
		gw.registerForCleanup(mount)

		group := s.engine.Group("/mcp/"+name,
			httperr.AttachResponder(openaiResponder),
			s.auth.OptionalAuthMiddleware(),
		)
		surface := "/mcp/" + name
		group.Any("/*path", func(c *gin.Context) {
			if !s.admitOpaqueForward(c, surface, mount.srv.Upstream.API() != nil) {
				return
			}
			release, ok := s.enforceAndAttribute(c, "mcp "+surface, nil)
			if !ok {
				return
			}
			defer release()
			gw.handleTransport(c, mount)
		})
		count++
	}

	utils.LogDebugf("[Routes] Registered MCP mounts: %d", count)
	return count
}

// registerForCleanup tracks the mount so shutdown can close stdio children.
func (gw *MCPGateway) registerForCleanup(mount *mcpMount) {
	if mount == nil || mount.srv == nil || mount.srv.Transport != mcp.TransportStdio {
		return
	}
	gw.mu.Lock()
	gw.mounts = append(gw.mounts, mount)
	gw.mu.Unlock()
}

func (gw *MCPGateway) handleTransport(c *gin.Context, mount *mcpMount) {
	if mount.srv.Transport == mcp.TransportStdio {
		gw.handleStdio(c, mount)
		return
	}
	gw.handleHTTP(c, mount)
}

// handleHTTP forwards the request to the configured remote MCP endpoint.
func (gw *MCPGateway) handleHTTP(c *gin.Context, mount *mcpMount) {
	body, bodyOK := gw.readBody(c)
	if !bodyOK {
		return
	}
	suffix := c.Param("path")
	if suffix == "" {
		suffix = "/"
	}
	target := backend.PassthroughTarget(mount.srv.URL, suffix, c.Request.URL.RawQuery)
	gw.proxyToBackend(c.Writer, c.Request, "mcp-"+mount.srv.Name, target, mount.srv.Upstream, body)
}

func (gw *MCPGateway) handleStdio(c *gin.Context, mount *mcpMount) {
	bridge, err := gw.getOrStartBridge(mount)
	if err != nil {
		gw.openaiResponder.BadGateway(c, "mcp stdio spawn failed: "+err.Error())
		return
	}

	if c.Request.Method != http.MethodPost {
		c.JSON(http.StatusOK, gin.H{
			"object":  "mcp_stdio",
			"message": "stdio transport accepts POST JSON-RPC requests only",
		})
		return
	}

	body, bodyOK := gw.readBody(c)
	if !bodyOK {
		return
	}

	resp, err := bridge.Exchange(c.Request.Context(), body)
	if err != nil {
		gw.openaiResponder.BadGateway(c, "mcp stdio exchange failed: "+err.Error())
		return
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(c.Writer, strings.NewReader(resp))
}

// getOrStartBridge returns the running bridge for a mount, spawning it on first use.
func (gw *MCPGateway) getOrStartBridge(mount *mcpMount) (*mcp.StdioBridge, error) {
	gw.mu.Lock()
	defer gw.mu.Unlock()

	if gw.shutdown {
		return nil, errors.New("mcp: gateway is shutting down")
	}
	if mount.stdio != nil && mount.stdio.Alive() {
		return mount.stdio, nil
	}
	bridge, err := mcp.StartStdioBridge(mount.srv.Name, mount.srv.Command, mount.srv.Args, mount.srv.Env)
	if err != nil {
		return nil, err
	}
	mount.stdio = bridge
	return bridge, nil
}

// Start is a no-op — bridges are created on demand.
// Present for Start/Stop symmetry with the server lifecycle orchestrator.
func (gw *MCPGateway) Start(_ context.Context) {}

// Stop closes every registered stdio bridge. Called from server graceful shutdown.
func (gw *MCPGateway) Stop() {
	if gw == nil {
		return
	}
	gw.mu.Lock()
	gw.shutdown = true
	mounts := gw.mounts
	gw.mounts = nil
	bridges := make([]*mcp.StdioBridge, 0, len(mounts))
	for _, m := range mounts {
		if m != nil && m.stdio != nil {
			bridges = append(bridges, m.stdio)
			m.stdio = nil
		}
	}
	gw.mu.Unlock()

	for _, b := range bridges {
		_ = b.Close()
	}
}
