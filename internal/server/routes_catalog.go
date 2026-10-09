package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// RouteSpec describes mounted routes for discovery and MCP escape-hatch eligibility.
type RouteSpec struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Surface   string `json:"surface"` // health|infra|openai|ollama|admin|internal|nativewire|mcp|other
	Auth      string `json:"auth"`    // none|optional|admin|cluster
	Streaming string `json:"streaming,omitempty"`
	Async     bool   `json:"async,omitempty"`
	// Retired marks a route that is mounted but only ever answers 410
	// Gone. The catalog is the enumeration an agent plans against, and a
	// gravestone that reads like a live route costs it a request to find
	// out. Omitted when false so the common case stays unchanged on the
	// wire; the 410 body names the replacement.
	Retired bool `json:"retired,omitempty"`
}

// handleListRoutes serves the registered route catalog over the running
// gin engine. Source of truth = engine.Routes(); keeps the harness
// contract test in sync with reality without an AST parse of route
// registration files.
func (s *Server) handleListRoutes(c *gin.Context) {
	if s.engine == nil {
		ServiceUnavailable(c, "engine not ready")
		return
	}
	routes := s.engine.Routes()
	out := make([]RouteSpec, 0, len(routes))
	for _, r := range routes {
		out = append(out, catalogRoute(r))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return out[i].Method < out[j].Method
		}
		return out[i].Path < out[j].Path
	})
	c.JSON(http.StatusOK, gin.H{"routes": out, "count": len(out)})
}

func catalogRoute(r gin.RouteInfo) RouteSpec {
	return RouteSpec{Method: r.Method, Path: r.Path, Surface: surfaceForPath(r.Path), Auth: authTierForPath(r.Path), Streaming: streamingForPath(r.Method, r.Path), Async: asyncForPath(r.Method, r.Path), Retired: isRetiredRoute(r.Handler)}
}

// eligibleAPIRoute uses the same mounted-route facts as the public catalog
// and returns the operationId the embedded spec gives the matched route.
func (s *Server) eligibleAPIRoute(method, path string) (string, error) {
	var selected gin.RouteInfo
	best := ""
	for _, route := range s.engine.Routes() {
		if route.Method != method {
			continue
		}
		if rank, ok := catalogPathMatch(route.Path, path); ok && rank > best {
			selected, best = route, rank
		}
	}
	if best == "" {
		return "", fmt.Errorf("route %s %s is not mounted; use zz_api_search", method, path)
	}
	route := catalogRoute(selected)
	if route.Auth != "admin" || route.Surface != "admin" || route.Retired || route.Streaming != "" {
		return "", fmt.Errorf("route %s %s is unavailable: auth=%s retired=%t streaming=%s", method, path, route.Auth, route.Retired, route.Streaming)
	}
	return openAPIOperationID(method, normalizeGinPath(selected.Path))
}

// normalizeGinPath rewrites gin's route patterns into OpenAPI's
// {param} placeholder syntax.
func normalizeGinPath(path string) string {
	if !strings.ContainsAny(path, ":*") {
		return path
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, "*") || strings.HasPrefix(seg, ":") {
			segments[i] = "{" + seg[1:] + "}"
		}
	}
	return strings.Join(segments, "/")
}

// catalogPathMatch preserves Gin's static, parameter, then catch-all precedence.
func catalogPathMatch(pattern, path string) (string, bool) {
	parts, segments := strings.Split(path, "/"), strings.Split(pattern, "/")
	rank := ""
	for i, segment := range segments {
		if i >= len(parts) {
			return "", false
		}
		switch {
		case strings.HasPrefix(segment, "*"):
			return rank + "0", true
		case strings.HasPrefix(segment, ":"):
			if parts[i] == "" {
				return "", false
			}
			rank += "1"
		default:
			if segment != parts[i] {
				return "", false
			}
			rank += "2"
		}
	}
	return rank, len(parts) == len(segments)
}

func surfaceForPath(p string) string {
	switch {
	case p == "/zzrouter/v1/mcp" || strings.HasPrefix(p, "/zzrouter/v1/mcp/"):
		return "mcp"
	case p == "/zzrouter/v1/internal" || strings.HasPrefix(p, "/zzrouter/v1/internal/"):
		return "internal"
	case p == "/zzrouter/v1" || strings.HasPrefix(p, "/zzrouter/v1/"):
		return "admin"
	case p == "/v1/messages" || strings.HasPrefix(p, "/v1/messages/"):
		return "anthropic"
	case strings.HasPrefix(p, "/v1/") || p == "/v1":
		return "openai"
	case strings.HasPrefix(p, "/api/") || p == "/api":
		return "ollama"
	case p == "/health" || strings.HasPrefix(p, "/health/"):
		return "health"
	case p == "/metrics" || strings.HasPrefix(p, "/openapi"):
		return "infra"
	case strings.HasPrefix(p, "/mcp/"):
		return "mcp"
	}
	return "other"
}

func authTierForPath(p string) string {
	switch {
	case p == "/zzrouter/v1/internal" || strings.HasPrefix(p, "/zzrouter/v1/internal/"):
		return "cluster"
	case p == "/zzrouter/v1" || strings.HasPrefix(p, "/zzrouter/v1/"):
		return "admin"
	case strings.HasPrefix(p, "/v1/") || p == "/v1",
		strings.HasPrefix(p, "/api/") || p == "/api":
		return "optional"
	case p == "/health" || strings.HasPrefix(p, "/health/"),
		p == "/metrics", strings.HasPrefix(p, "/openapi"):
		return "none"
	}
	return "none"
}

func streamingForPath(method, p string) string { //nolint:unparam // Keep method and path together at catalog call sites; streaming depends on path.
	switch p {
	// Server-log inventory returns JSON; the /logs suffix rule would misclassify it.
	case "/zzrouter/v1/server/logs":
		return ""
	case "/v1/chat/completions", "/v1/completions",
		"/api/chat", "/api/generate", "/api/pull":
		return "sse"
	case "/v1/audio/speech",
		"/zzrouter/v1/internal/sync/file":
		return "chunked"
	// Named rather than matched on an /events suffix: OpenAI's
	// /v1/fine_tuning/jobs/{id}/events is a paginated list, so the
	// suffix does not imply a stream.
	case "/zzrouter/v1/model-groups/events",
		"/zzrouter/v1/spend/events":
		return "sse"
	}
	if strings.HasSuffix(p, "/stream") || strings.HasSuffix(p, "/logs") {
		return "sse"
	}
	return ""
}

// snapshotAllowed admits only explicit snapshot modes on catalogued streaming routes.
func snapshotAllowed(r *http.Request) bool {
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.Method == http.MethodGet && len(segments) == 5 && segments[0] == "zzrouter" && segments[1] == "v1" && segments[2] == "runs" && segments[4] == "logs" {
		return r.URL.Query().Get("follow") == "false"
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.GetBody == nil {
		return false
	}
	body, err := r.GetBody()
	if err != nil {
		return false
	}
	defer body.Close()
	var mode struct {
		Stream *bool `json:"stream"`
	}
	return json.NewDecoder(body).Decode(&mode) == nil && mode.Stream != nil && !*mode.Stream
}

// asyncRoutes lists the method + path pairs (relative to /zzrouter/v1)
// that answer 202 with a job id to subscribe to. Hand-maintained mirror
// of the registration sites in routes_public.go + routes_internal.go,
// and it was wrong six times before anything checked it: /models/scans,
// /config/reload, /runs/batch and two install sub-steps claimed async
// handlers that answer synchronously, and DELETE /deployments inherited
// the flag from its POST sibling because the list was keyed by path
// alone. Hence the method here.
//
// TestRouteCatalog_AsyncFlagMatchesTheSpec now holds this list to the
// 202s documented in openapi.yaml, which is the artifact a human
// verified against the handlers. The internal mirror is not covered by
// that check: /zzrouter/v1/internal/* is undocumented by decision, so
// those entries remain claims.
var asyncRoutes = []string{
	"POST /runs",
	"POST /runs/load",
	"POST /runs/ensure",
	"POST /runs/:id/restart",
	"POST /deployments",
	"POST /providers/:name/install",
	"POST /providers/:name/install/execute-step",
	"POST /providers/:name/upgrade",
	"POST /update/apply",
	"DELETE /providers/:name",
	"POST /sync/deploy",
}

func asyncForPath(method, p string) bool {
	for _, entry := range asyncRoutes {
		m, suffix, ok := strings.Cut(entry, " ")
		if !ok || m != method {
			continue
		}
		if p == "/zzrouter/v1"+suffix || p == "/zzrouter/v1/internal"+suffix {
			return true
		}
	}
	return false
}
