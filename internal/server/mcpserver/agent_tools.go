package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stperic/zzrouter/pkg/config/runsettings"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
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
	// ToolProbeTokens leaves room for reasoning before a tool call while bounding probe cost.
	ToolProbeTokens = 4096
	probeFunction   = "lookup_weather"
	probeMessage    = "What is the current weather in Paris? Call lookup_weather with city Paris to find out; do not guess."
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
type deleteModelInput struct {
	retryInput
	Provider string `json:"provider,omitempty" jsonschema:"Optional assigned provider from zz_models, to select one backend."`
	Registry string `json:"registry,omitempty" jsonschema:"Optional source_repo from zz_models; addresses a literal physical catalog name."`
	SourceID string `json:"source_id,omitempty" jsonschema:"Optional source_id from zz_models, required to disambiguate same-name physical sources."`
	Model    string `json:"model" jsonschema:"Exact physical catalog name from zz_models; globs and logical variants cannot be deleted."`
	Node     string `json:"node,omitempty" jsonschema:"Optional owning node name. Required when several remote nodes hold the model."`
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
	Status string `json:"status,omitempty" jsonschema:"Optional status filter: starting, running, unhealthy, stopping, failed, restarting or stopped (a retained pin without a run)."`
}
type runInput struct {
	RunID string `json:"run_id" jsonschema:"Run id returned by zz_runs or zz_run_start."`
	Node  string `json:"node,omitempty" jsonschema:"Optional owning node name, never an address."`
}
type retryInput struct {
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional idempotency key, unique per operation: keys are remembered across sessions, so a new operation needs a key never used before. Reuse it only when retrying the same operation."`
}
type previewInput struct {
	Model       string                 `json:"model,omitempty" jsonschema:"Exact local model name from zz_models; omit for a node-scoped provider."`
	Provider    string                 `json:"provider" jsonschema:"Enabled provider name to preview."`
	Node        string                 `json:"node,omitempty" jsonschema:"Optional target node name, never an address."`
	Endpoint    string                 `json:"endpoint,omitempty" jsonschema:"Request shape: chat, embeddings or reranking. Default chat. An endpoint the provider does not serve returns 400; one whose runtime is missing names the runtime for zz_provider_install."`
	Parameters  schema.ParameterValues `json:"parameters,omitempty" jsonschema:"Optional launch parameter overrides as strings, numbers or booleans, including keep-alive (5m, 0 after release, -1 pins against idle expiry and pressure reclaim; inference hints never change pins) and evict-idle-on-memory-pressure (true or false)."`
	Environment map[string]string      `json:"environment,omitempty" jsonschema:"Optional launch environment overrides."`
}
type startInput struct {
	retryInput
	Model      string                 `json:"model,omitempty" jsonschema:"Exact local model name from zz_models; omit for a node-scoped provider."`
	Provider   string                 `json:"provider" jsonschema:"Enabled provider name to launch."`
	Node       string                 `json:"node,omitempty" jsonschema:"Optional target node name, never an address."`
	Endpoint   string                 `json:"endpoint,omitempty" jsonschema:"Request shape: chat, embeddings or reranking. Default chat. An endpoint the provider does not serve returns 400; one whose runtime is missing names the runtime for zz_provider_install."`
	Parameters schema.ParameterValues `json:"parameters,omitempty" jsonschema:"Optional launch parameter overrides as strings, numbers or booleans, including keep-alive (5m, 0 after release, -1 pins against idle expiry and pressure reclaim; inference hints never change pins) and evict-idle-on-memory-pressure (true or false)."`
	EnvVars    map[string]string      `json:"env_vars,omitempty" jsonschema:"Optional launch environment overrides, replayed on restart."`
	Runtime    string                 `json:"runtime,omitempty" jsonschema:"Optional runtime name declared by the provider; omit for its default."`
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
	Features []string `json:"features,omitempty" jsonschema:"Features to provision: vision, embeddings or reranking. Vision can fetch a projector listed by zz_model_files; a feature with its own runtime installs it. Omit for provider defaults; [] for none."`
	Format   string   `json:"format,omitempty" jsonschema:"Optional model format, such as gguf or safetensors."`
}
type cancelInput struct {
	JobID        string `json:"job_id,omitempty" jsonschema:"Job id to cancel, including an individual download. Supply exactly one of job_id or deployment_id."`
	DeploymentID string `json:"deployment_id,omitempty" jsonschema:"Download deployment id to cancel across all its nodes. Use job_id for an individual download."`
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
	Provider string `json:"provider" jsonschema:"Provider whose parameters to read or change. zz_params_get describes set parameters under accepts; pass grep to zz_params_get to find more accepted names and descriptions."`
	Node     string `json:"node,omitempty" jsonschema:"Optional target node name, never an address."`
	Model    string `json:"model,omitempty" jsonschema:"Optional model name for model-tier resolution."`
	Endpoint string `json:"endpoint,omitempty" jsonschema:"Optional run endpoint to resolve: chat, embeddings or reranking. Reads default to the base view; writes default to the single endpoint in the patch. Its parameters join values and accepts on reads; zz_params_get lists additional keys in endpoint_parameters."`
}
type paramsGetInput struct {
	paramsInput
	Grep string `json:"grep,omitempty" jsonschema:"Literal text matched case-insensitively in accepted parameter names and descriptions, for example context, kv or thread. Adds matches to the set parameters in accepts."`
}
type patchInput struct {
	paramsInput
	Patch           map[string]any `json:"patch" jsonschema:"Merge-patch object: declare a newer engine flag under schema.<flag> and set it in one call. Defaults, models or nodes tiers; each holds parameters, environment, request or endpoints. Create a logical variant with models.<new-name>.from pointing to a physical base, plus optional parameters, environment or request settings. Null deletes a value at that tier."`
	RestartAffected bool           `json:"restart_affected,omitempty" jsonschema:"Restart runs made stale by this patch. Follow any returned runs.restart_job_id with zz_job."`
}
type chatInput struct {
	Node      string    `json:"node,omitempty" jsonschema:"Optional serving node name. Addresses the model as name@node."`
	Model     string    `json:"model" jsonschema:"Model name to smoke test through the coordinator."`
	Message   string    `json:"message,omitempty" jsonschema:"One short user message for chat; ignored for fixed tools, embed and vision probes."`
	MaxTokens int       `json:"max_tokens,omitempty" jsonschema:"Maximum chat reply tokens. Default 64, capped at 256; fixed probes ignore this setting."`
	Probe     probeKind `json:"probe,omitempty" jsonschema:"Request-local smoke test: chat (default), tools, embed or vision. Fixed probes use their own input and token budgets."`
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

func registerAgentTools(s *mcp.Server, cfg Config, bindings map[string][]string, jobRetentionDescription string) {
	read := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(bool)}
	write := &mcp.ToolAnnotations{DestructiveHint: new(bool)}
	destructive := &mcp.ToolAnnotations{DestructiveHint: boolPointer(true)}
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_nodes", Description: "Find node names or inspect one node. Returns OS, roles, health, providers and hardware; use a node name in model and run tools. Memory, GPU and disk *_gb values are GiB (1024^3 bytes); use zz_run_preview for capacity assessment, unknown_cause and known weights_mib/capacity_mib evidence; these amounts alone do not establish fit.", Annotations: read}, []string{"listNodes", "getNode"}, dispatcher.nodes)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_models", Description: "Check available models before searching or downloading. The filter tests both local name and source_id, case-insensitively: exact (QWEN2.5 matches qwen2.5), the complete segment after the final / (QWEN2.5 matches org/qwen2.5), or a glob (*qwen2.5* matches org/qwen2.5-instruct). Matches are independent per entry; an exact hit does not suppress suffix hits. A partial name needs a glob. Returns names, formats, sizes and existing details.wire_endpoints and details.features when reported; feature keys include vision, tools and declared embeddings/reranking. Tools is present, available, unsupported or unknown; only present advertises tools. Use zz_chat probe: tools for request-local automatic-choice verification; wire endpoints describe chat and stay independent of endpoint runs. An exact name or source_id matching one complete total result also returns bounded show metadata: show.model_info, show.template_present, show.capabilities and show.parameters. Source fields can be omitted and then are unknown; never substitute serving details for missing show fields. Use zz_chat to verify serving support. Select a local name for zz_run_preview. incomplete includes retained manifest-only repositories; these return catalog identity without show. Repair with zz_model_download of the same model, or remove with zz_model_delete.", Annotations: read}, []string{"listModels", "showModels"}, dispatcher.models)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_model_delete", Description: "Delete one exact physical model from zz_models. Refuses weights held by a run or referenced by a configured variant: stop the named run or remove the variant through zz_params_set first. Supply node for an ambiguous cluster identity, and provider/source_id from zz_models for an ambiguous source. An active writer returns download_in_progress immediately; cancel its job with zz_job_cancel and await its terminal outcome before deleting. Verify the model is absent with zz_models after success.", Annotations: destructive}, []string{"deleteModels"}, dispatcher.deleteModel)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_model_search", Description: "Find registry models to download. Returns registry identifiers and popularity hints; for HuggingFace GGUF call zz_model_files to choose a file, then zz_model_download.", Annotations: read}, []string{"getSearch"}, dispatcher.search)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_model_files", Description: "Choose a GGUF file from a HuggingFace model found by zz_model_search. Returns filenames, quantizations, byte sizes and recommendations; pass the exact file to zz_model_download.", Annotations: read}, []string{"listRegistriesHuggingfaceVariants"}, dispatcher.files)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_model_download", OutputSchema: objectSchema[downloadOutput]{}, Description: "Download a registry model before previewing or starting it. Returns deployment_id, registry model, file and per-node job_id values; follow with zz_job, then zz_models on that node for the exact local model name to pass to preview/start. Cancel an individual download with zz_job_cancel and job_id; use deployment_id to cancel all deployment nodes. " + jobRetentionDescription, Annotations: write}, []string{"createDeployment"}, dispatcher.download)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_runs", Description: "List runs or inspect one run_id. Returns model, provider, node, port, base_url, engine_busy, pinned (true protects against router idle expiry and pressure reclaim), status, processor, context_length and optional cache (engine-reported kv_tokens per pool, kv_pools, sequence_slots, context_tokens, sharing and max_concurrency at that context; absent fields mean no captured evidence; absent kv_pools means the engine reported no pool count, as with vLLM's aggregate pool size; conflicting pool reports omit both KV fields; shared slots alone cannot establish concurrency, and scheduler limits are separate). A CPU share serves far slower; free GPU memory or use a smaller context or quant. Returns started_by (api: a run start; request: a chat or other inference to a model with no run started it), errors and evicted_runs, unpinned idle runs its launch reclaimed or models its daemon displaced; use zz_logs to diagnose or zz_chat to smoke test a ready model. A failed pin keeps its failure and supports zz_logs and zz_run_restart. A record_only pin is stopped with parameters and last_refusal: fix config with zz_params_set, replay with zz_run_start or zz_run_restart, or forget with zz_run_stop; it has no run log.", Annotations: read}, []string{"listRuns", "getRun"}, dispatcher.runs)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_run_preview", Description: "Check zz_models first and select its exact local name. Only if absent, use zz_model_search, zz_model_download and zz_job, then check zz_models again. Preview with an enabled local provider; returns full_command, parameters, sources and capacity assessment with context_mode, context_tokens, estimated affordable_context_tokens, optional placement, unknown_cause and known weights_mib/capacity_mib. Cloud and external providers have no local run to preview; use zz_chat to verify them. Follow the cause and reason with zz_params_get or measured zz_run_start, zz_job and zz_runs context_length/cache evidence; zz_logs diagnoses failures. For larger vLLM/llama.cpp workloads pass a number for the provider's context parameter (llama.cpp ctx-size, vLLM max-model-len); parameters take only declared names, others answer unknown_flag. Use zz_params_get with grep to find accepted names and descriptions. MLX always assesses its declared minimum. unknown is not confirmed fit; current load can still prevent launch.", Annotations: read}, []string{"previewRuns"}, dispatcher.preview)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_run_start", OutputSchema: objectSchema[runOutput]{}, Description: "Check zz_models first; search and download only if absent. Make its exact catalog name ready with an enabled provider. Choose chat, embeddings or reranking; an endpoint whose runtime is missing is refused with the runtime to install through zz_provider_install. For a node-scoped provider, omit model and endpoint; use the reported base_url directly. Process launches return run_id for zz_runs, zz_logs, zz_run_restart and zz_run_stop, and job_id for zz_job. With no parameters or env_vars, a start replays the pin. Env alone states a whole request tier; parameters replace the pin whole, so restate every override to keep. A reused run whose job has expired or is absent returns no job_id; inspect zz_runs. External and cloud models can return ready without a run or job. Follow any job, then smoke test with zz_chat. " + jobRetentionDescription, Annotations: write}, []string{"createRun"}, dispatcher.start)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_run_restart", OutputSchema: objectSchema[runOutput]{}, Description: "Relaunch a run_id with its saved request overrides. Returns a new run_id and job_id; pass job_id to zz_job and use the new run_id with run tools afterward. " + jobRetentionDescription, Annotations: write}, []string{"restartRun"}, dispatcher.restart)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_run_stop", Description: "Stop a run_id and interrupt its inference. The reply with run_id and stopped status confirms the stop. The run and its pin are forgotten. A record_only entry can also be forgotten; if a replay holds its key, use the live run_id named in the refusal.", Annotations: destructive}, []string{"deleteRun"}, dispatcher.stop)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_chat", OutputSchema: objectSchema[chatOutput]{}, Description: "Smoke test a model through the coordinator; pass message for chat, or probe: tools, embed or vision for fixed capability checks. A bare name works when one node holds it; otherwise pass node. A model with no run is started for the chat (zz_runs lists it with started_by request). Returns a request-local verdict: verified, not_supported or inconclusive, with next_steps. Tools also reports tool_call diagnostics; embed returns dimensions, never the vector. Vision checks two different images. Inspect zz_logs if it fails.", Annotations: write}, []string{"openaiCreateChatCompletion", "openaiCreateEmbedding"}, dispatcher.chat)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_logs", Description: "Diagnose a run with its last bounded log lines, or every line matching grep across the whole log, with follow forced off. A stopped run's log stays readable with the node it ran on. Without run_id, returns available run-log file metadata, not server log text; choose a run_id and call again.", Annotations: read}, []string{"listRunLogs", "listServerLogs"}, dispatcher.logs)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_job_cancel", Description: "Cancel work, including an individual download, using job_id; use deployment_id to cancel all deployment nodes. Returns cancellation acknowledgement; follow with zz_job to verify terminal state. A cancelled download reports failed with meta.transfer_status cancelled.", Annotations: destructive}, []string{"deleteJob", "deleteDeployment"}, dispatcher.cancelJob)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_providers", Description: "List inference provider inventory across the cluster or a selected node, including disabled providers; registries are excluded. Returns kind, enabled, installed, managed and version; managed means an installed copy owned by zzRouter, on-demand means a process launched per model, and runtime names a declared install recipe. For an absent copy use zz_provider_install; for an existing copy use zz_api to PATCH /zzrouter/v1/providers/{name} with enabled=true and the node query.", Annotations: read}, []string{"listProviders", "getProviderStatus"}, dispatcher.providers)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_provider_install", Description: "Check prerequisites with preflight_only=true, then install with false; managed=false means no zzRouter-owned copy is installed and does not forbid installing. Successful installation enables the provider during finalization, before the job completes. Follow with zz_job, then zz_providers. If installation is not needed, use zz_api to PATCH /zzrouter/v1/providers/{name} with enabled=true and the node query; cloud setup uses POST /zzrouter/v1/providers/{name}/verify after credentials are configured. " + jobRetentionDescription, Annotations: write}, []string{"createProviderInstallPreflight", "createProviderInstall"}, dispatcher.install)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_params_get", Description: "Read a provider's resolved parameters, environment, request and from for a node or model. Returns values and tiers; accepts gives type, range and description for every set parameter. Pass grep (literal text matched case-insensitively in names and descriptions, e.g. context, kv, thread) to add matches. accepts_total counts all parameters. Operator declarations carry operator: true; problems lists refused declarations or fields. Change values with zz_params_set.", Annotations: read}, []string{"getProviderResolvedParameters", "getProviderSchema"}, dispatcher.params)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_params_set", Description: "Apply a tiered merge-patch to provider parameters or environment. Declare a newer engine flag under patch.schema.<flag> and set its value in this same call. Create a single-level logical variant with patch.models.<new-name>.from set to a physical base, plus optional parameters, environment or request defaults. Returns resolved values and affected runs; restart_affected starts needed restarts. Follow with zz_job for runs.restart_job_id, then zz_params_get.", Annotations: write}, []string{"patchProviderParameters"}, dispatcher.patch)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_update_status", Description: "Read update status for selected nodes. Returns availability, version, progress, errors and bounded persisted rollout outcomes, which survive job expiry and node restart without an expiry; call zz_update_check to refresh availability, then zz_update_apply when an update is wanted.", Annotations: read}, []string{"getUpdateStatus"}, dispatcher.updateStatus)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_update_check", Description: "Check for a newer release on the coordinator. Returns availability, release and skipped reason; read zz_update_status or use zz_update_apply when an update is wanted.", Annotations: &mcp.ToolAnnotations{DestructiveHint: new(bool), IdempotentHint: true}}, []string{"createUpdateCheck"}, dispatcher.updateCheck)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_update_apply", Description: "Apply a pending coordinator update or an exact version on selected nodes; nodes may restart. Returns job_id when available; follow with zz_job, or zz_update_status when delegated to the privileged updater. " + jobRetentionDescription, Annotations: destructive}, []string{"createUpdateApply"}, dispatcher.updateApply)
}

func boolPointer(value bool) *bool { return &value }

func (in pageInput) values() (url.Values, error) {
	if in.Limit == 0 {
		in.Limit = DefaultListLimit
	}
	if in.Limit < 1 || in.Limit > MaxListLimit || in.Offset < 0 {
		return nil, newToolError(http.StatusBadRequest, codeInvalidInput, fmt.Sprintf("limit must be 1 to %d and offset must be nonnegative", MaxListLimit), nil)
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

func jsonBody(input any, omit []string) ([]byte, error) {
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
			case "rollouts":
				value = compactObjects(value, "job_id version state nodes unchecked_peers")
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
		Data     any            `json:"data"`
		Total    int            `json:"total"`
		HasMore  bool           `json:"has_more"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := d.call(ctx, request{method: http.MethodGet, path: path, query: query}, &response); err != nil {
		return agentOutput{}, err
	}
	if nested != "" {
		data, ok := response.Data.(map[string]any)
		if !ok {
			return agentOutput{}, newToolError(http.StatusBadGateway, codeResponseDecode, fmt.Sprintf("decode %s results: expected object", label), nil)
		}
		total, _ := data["total"].(float64)
		if pagination, ok := data["pagination"].(map[string]any); ok {
			response.HasMore, _ = pagination["has_next"].(bool)
		}
		response.Data, response.Total = data[nested], int(total)
	}
	items, ok := response.Data.([]any)
	if !ok && response.Data != nil {
		return agentOutput{}, newToolError(http.StatusBadGateway, codeResponseDecode, fmt.Sprintf("decode %s list: expected array", label), nil)
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
			return agentOutput{}, newToolError(http.StatusBadGateway, codeResponseDecode, fmt.Sprintf("decode %s item: expected object", label), nil)
		}
		out = append(out, selectFields(object, fields))
	}
	result := map[string]any{label: out, "total": response.Total, "has_more": response.HasMore}
	if node, ok := response.Metadata["node"]; ok {
		result["node"] = node
	}
	return agentOutput{fields: result, text: fmt.Sprintf("Returned %d %s; has_more=%t.", len(out), label, response.HasMore)}, nil
}

func (d dispatcher) nodes(ctx context.Context, in nodesInput) (agentOutput, error) {
	fields := "name os cluster_role health_status providers cpu memory gpu disk version last_error"
	if in.Node != "" {
		return d.childObject(ctx, request{method: http.MethodGet, query: url.Values{}}, routePrefix(string(nodesRoute)+"/"), in.Node, "", "", fields, "Node status retrieved; use its name as a selector.")
	}
	return d.list(ctx, nodesRoute.path(), in.pageInput, nil, "", fields, "nodes")
}

func (d dispatcher) models(ctx context.Context, in modelsInput) (agentOutput, error) {
	out, err := d.list(ctx, modelsRoute.path(), in.pageInput, url.Values{"node": {in.Node}, "provider": {in.Provider}, "model": {in.Model}}, "", "name source_id source_repo node assigned_app:provider format size is_cloud incomplete variant_of details", "models")
	if err != nil {
		return out, err
	}
	items, _ := out.fields["models"].([]map[string]any)
	if len(items) == 1 && items[0]["incomplete"] == true {
		return out, nil
	}
	if in.Model == "" || strings.ContainsAny(in.Model, "*?[]") || len(items) != 1 || out.fields["total"] != 1 || out.fields["has_more"] != false {
		return out, nil
	}
	entry := items[0]
	name, _ := entry["name"].(string)
	id, _ := entry["source_id"].(string)
	if !strings.EqualFold(in.Model, name) && !strings.EqualFold(in.Model, id) {
		return out, nil
	}
	node, _ := entry["node"].(string)
	provider, _ := entry["provider"].(string)
	registry, _ := entry["source_repo"].(string)
	var show struct {
		Data map[string]any `json:"data"`
	}
	err = d.call(ctx, request{method: http.MethodGet, path: routePrefix(string(modelsRoute) + "/show").path(), query: url.Values{"model": {in.Model}, "node": {node}, "provider": {provider}, "registry": {registry}, "compact": {"true"}}}, &show)
	if err != nil {
		var failure *toolError
		if !errors.As(err, &failure) || failure.problem.Status != http.StatusNotImplemented || failure.problem.Code != string(httperr.CodeModelDetailUnsupported) {
			return agentOutput{}, err
		}
		entry["show_unavailable"] = failure.problem
		out.text = "Catalog identity retrieved; model detail is unsupported for this source. Use serving probes for request-local evidence."
	} else {
		if show.Data == nil {
			return agentOutput{}, newToolError(http.StatusBadGateway, codeResponseDecode, "compact model detail must be an object", nil)
		}
		entry["show"] = show.Data
		out.text = "Exact source metadata is in show.model_info, show.template_present, show.capabilities and show.parameters. Omitted source fields are unknown; details describes serving. Use zz_chat for serving verification."
	}
	return out, nil
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
	fields := "id:run_id provider model node port base_url engine_busy pinned memory_commitments_mib processor context_length cache status started_by waiting_on evicted_runs error_message failure parameters_status parameters record_only last_refusal"
	q, err := in.values()
	if err != nil {
		return agentOutput{}, err
	}
	q.Set("node", in.Node)
	q.Set("status", in.Status)
	if in.RunID != "" {
		return d.childObject(ctx, request{method: http.MethodGet, query: q}, routePrefix(string(runsRoute)+"/"), in.RunID, "", in.Node, fields, "Run status retrieved; context_length and cache retain engine reports, omitted evidence is unknown. Inspect logs for failures or smoke test a ready model.")
	}
	return d.list(ctx, runsRoute.path(), in.pageInput, q, "", fields, "runs")
}

func (d dispatcher) preview(ctx context.Context, in previewInput) (agentOutput, error) {
	return d.object(ctx, request{method: http.MethodPost, path: previewRoute.path(), jsonBody: in, contentType: "application/json"}, "model provider node port full_command parameters parameter_sources assessment", "Inspect unknown_cause and reason for next tools; weights_mib and capacity_mib alone do not establish fit. Use zz_runs.context_length and zz_runs.cache for engine reports at the requested context; current load can still prevent launch.")
}

func (d dispatcher) start(ctx context.Context, in startInput) (agentOutput, error) {
	return d.object(ctx, request{method: http.MethodPost, path: runsRoute.path(), jsonBody: in, omit: []string{"idempotency_key"}, contentType: "application/json", idempotencyKey: in.IdempotencyKey}, "id:run_id job_id deploy_job_id node provider status port base_url parameters pinned", "Run launch accepted; follow job_id with zz_job when present, otherwise inspect zz_runs.")
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
	return d.childObject(ctx, request{method: http.MethodPost, idempotencyKey: in.IdempotencyKey}, routePrefix(string(runsRoute)+"/"), in.RunID, "/restart", in.Node, "id:run_id old_id:old_run_id job_id node provider model status port base_url", "Run restart accepted; follow job_id with zz_job when present, otherwise inspect zz_runs. Use the new run_id.")
}

func (d dispatcher) stop(ctx context.Context, in runInput) (agentOutput, error) {
	return d.childObject(ctx, request{method: http.MethodDelete}, routePrefix(string(runsRoute)+"/"), in.RunID, "", in.Node, "id:run_id status message node", "Run stop confirmed; the run and its pin are forgotten.")
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
		return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, "supply exactly one of job_id or deployment_id", nil)
	}
	prefix, id := jobsRoute, in.JobID
	if in.DeploymentID != "" {
		if in.Node != "" {
			return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, "deployment cancellation covers all its nodes; omit node", nil)
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

func (d dispatcher) params(ctx context.Context, in paramsGetInput) (agentOutput, error) {
	query := url.Values{"node": {in.Node}, "model": {in.Model}, "endpoint": {in.Endpoint}}
	out, err := d.childObject(ctx, request{method: http.MethodGet, query: query}, providersRoute, in.Provider, "/resolved", in.Node, "parameters environment request from", "Resolved values and tiers retrieved; use zz_params_set to change them.")
	if err != nil {
		return out, err
	}
	// Config is identical on every node, so the coordinator owns the dictionary.
	schema, err := d.childObject(ctx, request{method: http.MethodGet, query: url.Values{}}, providersRoute, in.Provider, "/schema", "", "parameters endpoints problems", "")
	if err != nil {
		return out, err
	}
	if problems, ok := schema.fields["problems"]; ok {
		out.fields["problems"] = problems
	}
	base, _ := schema.fields["parameters"].(map[string]any)
	endpoints, _ := schema.fields["endpoints"].(map[string]any)
	all := maps.Clone(base)
	scoped := map[string][]string{}
	for name, raw := range endpoints {
		overlay, _ := raw.(map[string]any)
		params, _ := overlay["parameters"].(map[string]any)
		if name == in.Endpoint {
			maps.Copy(all, params) // the endpoint's shape wins, as at launch
		} else if in.Endpoint == "" {
			for _, key := range slices.Sorted(maps.Keys(params)) {
				if _, shared := base[key]; !shared {
					scoped[name] = append(scoped[name], key)
				}
			}
		}
	}
	set, _ := out.fields["parameters"].(map[string]any)
	accepts := map[string]any{}
	for key := range set {
		if name := runsettings.FlagName(key); all[name] != nil {
			accepts[name] = all[name]
		}
	}
	grep := strings.ToLower(in.Grep)
	for name, entry := range all {
		shape, _ := entry.(map[string]any)
		description, _ := shape["description"].(string)
		if grep != "" && (strings.Contains(strings.ToLower(name), runsettings.FlagName(grep)) || strings.Contains(strings.ToLower(description), grep)) {
			accepts[name] = entry
		}
	}
	out.fields["accepts"], out.fields["accepts_total"] = accepts, len(all)
	if len(accepts) < len(all) {
		out.text += " Pass grep to zz_params_get to find more accepted parameter names and descriptions."
	}
	if len(scoped) > 0 {
		out.fields["endpoint_parameters"] = scoped
		out.text += " endpoint_parameters names parameters that apply only to that endpoint: pass endpoint to zz_params_get for their shapes and values, and set them with zz_params_set under endpoints.<endpoint>.parameters."
	}
	return out, nil
}

func (d dispatcher) patch(ctx context.Context, in patchInput) (agentOutput, error) {
	if in.Patch == nil {
		return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, "patch must be a merge-patch object", nil)
	}
	q := url.Values{"node": {in.Node}, "model": {in.Model}, "endpoint": {in.Endpoint}}
	if in.RestartAffected {
		q.Set("restart", "affected")
	}
	return d.childObject(ctx, request{method: http.MethodPatch, query: q, jsonBody: in.Patch, contentType: "application/merge-patch+json"}, providersRoute, in.Provider, "/parameters", in.Node, "parameters environment request from runs", "Parameters patched; follow with zz_job if runs.restart_job_id is present, then zz_params_get.")
}

func (d dispatcher) logs(ctx context.Context, in logsInput) (agentOutput, error) {
	if in.RunID == "" {
		return d.list(ctx, serverLogsRoute.path(), in.pageInput, nil, "", "instance_id:run_id size modified is_compressed", "log_files")
	}
	if in.Lines == 0 {
		in.Lines = DefaultLogLines
	}
	if in.Lines < 1 {
		return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, "lines must be positive", nil)
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
	out, err := d.object(ctx, request{method: http.MethodGet, path: updateStatusRoute.path(), query: q}, updateFields+" nodes rollouts", "Update status retrieved; apply a wanted update with zz_update_apply.")
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

func (d dispatcher) deleteModel(ctx context.Context, in deleteModelInput) (agentOutput, error) {
	if strings.TrimSpace(in.Model) == "" || strings.ContainsAny(in.Model, "*?[]") {
		return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, "model deletion requires one exact catalog name without glob syntax", nil)
	}
	body, err := json.Marshal(map[string]string{"model": in.Model, "node": in.Node, "provider": in.Provider, "registry": in.Registry, "source_id": in.SourceID})
	if err != nil {
		return agentOutput{}, err
	}
	return d.object(ctx, request{method: http.MethodDelete, path: modelsRoute.path(), body: body, idempotencyKey: in.IdempotencyKey}, "model node deleted errors", "Model deletion completed; verify absence with zz_models on the returned node.")
}
