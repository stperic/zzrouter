package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// DefaultListLimit keeps catalogs small enough for an agent's context.
	DefaultListLimit = 50
	// MaxListLimit matches REST pagination's ceiling.
	MaxListLimit = 1000
	// DefaultChatTokens is enough to verify that a model answers.
	DefaultChatTokens = 64
	// MaxChatTokens bounds the cost of this smoke-test surface.
	MaxChatTokens = 256
	// DefaultLogLines gives diagnostic context without dumping a whole log.
	DefaultLogLines = 200
	// MaxLogLines bounds line count before the dispatcher enforces its byte cap.
	MaxLogLines                   = 1000
	modelsRoute       routePrefix = "/zzrouter/v1/models"
	searchRoute       routePrefix = "/zzrouter/v1/search"
	runsRoute         routePrefix = "/zzrouter/v1/runs"
	previewRoute      routePrefix = "/zzrouter/v1/runs/preview"
	deploymentsRoute  routePrefix = "/zzrouter/v1/deployments"
	providersRoute    routePrefix = "/zzrouter/v1/providers/"
	filesRoute        routePrefix = "/zzrouter/v1/registries/huggingface/variants"
	serverLogsRoute   routePrefix = "/zzrouter/v1/server/logs"
	chatRoute         routePrefix = "/v1/chat/completions"
	updateStatusRoute routePrefix = "/zzrouter/v1/update/status"
	updateCheckRoute  routePrefix = "/zzrouter/v1/update/check"
	updateApplyRoute  routePrefix = "/zzrouter/v1/update/apply"
)

type pageInput struct {
	Limit  int `json:"limit,omitempty" jsonschema:"Maximum entries to return. Default 50, maximum 1000."`
	Offset int `json:"offset,omitempty" jsonschema:"Entries to skip when paging. Default 0; use with limit when has_more is true."`
}
type nodesInput struct {
	pageInput
	Node string `json:"node,omitempty" jsonschema:"Optional node name to inspect, never an address. Omit to list nodes."`
}
type modelsInput struct {
	pageInput
	Node     string `json:"node,omitempty" jsonschema:"Optional node name, never an address."`
	Provider string `json:"provider,omitempty" jsonschema:"Optional provider name to filter models."`
	Model    string `json:"model,omitempty" jsonschema:"Optional model name or pattern to filter models."`
}
type searchInput struct {
	pageInput
	Query    string `json:"query" jsonschema:"Model name or keywords to search for."`
	Provider string `json:"provider,omitempty" jsonschema:"Search registry, such as huggingface or ollama. Default all."`
}
type filesInput struct {
	Model    string `json:"model" jsonschema:"HuggingFace org/repo, a zz_model_search result's model."`
	Provider string `json:"provider,omitempty" jsonschema:"Optional provider whose feature files (a vision projector) to identify; omit for every enabled provider."`
}
type runsInput struct {
	pageInput
	RunID  string `json:"run_id,omitempty" jsonschema:"Optional run id to inspect. Omit to list runs."`
	Node   string `json:"node,omitempty" jsonschema:"Optional owning node name, never an address."`
	Status string `json:"status,omitempty" jsonschema:"Optional status filter: starting, running, unhealthy, stopping, failed or restarting. Stopped runs are removed."`
}
type runInput struct {
	RunID string `json:"run_id" jsonschema:"Run id returned by zz_runs or zz_run_start."`
	Node  string `json:"node,omitempty" jsonschema:"Optional owning node name, never an address."`
}
type retryInput struct {
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional idempotency key; reuse it when retrying the same operation."`
}
type previewInput struct {
	Model       string            `json:"model" jsonschema:"Exact local model name from zz_models, including its variant if needed."`
	Provider    string            `json:"provider" jsonschema:"Enabled provider name to preview."`
	Node        string            `json:"node,omitempty" jsonschema:"Optional target node name, never an address."`
	Endpoint    string            `json:"endpoint,omitempty" jsonschema:"Request shape: chat, embeddings or reranking. Default chat."`
	Parameters  map[string]string `json:"parameters,omitempty" jsonschema:"Optional launch parameter overrides."`
	Environment map[string]string `json:"environment,omitempty" jsonschema:"Optional launch environment overrides."`
}
type startInput struct {
	retryInput
	Model      string            `json:"model" jsonschema:"Exact local model name from zz_models."`
	Provider   string            `json:"provider" jsonschema:"Enabled provider name to launch."`
	Node       string            `json:"node,omitempty" jsonschema:"Optional target node name, never an address."`
	Endpoint   string            `json:"endpoint,omitempty" jsonschema:"Request shape: chat, embeddings or reranking. Default chat."`
	Parameters map[string]string `json:"parameters,omitempty" jsonschema:"Optional launch parameter overrides."`
	EnvVars    map[string]string `json:"env_vars,omitempty" jsonschema:"Optional launch environment overrides, replayed on restart."`
	Runtime    string            `json:"runtime,omitempty" jsonschema:"Optional runtime name declared by the provider; omit for its default."`
}
type restartInput struct {
	runInput
	retryInput
}
type downloadInput struct {
	retryInput
	Model    string   `json:"model" jsonschema:"Registry model identifier from zz_model_search."`
	Node     string   `json:"node,omitempty" jsonschema:"Optional target node name, never an address. Omit for REST's default placement."`
	Registry string   `json:"registry,omitempty" jsonschema:"Registry such as huggingface or ollama."`
	Provider string   `json:"provider,omitempty" jsonschema:"Optional provider to resolve the download format."`
	File     string   `json:"file,omitempty" jsonschema:"A variant's file from zz_model_files; a split variant's file fetches every shard."`
	Features []string `json:"features,omitempty" jsonschema:"Features whose files to fetch beside the weights, such as [vision] for a projector zz_model_files lists. Omit for provider defaults; [] for none."`
	Format   string   `json:"format,omitempty" jsonschema:"Optional model format, such as gguf or safetensors."`
}
type cancelInput struct {
	JobID        string `json:"job_id,omitempty" jsonschema:"Job id to cancel. Supply exactly one of job_id or deployment_id."`
	DeploymentID string `json:"deployment_id,omitempty" jsonschema:"Download deployment id to cancel across its nodes. Use this for downloads, not their job ids."`
	Node         string `json:"node,omitempty" jsonschema:"Optional owning node name for job cancellation, never an address."`
}
type providerInput struct {
	pageInput
	Provider string `json:"provider,omitempty" jsonschema:"Optional provider name to inspect. Omit to list inference provider inventory, including disabled providers."`
	Node     string `json:"node,omitempty" jsonschema:"Optional node name to filter inventory or target one provider, never an address."`
}
type installInput struct {
	retryInput
	Provider      string `json:"provider" jsonschema:"Configured provider name to check or install, even when managed is false."`
	Node          string `json:"node,omitempty" jsonschema:"Optional target node name, never an address."`
	PreflightOnly bool   `json:"preflight_only,omitempty" jsonschema:"Set true to check prerequisites first; false starts installation."`
	Version       string `json:"version,omitempty" jsonschema:"Optional provider version allowed by the managed recipe."`
	Runtime       string `json:"runtime,omitempty" jsonschema:"Optional runtime name declared by the provider; omit for its default."`
}
type paramsInput struct {
	Provider string `json:"provider" jsonschema:"Provider name whose resolved parameters to read."`
	Node     string `json:"node,omitempty" jsonschema:"Optional target node name, never an address."`
	Model    string `json:"model,omitempty" jsonschema:"Optional model name for model-tier resolution."`
}
type patchInput struct {
	paramsInput
	Patch           map[string]any `json:"patch" jsonschema:"Merge-patch object with defaults, models or nodes tiers; each holds parameters, environment or endpoints. Null deletes a value at that tier."`
	RestartAffected bool           `json:"restart_affected,omitempty" jsonschema:"Restart runs made stale by this patch. Follow any returned runs.restart_job_id with zz_job."`
}
type chatInput struct {
	Node      string `json:"node,omitempty" jsonschema:"Optional serving node name. Addresses the model as name@node."`
	Model     string `json:"model" jsonschema:"Model name to smoke test through the coordinator."`
	Message   string `json:"message" jsonschema:"One short user message."`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"Maximum reply tokens. Default 64, capped at 256."`
}
type logsInput struct {
	pageInput
	RunID string `json:"run_id,omitempty" jsonschema:"Run id for a log snapshot. Omit to list available run-log files on the coordinator."`
	Lines int    `json:"lines,omitempty" jsonschema:"Last N run-log lines. Default 200, capped at 1000. Never follows a stream."`
	Node  string `json:"node,omitempty" jsonschema:"Node the run ran on. Needed only for a stopped run, whose log stays on that node."`
	Grep  string `json:"grep,omitempty" jsonschema:"Literal text to find anywhere in the run log, including startup lines long scrolled out of the last N; returns the matching lines instead of the last N."`
}
type updateInput struct {
	Nodes []string `json:"nodes,omitempty" jsonschema:"Optional node names for status, or [all]. Omit for coordinator."`
}
type updateApplyInput struct {
	retryInput
	Version string   `json:"version,omitempty" jsonschema:"Optional exact version to apply. Omit to apply the coordinator's pending update."`
	Nodes   []string `json:"nodes,omitempty" jsonschema:"Optional target node names, or [all]; requires version. Omit for coordinator."`
}

type agentOutput struct {
	fields map[string]any
	text   string
}

func (out agentOutput) summary() string              { return out.text }
func (out agentOutput) MarshalJSON() ([]byte, error) { return json.Marshal(out.fields) }

func registerAgentTools(s *mcp.Server, cfg Config, bindings map[string][]string) {
	read := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(bool)}
	write := &mcp.ToolAnnotations{DestructiveHint: new(bool)}
	destructive := &mcp.ToolAnnotations{DestructiveHint: boolPointer(true)}
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_nodes", Description: "Find node names or inspect one node. Returns OS, roles, health, providers and hardware; use a node name in model and run tools. Memory, GPU and disk *_gb values are GiB (1024^3 bytes); compare them with a model's size_bytes divided by 1073741824.", Annotations: read}, []string{"listNodes", "getNode"}, dispatcher.nodes)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_models", Description: "Check available models before searching or downloading. The filter tests both local name and source_id, case-insensitively: exact (QWEN2.5 matches qwen2.5), the complete segment after the final / (QWEN2.5 matches org/qwen2.5), or a glob (*qwen2.5* matches org/qwen2.5-instruct). Matches are independent per entry; an exact hit does not suppress suffix hits. A partial name needs a glob. Returns names, formats, sizes and existing details.wire_endpoints and details.features when reported; these are serving capabilities, not a guessed model type. Select a local name for zz_run_preview.", Annotations: read}, []string{"listModels"}, dispatcher.models)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_model_search", Description: "Find registry models to download. Returns registry identifiers and popularity hints; for HuggingFace GGUF call zz_model_files to choose a file, then zz_model_download.", Annotations: read}, []string{"getSearch"}, dispatcher.search)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_model_files", Description: "Choose a GGUF file from a HuggingFace model found by zz_model_search. Returns filenames, quantizations, byte sizes and recommendations; pass the exact file to zz_model_download.", Annotations: read}, []string{"listRegistriesHuggingfaceVariants"}, dispatcher.files)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_model_download", OutputSchema: objectSchema[downloadOutput]{}, Description: "Download a registry model before previewing or starting it. Returns deployment_id, registry model, file and per-node job_id values; follow with zz_job, then zz_models on that node for the exact local model name to pass to preview/start. Cancel with zz_job_cancel and deployment_id.", Annotations: write}, []string{"createDeployment"}, dispatcher.download)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_runs", Description: "List runs or inspect one run_id. Returns model, provider, node, port, status and errors; use zz_logs to diagnose or zz_chat to smoke test a ready model.", Annotations: read}, []string{"listRuns", "getRun"}, dispatcher.runs)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_run_preview", Description: "Check zz_models first and select its exact local name. Only if absent, use zz_model_search, zz_model_download and zz_job, then check zz_models again. Preview with an enabled provider; returns full_command, parameters and sources, then call zz_run_start.", Annotations: read}, []string{"previewRuns"}, dispatcher.preview)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_run_start", OutputSchema: objectSchema[runOutput]{}, Description: "Check zz_models first; search and download only if absent. After zz_run_preview, start its exact local name with an enabled native provider. Returns run_id for zz_runs, zz_logs, zz_run_restart and zz_run_stop, and job_id for zz_job; follow the job, then smoke test with zz_chat.", Annotations: write}, []string{"createRun"}, dispatcher.start)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_run_restart", OutputSchema: objectSchema[runOutput]{}, Description: "Relaunch a run_id with its saved request overrides. Returns a new run_id and job_id; pass job_id to zz_job and use the new run_id with run tools afterward.", Annotations: write}, []string{"restartRun"}, dispatcher.restart)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_run_stop", Description: "Stop a run_id and interrupt its inference. The reply with run_id and stopped status confirms the stop. The run is removed; a later zz_runs lookup returns 404 by design.", Annotations: destructive}, []string{"deleteRun"}, dispatcher.stop)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_chat", OutputSchema: objectSchema[chatOutput]{}, Description: "Smoke test a model with one short user message through the coordinator. A bare name works when one node holds it; otherwise pass node. Returns reply text, model and token usage; inspect zz_logs if it fails. This is a smoke test, not an inference surface.", Annotations: write}, []string{"openaiCreateChatCompletion"}, dispatcher.chat)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_logs", Description: "Diagnose a run with its last bounded log lines, or every line matching grep across the whole log, with follow forced off. A stopped run's log stays readable with the node it ran on. Without run_id, returns available run-log file metadata, not server log text; choose a run_id and call again.", Annotations: read}, []string{"listRunLogs", "listServerLogs"}, dispatcher.logs)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_job_cancel", Description: "Cancel work using job_id, or cancel a download using deployment_id. Returns cancellation acknowledgement; follow with zz_job to verify a job's terminal state.", Annotations: destructive}, []string{"deleteJob", "deleteDeployment"}, dispatcher.cancelJob)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_providers", Description: "List inference provider inventory across the cluster or a selected node, including disabled providers; registries are excluded. Returns kind, enabled, installed, managed and version; managed means an installed copy owned by zzRouter, on-demand means a process launched per model, and runtime names a declared install recipe. For an absent copy use zz_provider_install; for an existing copy use zz_api to PATCH /zzrouter/v1/providers/{name} with enabled=true and the node query.", Annotations: read}, []string{"listProviders", "getProviderStatus"}, dispatcher.providers)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_provider_install", Description: "Check prerequisites with preflight_only=true, then install with false; managed=false means no zzRouter-owned copy is installed and does not forbid installing. Successful installation enables the provider during finalization, before the job completes. Follow with zz_job, then zz_providers. If installation is not needed, use zz_api to PATCH /zzrouter/v1/providers/{name} with enabled=true and the node query; cloud setup uses POST /zzrouter/v1/providers/{name}/verify after credentials are configured.", Annotations: write}, []string{"createProviderInstallPreflight", "createProviderInstall"}, dispatcher.install)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_params_get", Description: "Read a provider's resolved parameters and environment for a node or model, and in accepts every parameter it takes with type and description. Returns each value and its tier; use zz_params_set with a tiered merge-patch to change it.", Annotations: read}, []string{"getProviderResolvedParameters", "getProviderSchema"}, dispatcher.params)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_params_set", Description: "Apply a tiered merge-patch to provider parameters or environment. Returns resolved values and affected runs; restart_affected starts needed restarts. Follow with zz_job for runs.restart_job_id, then zz_params_get.", Annotations: write}, []string{"patchProviderParameters"}, dispatcher.patch)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_update_status", Description: "Read update status for selected nodes. Returns availability, version, progress and errors; call zz_update_check to refresh availability, then zz_update_apply when an update is wanted.", Annotations: read}, []string{"getUpdateStatus"}, dispatcher.updateStatus)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_update_check", Description: "Check for a newer release on the coordinator. Returns availability, release and skipped reason; read zz_update_status or use zz_update_apply when an update is wanted.", Annotations: &mcp.ToolAnnotations{DestructiveHint: new(bool), IdempotentHint: true}}, []string{"createUpdateCheck"}, dispatcher.updateCheck)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_update_apply", Description: "Apply a pending coordinator update or an exact version on selected nodes; nodes may restart. Returns job_id when available; follow with zz_job, or zz_update_status when delegated to the privileged updater.", Annotations: destructive}, []string{"createUpdateApply"}, dispatcher.updateApply)
}

func boolPointer(value bool) *bool { return &value }

func (in pageInput) values() (url.Values, error) {
	if in.Limit == 0 {
		in.Limit = DefaultListLimit
	}
	if in.Limit < 1 || in.Limit > MaxListLimit || in.Offset < 0 {
		return nil, fmt.Errorf("limit must be 1 to %d and offset must be nonnegative", MaxListLimit)
	}
	return url.Values{"limit": {strconv.Itoa(in.Limit)}, "offset": {strconv.Itoa(in.Offset)}}, nil
}

// childPath keeps every dynamic identifier in the shared segment validator.
func childPath(prefix routePrefix, id, suffix string) (requestPath, error) {
	path, err := pathSegment(prefix, id)
	if err == nil {
		path.value += suffix
	}
	return path, err
}

func jsonBody(input any, omit ...string) ([]byte, error) {
	raw, err := json.Marshal(input)
	if err != nil || len(omit) == 0 {
		return raw, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	for _, key := range omit {
		delete(body, key)
	}
	return json.Marshal(body)
}

func selectFields(data map[string]any, fields string) map[string]any {
	out := map[string]any{}
	for _, field := range strings.Fields(fields) {
		key, alias, renamed := strings.Cut(field, ":")
		if value, ok := data[key]; ok {
			if renamed {
				key = alias
			}
			switch key {
			case "runs":
				if runs, ok := value.(map[string]any); ok {
					value = selectFields(runs, "stale overridden unknown restart_job_id error")
				}
			case "stale", "overridden", "unknown":
				value = compactObjects(value, "id:run_id node model changed overridden reason")
			case "details":
				if details, ok := value.(map[string]any); ok {
					value = selectFields(details, "wire_endpoints features")
				}
			case "providers":
				value = compactObjects(value, "key name type kind formats running")
			case "latest_release":
				if release, ok := value.(map[string]any); ok {
					value = selectFields(release, "version tag_name name published_at")
				}
			case "status":
				if status, ok := value.(map[string]any); ok {
					value = selectFields(status, updateFields)
				}
			}
			out[key] = value
		}
	}
	return out
}

func compactObjects(value any, fields string) any {
	if items, ok := value.([]any); ok {
		for i, item := range items {
			if object, ok := item.(map[string]any); ok {
				items[i] = selectFields(object, fields)
			}
		}
	}
	return value
}

func (d dispatcher) object(ctx context.Context, req request, fields, summary string) (agentOutput, error) {
	var response map[string]any
	if err := d.call(ctx, req, &response); err != nil {
		return agentOutput{}, err
	}
	if data, wrapped := response["data"].(map[string]any); wrapped {
		response = data
	}
	return agentOutput{fields: selectFields(response, fields), text: summary}, nil
}

func (d dispatcher) list(ctx context.Context, path requestPath, page pageInput, filters url.Values, nested, fields, label string) (agentOutput, error) {
	query, err := page.values()
	if err != nil {
		return agentOutput{}, err
	}
	for key, values := range filters {
		query[key] = values
	}
	var response struct {
		Data    any  `json:"data"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	}
	if err := d.call(ctx, request{method: http.MethodGet, path: path, query: query}, &response); err != nil {
		return agentOutput{}, err
	}
	if nested != "" {
		data, ok := response.Data.(map[string]any)
		if !ok {
			return agentOutput{}, fmt.Errorf("decode %s results: expected object", label)
		}
		total, _ := data["total"].(float64)
		if pagination, ok := data["pagination"].(map[string]any); ok {
			response.HasMore, _ = pagination["has_next"].(bool)
		}
		response.Data, response.Total = data[nested], int(total)
	}
	items, ok := response.Data.([]any)
	if !ok && response.Data != nil {
		return agentOutput{}, fmt.Errorf("decode %s list: expected array", label)
	}
	limit, _ := strconv.Atoi(query.Get("limit"))
	offset, _ := strconv.Atoi(query.Get("offset"))
	// These routes ignore REST pagination, so bound their projection here.
	if path == nodesRoute.path() || path == runsRoute.path() || path == filesRoute.path() {
		response.Total = len(items)
		items = items[min(offset, len(items)):]
	}
	response.HasMore = response.HasMore || len(items) > limit
	out := make([]map[string]any, 0, min(len(items), limit))
	for _, item := range items[:min(len(items), limit)] {
		object, ok := item.(map[string]any)
		if !ok {
			return agentOutput{}, fmt.Errorf("decode %s item: expected object", label)
		}
		out = append(out, selectFields(object, fields))
	}
	return agentOutput{fields: map[string]any{label: out, "total": response.Total, "has_more": response.HasMore}, text: fmt.Sprintf("Returned %d %s; has_more=%t.", len(out), label, response.HasMore)}, nil
}

func (d dispatcher) nodes(ctx context.Context, in nodesInput) (agentOutput, error) {
	fields := "name os cluster_role health_status providers cpu memory gpu disk version last_error"
	if in.Node != "" {
		return d.childObject(ctx, request{method: http.MethodGet, query: url.Values{}}, routePrefix(string(nodesRoute)+"/"), in.Node, "", "", fields, "Node status retrieved; use its name as a selector.")
	}
	return d.list(ctx, nodesRoute.path(), in.pageInput, nil, "", fields, "nodes")
}

func (d dispatcher) models(ctx context.Context, in modelsInput) (agentOutput, error) {
	return d.list(ctx, modelsRoute.path(), in.pageInput, url.Values{"node": {in.Node}, "provider": {in.Provider}, "model": {in.Model}}, "", "name source_id node assigned_app:provider format size is_cloud variant_of details", "models")
}

func (d dispatcher) search(ctx context.Context, in searchInput) (agentOutput, error) {
	return d.list(ctx, searchRoute.path(), in.pageInput, url.Values{"type": {"models"}, "q": {in.Query}, "provider": {in.Provider}}, "results", "id:model name provider size downloads likes pipeline_tag url", "matches")
}

func (d dispatcher) files(ctx context.Context, in filesInput) (agentOutput, error) {
	out, err := d.object(ctx, request{method: http.MethodGet, path: filesRoute.path(), query: url.Values{"id": {in.Model}, "provider": {in.Provider}}}, "variants features", "Pass a variant's file to zz_model_download; add features: [vision] to also fetch a listed vision file.")
	if err != nil {
		return out, err
	}
	out.fields["variants"] = compactObjects(out.fields["variants"], "file shards quantization bits size_bytes size_human recommended recommendation_reason")
	out.fields["features"] = compactObjects(out.fields["features"], "feature file size_bytes size_human")
	return out, nil
}

func (d dispatcher) runs(ctx context.Context, in runsInput) (agentOutput, error) {
	fields := "id:run_id provider model node port status waiting_on error_message failure parameters_status"
	q, err := in.values()
	if err != nil {
		return agentOutput{}, err
	}
	q.Set("node", in.Node)
	q.Set("status", in.Status)
	if in.RunID != "" {
		return d.childObject(ctx, request{method: http.MethodGet, query: q}, routePrefix(string(runsRoute)+"/"), in.RunID, "", in.Node, fields, "Run status retrieved; inspect logs or smoke test a ready model.")
	}
	return d.list(ctx, runsRoute.path(), in.pageInput, q, "", fields, "runs")
}

func (d dispatcher) preview(ctx context.Context, in previewInput) (agentOutput, error) {
	return d.object(ctx, request{method: http.MethodPost, path: previewRoute.path(), jsonBody: in, contentType: "application/json"}, "model provider node port full_command parameters parameter_sources", "Launch preview retrieved; adjust overrides or call zz_run_start.")
}

func (d dispatcher) start(ctx context.Context, in startInput) (agentOutput, error) {
	return d.object(ctx, request{method: http.MethodPost, path: runsRoute.path(), jsonBody: in, omit: []string{"idempotency_key"}, contentType: "application/json", idempotencyKey: in.IdempotencyKey}, "id:run_id job_id deploy_job_id node provider status port", "Run launch accepted; follow with zz_job.")
}

func (d dispatcher) childObject(ctx context.Context, req request, prefix routePrefix, id, suffix, node, fields, summary string) (agentOutput, error) {
	path, err := childPath(prefix, id, suffix)
	if err != nil {
		return agentOutput{}, err
	}
	req.path = path
	if req.query == nil {
		req.query = url.Values{"node": {node}}
	}
	return d.object(ctx, req, fields, summary)
}

func (d dispatcher) restart(ctx context.Context, in restartInput) (agentOutput, error) {
	return d.childObject(ctx, request{method: http.MethodPost, idempotencyKey: in.IdempotencyKey}, routePrefix(string(runsRoute)+"/"), in.RunID, "/restart", in.Node, "id:run_id old_id:old_run_id job_id node provider model status port", "Run restart accepted; follow with zz_job and use the new run_id.")
}

func (d dispatcher) stop(ctx context.Context, in runInput) (agentOutput, error) {
	return d.childObject(ctx, request{method: http.MethodDelete}, routePrefix(string(runsRoute)+"/"), in.RunID, "", in.Node, "id:run_id status message node", "Run stop confirmed; the run is removed and later lookups return 404 by design.")
}

func (d dispatcher) download(ctx context.Context, in downloadInput) (agentOutput, error) {
	body := map[string]any{"model": in.Model, "registry": in.Registry, "provider": in.Provider, "file": in.File, "format": in.Format}
	if in.Features != nil {
		body["features"] = in.Features
	}
	if in.Node != "" {
		body["nodes"] = []string{in.Node}
	}
	out, err := d.object(ctx, request{method: http.MethodPost, path: deploymentsRoute.path(), jsonBody: body, contentType: "application/json", idempotencyKey: in.IdempotencyKey}, "id:deployment_id model file status nodes restart_job_id", "Model placement returned; follow with zz_job for each node job_id, then zz_models.")
	if nodes, ok := out.fields["nodes"]; ok {
		out.fields["nodes"] = compactObjects(nodes, "node job_id status error")
	}
	return out, err
}

func (d dispatcher) cancelJob(ctx context.Context, in cancelInput) (agentOutput, error) {
	if (in.JobID == "") == (in.DeploymentID == "") {
		return agentOutput{}, fmt.Errorf("supply exactly one of job_id or deployment_id")
	}
	prefix, id := jobsRoute, in.JobID
	if in.DeploymentID != "" {
		if in.Node != "" {
			return agentOutput{}, fmt.Errorf("deployment cancellation covers all its nodes; omit node")
		}
		prefix, id = routePrefix(string(deploymentsRoute)+"/"), in.DeploymentID
	}
	return d.childObject(ctx, request{method: http.MethodDelete}, prefix, id, "", in.Node, "accepted job_id id status", "Cancellation acknowledged; follow with zz_job to verify job completion.")
}

func (d dispatcher) providers(ctx context.Context, in providerInput) (agentOutput, error) {
	if in.Provider != "" {
		out, err := d.childObject(ctx, request{method: http.MethodGet}, providersRoute, in.Provider, "/status", in.Node, "name installed managed version managed_process mode enabled", "Provider status retrieved.")
		if err == nil {
			out.text = fmt.Sprintf("Provider status: installed=%v, enabled=%v.", out.fields["installed"], out.fields["enabled"])
		}
		return out, err
	}
	return d.list(ctx, routePrefix(strings.TrimSuffix(string(providersRoute), "/")).path(), in.pageInput, url.Values{"node": {in.Node}}, "", "name type node kind mode enabled installed managed version", "providers")
}

func (d dispatcher) install(ctx context.Context, in installInput) (agentOutput, error) {
	suffix, summary := "/install", "Provider install accepted; follow with zz_job, then zz_providers."
	if in.PreflightOnly {
		suffix, summary = "/install/preflight", "Provider prerequisites retrieved; resolve blockers before starting installation."
	}
	return d.childObject(ctx, request{method: http.MethodPost, jsonBody: in, omit: []string{"provider", "node", "preflight_only", "idempotency_key"}, contentType: "application/json", idempotencyKey: in.IdempotencyKey}, providersRoute, in.Provider, suffix, in.Node, "accepted job_id provider node runtime results all_ok", summary)
}

func (d dispatcher) params(ctx context.Context, in paramsInput) (agentOutput, error) {
	out, err := d.childObject(ctx, request{method: http.MethodGet, query: url.Values{"node": {in.Node}, "model": {in.Model}}}, providersRoute, in.Provider, "/resolved", in.Node, "parameters environment request from", "Resolved values and tiers retrieved; accepts lists every parameter the provider takes. Use a tiered merge-patch to change them.")
	if err != nil {
		return out, err
	}
	// What is set and what can be set answer one question: how to change a launch.
	// The schema is the coordinator's: config is identical on every node.
	schema, err := d.childObject(ctx, request{method: http.MethodGet, query: url.Values{}}, providersRoute, in.Provider, "/schema", "", "parameters", "")
	if err != nil {
		return out, err
	}
	out.fields["accepts"] = schema.fields["parameters"]
	return out, nil
}

func (d dispatcher) patch(ctx context.Context, in patchInput) (agentOutput, error) {
	if in.Patch == nil {
		return agentOutput{}, fmt.Errorf("patch must be a merge-patch object")
	}
	q := url.Values{"node": {in.Node}, "model": {in.Model}}
	if in.RestartAffected {
		q.Set("restart", "affected")
	}
	return d.childObject(ctx, request{method: http.MethodPatch, query: q, jsonBody: in.Patch, contentType: "application/merge-patch+json"}, providersRoute, in.Provider, "/parameters", in.Node, "parameters environment request from runs", "Parameters patched; follow with zz_job if runs.restart_job_id is present, then zz_params_get.")
}

func (d dispatcher) chat(ctx context.Context, in chatInput) (agentOutput, error) {
	if in.Node != "" {
		in.Model += "@" + in.Node
	}
	if in.MaxTokens == 0 {
		in.MaxTokens = DefaultChatTokens
	}
	if in.MaxTokens < 1 {
		return agentOutput{}, fmt.Errorf("max_tokens must be positive")
	}
	var reply struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := d.call(ctx, request{method: http.MethodPost, path: chatRoute.path(), jsonBody: map[string]any{"model": in.Model, "messages": []map[string]string{{"role": "user", "content": in.Message}}, "stream": false, "max_tokens": min(in.MaxTokens, MaxChatTokens)}, contentType: "application/json"}, &reply); err != nil {
		return agentOutput{}, err
	}
	if len(reply.Choices) == 0 {
		return agentOutput{}, fmt.Errorf("chat response has no choices; inspect zz_logs")
	}
	return agentOutput{fields: map[string]any{"reply": reply.Choices[0].Message.Content, "model": reply.Model, "usage": selectFields(reply.Usage, "prompt_tokens completion_tokens total_tokens")}, text: "Smoke test reply received; token usage is included."}, nil
}

func (d dispatcher) logs(ctx context.Context, in logsInput) (agentOutput, error) {
	if in.RunID == "" {
		return d.list(ctx, serverLogsRoute.path(), in.pageInput, nil, "", "instance_id:run_id size modified is_compressed", "log_files")
	}
	if in.Lines == 0 {
		in.Lines = DefaultLogLines
	}
	if in.Lines < 1 {
		return agentOutput{}, fmt.Errorf("lines must be positive")
	}
	q := url.Values{"follow": {"false"}, "lines": {strconv.Itoa(min(in.Lines, MaxLogLines))}}
	if in.Grep != "" {
		q.Set("grep", in.Grep)
	}
	if in.Node != "" {
		q.Set("node", in.Node)
	}
	return d.childObject(ctx, request{method: http.MethodGet, query: q}, routePrefix(string(runsRoute)+"/"), in.RunID, "/logs", "", "log_file total_lines lines truncated filtered", "Run log snapshot retrieved; use errors to adjust parameters or provider setup.")
}

func (d dispatcher) updateStatus(ctx context.Context, in updateInput) (agentOutput, error) {
	q := url.Values{}
	if len(in.Nodes) != 0 {
		q.Set("nodes", strings.Join(in.Nodes, ","))
	}
	out, err := d.object(ctx, request{method: http.MethodGet, path: updateStatusRoute.path(), query: q}, updateFields+" nodes", "Update status retrieved; apply a wanted update with zz_update_apply.")
	if nodes, ok := out.fields["nodes"]; ok {
		out.fields["nodes"] = compactObjects(nodes, "node state error status")
	}
	return out, err
}

func (d dispatcher) updateCheck(ctx context.Context, _ overviewInput) (agentOutput, error) {
	out, err := d.object(ctx, request{method: http.MethodPost, path: updateCheckRoute.path()}, updateFields, "Update check complete; use zz_update_apply when an update is wanted.")
	return out, err
}

// updateFields keeps local and per-node update projections consistent.
const updateFields = "state current_version latest_version latest_release update_available skipped_reason download_progress error blocked restart_required restart_required_reason operation privileged_run confirmation_error"

func (d dispatcher) updateApply(ctx context.Context, in updateApplyInput) (agentOutput, error) {
	return d.object(ctx, request{method: http.MethodPost, path: updateApplyRoute.path(), jsonBody: in, omit: []string{"idempotency_key"}, contentType: "application/json", idempotencyKey: in.IdempotencyKey}, "job_id id version nodes state action", "Update accepted; follow with zz_job when job_id is present, otherwise zz_update_status.")
}
