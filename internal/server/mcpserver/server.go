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

const instructions = "zzRouter manages providers and models across a cluster through its coordinator. A node selector is a node name, never an address. Start with zz_overview and zz_providers, which lists inference providers including disabled ones. zz_nodes reports a node's OS, GPU, memory and disk, with *_gb values in GiB. managed=false means no installed copy owned by zzRouter; installation can still be available. Successful installation enables the provider during finalization, before the job completes. For an existing install, use zz_api to PATCH /zzrouter/v1/providers/{name} with enabled=true and a node query when needed. Check zz_models first. Only if the model is absent, search with zz_model_search, choose a HuggingFace GGUF variant with zz_model_files (a split variant is one choice; its file fetches every shard and the engine loads them together), download with zz_model_download and follow each per-node job_id with zz_job. To give a model vision, download with features: [vision]; zz_model_files lists the vision file it fetches. Then read zz_models on that node for the exact local name, preview with zz_run_preview, and launch with zz_run_start. zz_chat is a small smoke test. Inspect zz_runs, zz_logs and zz_params_get; tune with zz_params_set. zz_job waits at most 45 seconds; retry while running. Reuse idempotency_key when retrying the same start request; only an accepted request is replayed, with its job_id, so after an error or a failed job retry with a new key. Downloads cancel by deployment_id through zz_job_cancel; zz_run_restart relaunches, zz_run_stop stops. Read zz_update_status, refresh with zz_update_check and apply with zz_update_apply. For other tasks, zz_api_search searches the embedded spec; zz_api_get reads and zz_api changes eligible public management routes. When a result names task_tools, use that tool instead. REST remains the full contract at /openapi.yaml."

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
