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

const instructions = "zzRouter manages providers and models across a cluster. Connect to the coordinator; workers are reached through it. A node selector is a node name, never an address. Start with zz_overview, then zz_nodes, zz_models or zz_providers. Find models with zz_model_search and place them with zz_model_download. Preview with zz_run_preview before zz_run_start; inspect zz_runs, zz_logs or zz_params_get to diagnose, and use zz_params_set to tune. zz_chat is a small smoke test. Follow returned job_id values with zz_job; wait_seconds defaults to 0 and cannot exceed 45. Downloads return deployment_id plus per-node job ids; zz_job_cancel takes deployment_id to cancel a download. Reuse idempotency_key when retrying start tools. Use zz_run_restart to relaunch and zz_run_stop to stop. Read zz_update_status, refresh with zz_update_check, then use zz_update_apply to apply. REST remains the full API contract at /openapi.yaml."

// Config supplies the engine and its existing credential and route rules.
type Config struct {
	Engine          http.Handler
	KeyHeaders      []string
	Streaming       func(method, path string) string
	SnapshotAllowed func(*http.Request) bool
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
