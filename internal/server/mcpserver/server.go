// Package mcpserver exposes task-level MCP tools through the REST engine.
package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stperic/zzrouter/pkg/version"
)

// MaxRequestBytes bounds tool inputs independently of the general API limit.
const MaxRequestBytes = 1 << 20

// toolsListTTL keeps the binary's fixed tool declarations fresh for five minutes.
const toolsListTTL = 5 * time.Minute

const instructions = "zzRouter manages providers and models across a cluster. Connect to the coordinator; workers are reached through it. A node selector is a node name, never an address. Use zz_overview to learn nodes, health, enabled providers and endpoints. Use zz_job with a returned job_id to follow work; wait_seconds defaults to 0 and cannot exceed 45. REST remains the full API contract. For operations beyond these two tools, consult the discovery endpoints or /openapi.yaml."

// Config supplies the engine and its existing credential and route rules.
type Config struct {
	Engine     http.Handler
	KeyHeaders []string
	Streaming  func(method, path string) string
}

type contextKey uint8

const (
	outerKey contextKey = iota
	dispatchKey
)

type outerRequest struct {
	header                http.Header
	remoteAddr, requestID string
	ctx                   context.Context
	index                 atomic.Uint64
}

// IsDispatch identifies inner calls for HTTP accounting only, never auth.
func IsDispatch(ctx context.Context) bool { return ctx.Value(dispatchKey) == true }

func newDispatcher(ctx context.Context, cfg Config) (dispatcher, error) {
	outer, ok := ctx.Value(outerKey).(*outerRequest)
	if !ok || outer == nil {
		return dispatcher{}, fmt.Errorf("MCP request metadata is unavailable; retry through the public MCP endpoint")
	}
	return dispatcher{cfg, outer}, nil
}

// New builds a stateless handler serving both supported protocol eras.
func New(cfg Config) http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "zzrouter", Version: version.Current.String()}, &mcp.ServerOptions{Instructions: instructions, SetCacheable: func(_ context.Context, req mcp.Request, cache *mcp.Cacheable) {
		if _, ok := req.(*mcp.ListToolsRequest); ok {
			cache.TTLMs = int(toolsListTTL.Milliseconds())
		}
	}})
	registerTools(s, cfg)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, CrossOriginProtection: http.NewCrossOriginProtection(),
		MaxRequestBodyBytes: MaxRequestBytes, PropagateRequestCancellation: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outer := &outerRequest{header: r.Header.Clone(), remoteAddr: r.RemoteAddr, requestID: w.Header().Get("X-Request-ID"), ctx: r.Context()}
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), outerKey, outer)))
	})
}
