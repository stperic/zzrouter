// Package mcpserver exposes task-level MCP tools through the REST engine.
package mcpserver

import (
	"context"
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

const instructions = "zzRouter manages cluster providers and models through its coordinator. Node selectors are names, never addresses. Start with zz_overview and zz_providers, including disabled providers. zz_nodes reports OS, GPU, memory and disk; *_gb is GiB. managed=false means no zzRouter-owned install, not unavailable. Installation enables the provider before its job completes. Enable an existing install with zz_api PATCH /zzrouter/v1/providers/{name}, enabled=true and node query. Check zz_models first. If absent, use zz_model_search, zz_model_files for HuggingFace GGUF, then zz_model_download; one split GGUF choice fetches all shards. Follow per-node job_id with zz_job. Add vision with zz_model_download features: [vision]; zz_model_files lists its file. Read zz_models for the exact local name, then zz_run_preview and zz_run_start. zz_chat is a smoke test; use probe: tools, embed or vision for capability evidence. Exact source metadata is show.model_info, show.template_present, show.capabilities and show.parameters; omissions are unknown, details describes serving. zz_model_delete deletes one exact physical identity; verify absence. Inspect zz_runs, zz_logs and zz_params_get; tune with zz_params_set. Omit job_id in zz_job for recent jobs on a node, with limit and offset. Supply job_id to read or wait up to 45 seconds; retry while running. A fresh unique idempotency_key identifies each operation across sessions; reuse only to retry that request. Only accepted requests replay with their job_id; after an error or failed job use a new key. Cancel downloads by deployment_id with zz_job_cancel; zz_run_restart relaunches, zz_run_stop stops. Use zz_update_status, zz_update_check and zz_update_apply for updates. For other tasks, zz_api_search searches the spec, zz_api_get reads and zz_api changes eligible public management routes. Prefer a result's task_tools. REST is the full contract at /openapi.yaml."

// Config supplies the engine and its existing credential and route rules.
type Config struct {
	Engine           http.Handler
	KeyHeaders       []string
	Streaming        func(method, path string) string
	SnapshotAllowed  func(*http.Request) bool
	SearchAPI        func(string, int) ([]map[string]any, error)
	EligibleAPIRoute func(method, path string) (operationID string, err error)
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

func newDispatcher(ctx context.Context, cfg Config, bindings map[string][]string) (dispatcher, error) {
	outer, ok := ctx.Value(outerKey).(*outerRequest)
	if !ok || outer == nil {
		return dispatcher{}, newToolError(http.StatusInternalServerError, codeInternal, "MCP request metadata is unavailable; retry through the public MCP endpoint", nil)
	}
	return dispatcher{cfg, outer, bindings}, nil
}

func newServer(cfg Config) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "zzrouter", Version: version.Current.String()}, &mcp.ServerOptions{Instructions: instructions, SetCacheable: func(_ context.Context, req mcp.Request, cache *mcp.Cacheable) {
		if _, ok := req.(*mcp.ListToolsRequest); ok {
			cache.TTLMs = int(toolsListTTL.Milliseconds())
		}
	}})
	s.AddReceivingMiddleware(toolErrorMiddleware)
	registerTools(s, cfg)
	return s
}

// New builds a stateless handler serving both supported protocol eras.
func New(cfg Config) http.Handler {
	s := newServer(cfg)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, CrossOriginProtection: http.NewCrossOriginProtection(),
		MaxRequestBodyBytes: MaxRequestBytes, PropagateRequestCancellation: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outer := &outerRequest{header: r.Header.Clone(), remoteAddr: r.RemoteAddr, requestID: w.Header().Get("X-Request-ID"), ctx: r.Context()}
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), outerKey, outer)))
	})
}
