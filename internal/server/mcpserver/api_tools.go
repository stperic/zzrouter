package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultAPISearchLimit lets an agent discover operations without dumping the spec.
const DefaultAPISearchLimit = 20

type apiSearchInput struct {
	Query string `json:"query" jsonschema:"Keywords to match against REST methods, paths, operation ids, summaries and parameters with their names, locations and required flags."`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum matching operations. Default 20, maximum 1000."`
}
type apiGetInput struct {
	Path  string            `json:"path" jsonschema:"Literal public management path beginning /zzrouter/v1/. No URL encoding, query, fragment, internal, MCP, streaming or retired routes."`
	Query map[string]string `json:"query,omitempty" jsonschema:"Optional query parameters, including node names. Keep them out of path."`
}
type apiInput struct {
	apiGetInput
	retryInput
	Method      string          `json:"method" jsonschema:"REST method other than GET, such as POST, PATCH, PUT or DELETE. Use zz_api_get for GET."`
	Body        json.RawMessage `json:"body,omitempty" jsonschema:"Optional JSON value to send unchanged, including null or a merge-patch object."`
	ContentType string          `json:"content_type,omitempty" jsonschema:"Body Content-Type. Default application/json; use application/merge-patch+json for parameter patches."`
	IfMatch     string          `json:"if_match,omitempty" jsonschema:"Optional REST entity revision for the If-Match header."`
}

func registerAPITools(s *mcp.Server, cfg Config, bindings map[string][]string) {
	read := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(bool)}
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_api_search", Description: "Find a REST operation when a curated tool does not cover the task. Searches the embedded OpenAPI spec and returns method, path, operationId, summary and parameter names, plus task_tools naming the curated tool that serves an operation; use zz_api_get for eligible public GETs or zz_api for writes.", Annotations: read}, nil, dispatcher.apiSearch)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_api_get", Description: "Read one public management REST route found with zz_api_search. Returns its JSON reply unchanged; use the operation summary and /openapi.yaml to interpret it. Internal, MCP, streaming and retired routes are refused.", Annotations: read}, nil, dispatcher.apiGet)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_api", Description: "Change one public management REST resource found with zz_api_search; every call is destructive. Supply a method other than GET and optional JSON body, content_type and retry headers; returns the REST JSON reply unchanged, or null for no content. Follow returned job_id values with zz_job. Internal, MCP, streaming and retired routes are refused.", Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(true)}}, nil, dispatcher.api)
}

func (d dispatcher) apiSearch(_ context.Context, in apiSearchInput) (agentOutput, error) {
	if in.Limit == 0 {
		in.Limit = DefaultAPISearchLimit
	}
	if in.Limit < 1 || in.Limit > MaxListLimit {
		return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, fmt.Sprintf("limit must be 1 to %d", MaxListLimit), nil)
	}
	if d.cfg.SearchAPI == nil {
		return agentOutput{}, newToolError(http.StatusInternalServerError, codeInternal, "OpenAPI search is unavailable", nil)
	}
	operations, err := d.cfg.SearchAPI(in.Query, in.Limit)
	if err != nil {
		return agentOutput{}, newToolError(http.StatusInternalServerError, codeInternal, err.Error(), err)
	}
	text := fmt.Sprintf("Returned %d operations; read the contract before calling one.", len(operations))
	var curated []string
	for _, operation := range operations {
		id, _ := operation["operationId"].(string)
		if tools := d.taskTools(id); len(tools) > 0 {
			operation["task_tools"] = tools
			curated = append(curated, tools...)
		}
	}
	if len(curated) > 0 {
		slices.Sort(curated)
		text += fmt.Sprintf(" Operations with task_tools are served by %s; call that tool instead.", strings.Join(slices.Compact(curated), ", "))
	}
	return agentOutput{fields: map[string]any{"operations": operations}, text: text}, nil
}

// taskTools names the registered tools bound to an operationId, sorted.
func (d dispatcher) taskTools(operationID string) []string {
	if operationID == "" {
		return nil
	}
	var tools []string
	for tool, ids := range d.bindings {
		if slices.Contains(ids, operationID) {
			tools = append(tools, tool)
		}
	}
	slices.Sort(tools)
	return tools
}

func (d dispatcher) apiGet(ctx context.Context, in apiGetInput) (agentOutput, error) {
	return d.apiCall(ctx, in, request{method: http.MethodGet})
}

func (d dispatcher) api(ctx context.Context, in apiInput) (agentOutput, error) {
	method := strings.ToUpper(in.Method)
	if method == http.MethodGet {
		return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, "GET requires zz_api_get", nil)
	}
	if in.ContentType == "" {
		in.ContentType = "application/json"
	}
	return d.apiCall(ctx, in.apiGetInput, request{method: method, body: in.Body, contentType: in.ContentType, idempotencyKey: in.IdempotencyKey, ifMatch: in.IfMatch})
}

func (d dispatcher) apiCall(ctx context.Context, in apiGetInput, req request) (agentOutput, error) {
	path := in.Path
	if !strings.HasPrefix(path, "/zzrouter/v1/") || strings.ContainsAny(path, "\\%?#") || strings.Contains(path, "..") || strings.Contains(path, "//") {
		return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, "use a literal /zzrouter/v1/ management path without encoding or traversal", nil)
	}
	if d.cfg.EligibleAPIRoute == nil {
		return agentOutput{}, newToolError(http.StatusInternalServerError, codeInternal, "route catalog is unavailable", nil)
	}
	operationID, err := d.cfg.EligibleAPIRoute(req.method, path)
	if err != nil {
		return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, err.Error(), err)
	}
	req.path, req.query = requestPath{path}, url.Values{}
	for key, value := range in.Query {
		req.query.Set(key, value)
	}
	var reply any
	if err := d.call(ctx, req, &reply); err != nil {
		return agentOutput{}, err
	}
	// Preserve any JSON value, while keeping the structured result an object for legacy clients.
	out := agentOutput{fields: map[string]any{"response": reply}, text: "REST request completed; interpret the response using /openapi.yaml."}
	// Served, not refused: a binding means a tool uses the route, not that it covers every option.
	if tools := d.taskTools(operationID); len(tools) > 0 {
		out.fields["task_tools"] = tools
		out.text = fmt.Sprintf("REST request completed. %s serves this route; use it next.", strings.Join(tools, " or "))
	}
	return out, nil
}
