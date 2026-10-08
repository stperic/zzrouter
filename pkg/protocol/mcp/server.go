package mcp

import (
	"errors"
	"fmt"
	"strings"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
)

// Transport identifies the wire transport used to reach an MCP server.
type Transport string

const (
	TransportHTTP  Transport = "http"
	TransportSSE   Transport = "sse"
	TransportStdio Transport = "stdio"
)

// Server is the validated, in-memory view of one configured MCP mount.
// Transport-specific runtime state (the active StdioBridge) is attached
// lazily by the gateway and not represented here.
type Server struct {
	Name      string
	Transport Transport
	URL       string // http/sse only
	Command   string // stdio only
	Args      []string
	Env       map[string]string
	// Upstream is what an http or sse server is sent: the credential
	// the mount declares, or nothing.
	Upstream backend.Upstream
}

// Build validates an MCPServerConfig entry from node.yaml and returns a
// ready-to-mount Server. It rejects empty or malformed config before
// the gateway tries to register the mount, so bad config fails at
// startup-log time instead of at first request.
func Build(name string, cfg pkgConfig.MCPServerConfig) (*Server, error) {
	transport := Transport(strings.ToLower(strings.TrimSpace(cfg.Transport)))
	if transport == "" {
		transport = TransportHTTP
	}
	switch transport {
	case TransportHTTP, TransportSSE:
		if strings.TrimSpace(cfg.URL) == "" {
			return nil, errors.New("mcp: http/sse transport requires a non-empty url")
		}
	case TransportStdio:
		if strings.TrimSpace(cfg.Command) == "" {
			return nil, errors.New("mcp: stdio transport requires a non-empty command")
		}
	default:
		return nil, errors.New("mcp: transport must be http, sse, or stdio")
	}
	if err := cfg.API.Validate(); err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	upstream := backend.Engine()
	if cfg.API != nil {
		upstream = backend.ForProvider(cfg.API)
	}
	return &Server{
		Upstream:  upstream,
		Name:      name,
		Transport: transport,
		URL:       cfg.URL,
		Command:   cfg.Command,
		Args:      append([]string(nil), cfg.Args...),
		Env:       cfg.Env,
	}, nil
}
