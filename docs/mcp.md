# MCP for coding agents

zzRouter exposes `POST /zzrouter/v1/mcp` on the coordinator for Claude Code and
Codex. Tools call the existing REST handlers with your admin key; validation,
cluster routing, jobs and errors are the REST contract. Workers do not expose
this endpoint. Both 2026-07-28 and legacy 2025-06-18 clients are supported.

`zz_nodes` list/detail and `zz_overview` render the same effective node health
as REST. Three consecutive transport failures open the node breaker (`down`);
HTTP provider errors, caller deadlines and errors after response headers are neutral.
After a 30-second cooldown, two successful full
cluster probes recover it without inference. Half-open recovery reports `degraded`
and admits probes only. One missed full probe retains dispatch grace; public-only
`degraded`, `unknown`, `down` and half-open nodes cannot receive ordinary traffic.
Two missed probes or a graceful goodbye mark a node down. `last_seen_at` is the
latest successful probe, and `last_error` carries sanitized probe/dispatch evidence.
See [node routing errors](agent_error_codes.md#node-reachability).

## Connect

Make `ZZROUTER_ADMIN_API_KEY` available to your client through your normal
credential channel, and replace `<coordinator>` with its hostname.

Claude Code:

```sh
claude mcp add --transport http zzrouter \
  http://<coordinator>:9090/zzrouter/v1/mcp \
  --header "X-API-Key: ${ZZROUTER_ADMIN_API_KEY}"
```

Codex: add this to `~/.codex/config.toml` (or your project configuration):

```toml
[mcp_servers.zzrouter]
url = "http://<coordinator>:9090/zzrouter/v1/mcp"
env_http_headers = { "X-API-Key" = "ZZROUTER_ADMIN_API_KEY" }
tool_timeout_sec = 60
default_tools_approval_mode = "approve"
```

Non-interactive `codex exec` rejects tools not marked read-only unless
`default_tools_approval_mode = "approve"`. For interactive sessions use
`"writes"`, which prompts only for non-read-only tools. A per-tool override:

```toml
[mcp_servers.zzrouter.tools.zz_update_apply]
approval_mode = "prompt"
```

These policies control client approval; the server still enforces your API key.
See the [Codex configuration reference](https://developers.openai.com/codex/config-reference).

`env_http_headers` maps header names to environment variable names. See the
[Codex MCP configuration](https://developers.openai.com/codex/mcp) for these
settings. Codex versions exposing the optional `mcp_2026_07_28` feature can
opt into the modern protocol; legacy mode also works:

```toml
[features]
mcp_2026_07_28 = true
```

Merge this into an existing `[features]` table rather than declaring it twice.

## Tools by task

| Task | Tools |
|---|---|
| Cluster and provider inventory | `zz_overview`, `zz_nodes`, `zz_providers` |
| Provider installation | `zz_provider_install` |
| Model discovery, download and deletion | `zz_model_search`, `zz_model_files`, `zz_model_download`, `zz_model_delete`, `zz_models` |
| Run lifecycle | `zz_run_preview`, `zz_run_start`, `zz_runs`, `zz_run_restart`, `zz_run_stop` |
| Smoke test and diagnosis | `zz_chat`, `zz_logs` |
| Follow or cancel work | `zz_job`, `zz_job_cancel` |
| Parameters | `zz_params_get`, `zz_params_set` |
| Updates | `zz_update_status`, `zz_update_check`, `zz_update_apply` |
| Other REST tasks | `zz_api_search`, `zz_api_get`, `zz_api` |

Start with `zz_overview` and `zz_providers`. Provider inventory includes disabled
inference providers and excludes model registries. `managed=false` means no
installed copy is owned by zzRouter; it does not forbid installation.
Successful installation enables the provider during finalization, before the job completes.
For an existing installation, use `zz_api` to PATCH `/zzrouter/v1/providers/{name}` with
`{"enabled":true}` and a `node` query when needed. Cloud onboarding uses POST
`/zzrouter/v1/providers/{name}/verify` after configuring credentials.

Check `zz_models` first for an available local model. Only if it is absent,
search with `zz_model_search`; for a HuggingFace GGUF, choose a variant and
size with `zz_model_files`. A split variant is one choice: its `file` fetches
every shard, and the engine loads them together. To give a model vision,
download with `features: [vision]`; `zz_model_files` lists the vision file
that fetches. Download with `zz_model_download` and follow the jobs,
then read `zz_models` on the target node for the exact local name before
`zz_run_preview` and `zz_run_start`. Existing catalog `details.wire_endpoints`
and `details.features` report serving capabilities when known; they are not
a guessed model task. The download's registry model name
is not necessarily the local name. A `node` input is a node name, never an
address. Lists default to 50 results; use `limit` and `offset` to bound output.

`details.features.tools` is `present`, `available`, `unsupported` or `unknown`.
Only present projects to `/v1/models` `capabilities.tools: true`; false does not
block requests. Running chat evidence survives configuration patches until a
restart. Use `zz_chat` with `probe: tools` for request-local automatic-choice
verification and diagnostics; no probe verdict is saved in the catalog.
`zz_run_preview` preserves the node's capacity `assessment`, including its reason
and optional MiB amounts. Only `fit` confirms capacity; `unknown` is uncertainty,
and `no_fit` is a refusal by the shared memory policy. Preview never launches,
evicts or reserves memory, and current load can still prevent a later launch.
`zz_nodes` reports inventory; comparing file size with its memory does not prove
fit. Use `zz_api_get` on `/zzrouter/v1/nodes/compatible` with the exact `model`
and optional `provider`/`registry` to read confirmed nodes and all
`metadata.assessments` unchanged. Missing worker fields or replies are unknown.
Storage/download eligibility remains independent of capacity uncertainty.

Use `zz_run_preview` to inspect `context_mode`, `context_tokens`, estimated
`affordable_context_tokens` and llama.cpp `placement`. Auto vLLM and MLX assess
the declared 4096 minimum; auto llama.cpp assesses native GGUF context. Use
`zz_run_preview` numeric context overrides for larger vLLM/llama.cpp workloads.
MLX always assesses its minimum; `max-tokens` limits generation, not context.
MLX uses the tier-resolved `MLX_MEMORY_FRACTION` environment value (default 0.75).
Affordability never substitutes for served context. Missing geometry, native
range, devices or context-aware worker evidence is unknown. Fixed-budget
failures refuse launch without eviction. Auto context keeps greedy allocation.

An exact `zz_models.model` name or source-id filter returning one total model
also adds `show` to that entry. Case-insensitive exact matches qualify; globs,
suffix-only matches, truncated pages and duplicate replicas keep the list form.
Show metadata includes numeric context fields under `model_info`, template
presence and reported parameters/capabilities when known. Missing evidence stays
missing. Catalog `details.wire_endpoints/features` and source-reported metadata
remain separate; use `zz_chat` to verify a serving request.

Show requests retain the selected node, provider and physical source. A logical
variant reads its base's metadata and keeps its addressed name. Explicitly
unsupported detail retains the catalog entry with `show_unavailable`, an RFC 9457
problem; auth, inventory, transport and malformed metadata failures remain tool
errors. Full templates, model cards, licenses and tensors are omitted before
the metadata reaches the MCP response cap. Full REST/native show stays available.

Delete one physical model with `zz_model_delete`: use its exact `name` from
`zz_models`, plus `node`, `provider`, `registry` and `source_id` when needed to
identify one replica. Globs and suffix or repository aliases do not select weights
for deletion. Incomplete physical models can be cleaned up. Cloud models, groups and logical
variants are configuration: remove their entry with the supported configuration
tools instead. A 409 `model_in_use` identifies the holding run to stop; a 409
`model_referenced` identifies a variant to remove with `zz_params_set`.
After success, query `zz_models` on that node to verify absence.

Use an `idempotency_key` for deletion retries. A failure with
`mutation_applied: true` and `deleted` reports completed effects and is replayed
with the same key. Inspect the catalog before starting a new operation. Other
failures remain retryable. Replay has the same bounded, in-memory lifetime as
job-starting operations below.

## Follow jobs

Job-starting tools return IDs immediately. `zz_run_start`, `zz_run_restart`,
`zz_model_download`, `zz_provider_install` (including `preflight_only: true`)
and `zz_update_apply` replay successful REST responses when retried with the
same `idempotency_key`, body and query on the same route by the same caller.
Replay uses a bounded, in-memory cache for up to 24 hours and does not survive
server restarts. Only an accepted request is retained, and replay returns that acceptance:
for a job-creating tool, the same `job_id`, even if that job later failed.
A retry after an error runs again; after a failed job, use a new key.
Use a fresh key to rerun preflight checks.

Omit `job_id` from `zz_job` to list recent jobs on the coordinator, or set
`node` to list one serving node's jobs. Use `limit` and `offset`; results include
`jobs`, `total`, `has_more` and the answering `node`. Lists sort by most recent
update, then job id, so an unchanged collection can be paged without duplicates.
This does not fan out across the cluster. `wait_seconds` requires a `job_id`.

Call `zz_job` with each returned `job_id` and
`wait_seconds: 45`; if it is still running, call again. Downloads return jobs
per node. Follow a launch's `deploy_job_id` too when present. Keep the
`job_id` to cancel an individual download through `zz_job_cancel`, or use
`deployment_id` to cancel all deployment nodes. Cancellation is acknowledged
before cleanup completes; follow each job with `zz_job`. A cancelled download
reports `failed` with `meta.transfer_status: cancelled`; an already completed
job stays successful. Completed files survive and unfinished catalog files
stay pending. Repeat `zz_model_download` with a fresh idempotency key to repair
missing files. Downloads survive the accepting request and silence, bounded by
their 48-hour deadline or explicit cancellation. After launch succeeds, use
`zz_chat` for one short smoke test. Pass `run_id` to `zz_runs`, `zz_logs`, `zz_run_restart` or `zz_run_stop`;
pass `job_id` to `zz_job`. The stop reply confirms completion. Stopped runs are
removed, so a later run lookup returns 404 by design; its detail says where the
run's log stays readable. Launch job results omit the serving node's local probe
URL because it is not a coordinator-facing URL. A launch that stopped idle runs
to free GPU memory names them (`run_id`, `model`, `freed_mib`) in
`evicted_runs`, both in its `zz_job` result and in `zz_runs`, and a lookup of an
evicted run names the run that evicted it while that run is live.

`zz_chat` accepts `model`, optional `node`, and `probe: chat | tools | embed | vision`.
The default is `chat`, which requires `message`, uses 64 reply tokens by default,
and caps `max_tokens` at 256. A nonempty reply verifies this request; a truncated
reply is inconclusive.

`probe: tools` replaces `tools: true`. It sends one fixed harmless `lookup_weather`
function requiring a string `city` argument through the same chat route with
`tool_choice: "auto"`. It uses its own Paris weather message and a fixed 4096-token
budget, ignoring caller `message` and `max_tokens`. The tool is never executed.

`probe: embed` sends one fixed sentence through `/v1/embeddings`. A successful
probe requires one nonempty finite numeric vector and returns `dimensions`,
never the vector. `probe: vision` sends two different 16-by-16 solid-color images
with the same neutral question in two chat requests, each capped at 256 reply
tokens. Both answers must match their image. A constant answer cannot verify
vision; truncation or a wrong answer is inconclusive. Fixed probes ignore caller
`message` and `max_tokens`.

Results include `probe`, a request-local `verdict` (`verified`, `not_supported`
or `inconclusive`), and `next_steps`. Chat modes include `reply`, `model` and
`usage`; vision's usage combines both requests. Tools also returns `tool_call`:

| `tool_call` | Evidence and next step |
|---|---|
| `verified` | The first choice contains a structured call naming the probe, with JSON arguments carrying a nonempty string `city`. Tool requests can use this model. |
| `parser_missing` | No structured calls, but content names the probe. Use `zz_params_set` for this model's provider tool-calling parameters from the publisher's serving instructions, then `zz_run_restart` and probe again. |
| `truncated` | The first choice finished with `length`, taking precedence over other evidence. Inspect logs and publisher reasoning settings, then probe again. |
| `inconclusive` | No valid probe call. Probe again or read the model card. |

Classified refusals remain tool errors with their REST status, `code` and problem
fields. Only typed `unsupported_input` and `tool_calling_not_enabled` refusals
add `probe`, `verdict: not_supported` and `next_steps` to that problem. Auth,
routing, transport, malformed responses and other failures keep their errors;
error prose never establishes a capability verdict. Existing refusals include
`tool_calling_not_enabled` and `chat_template_rejected`. The former's remedy
names vLLM's boolean `enable-auto-tool-choice: true` and an operator-selected
`tool-call-parser` from the publisher's per-family serving instructions; set
them with `zz_params_set`, restart the run and probe again. Template refusals
carry the existing template remedy. No probe result is stored on a run. Verification proves only the fixed request,
not universal capability or a model family's declared support.

Create a logical variant with `zz_params_set` using the existing merge patch:

```json
{"provider":"llamacpp","patch":{"models":{"my-variant":{"from":"physical-base","parameters":{"ctx-size":8192}}}}}
```

Use a physical base name from `zz_models`; variants cannot point to another
variant. Optional `parameters`, `environment` and `request` settings belong
beside `from`. Remove your variant with `{"models":{"my-variant":null}}` in
`patch`. `zz_params_get` lists accepted provider parameters and their types.

`zz_api_search` searches the embedded [OpenAPI contract](../internal/server/openapi.yaml) and
returns methods, paths, operation IDs, summaries and each parameter's name, location
and required flag. An operation a task tool uses carries `task_tools`, naming that
tool (`getJob` carries `["zz_job"]`); the reply text says to call it instead. The
names come from the operation IDs each tool declares, so a new tool marks its
routes without another list.
`zz_api_get` reads a GET route; `zz_api` handles other methods and carries a
destructive hint. Its `idempotency_key` is forwarded to REST and replays only
on operations whose OpenAPI parameters declare `Idempotency-Key`. These include
the job-starting routes above, `POST /runs/ensure`, model-group PUT/PATCH/DELETE
mutations (group, replica and owner release), and authenticated, non-streaming
`POST /v1/chat/completions`, `POST /v1/completions` and `POST /v1/responses`.
The `zz_api` escape hatch itself admits only eligible public management routes.
Both refuse internal, MCP, streaming, retired and unmounted
routes and require literal paths without URL encoding or traversal. A route a
task tool uses is still served, because a tool may not expose every option of
its route; the reply adds `task_tools` and text such as "zz_job serves this
route; use it next." Failures carry no such hint. Put query
parameters in `query`; use `content_type: "application/merge-patch+json"` for
parameter merge patches.

Provider model files use these escape-hatch tools; see the
[ComfyUI inventory, download, delete and workflow recipe](comfyui.md#agent-recipe).
Their download and resolve POSTs accept `idempotency_key`. Follow each accepted
job with `zz_job` on its returned node, then verify the filesystem inventory.

## Tool errors

Application failures return `isError: true`. `content[0].text` keeps a human
explanation, and `structuredContent` carries the RFC 9457 Problem Details
object. REST Problems retain their `type`, `title`, `status`, `code`, `detail`,
`instance`, `request_id`, `node` and per-key `errors[]` (including `key`, `code`,
`got` and `want`). Branch on the problem's `code`, not the text.

Non-problem REST error bodies use the HTTP status slug (such as `bad_gateway`)
and a bounded body excerpt as `detail`. MCP validation, response limits,
streaming refusal, response decode failures, interrupted requests and internal
failures use the [MCP error codes](agent_error_codes.md#mcp-tools).
SDK argument-schema and protocol failures retain the SDK's own error behavior;
they do not acquire a synthetic problem from this middleware.

A `zz_job` response describing a FAILED job is a successful state snapshot,
with the job's error in its `error` field. Reaching the polling wait limit also
returns a successful snapshot; call `zz_job` again while work is running.

## Conformance

Against a running dev coordinator:

```sh
scripts/mcp-conformance.sh http://127.0.0.1:9091/zzrouter/v1/mcp
```

The official suite cannot send an API key. Target a local key-injecting proxy
or a loopback test setup, as described in the script. Preserve the incoming
`Host` header so the DNS rebinding checks exercise the server's host validation.
The suite is pinned to
`@modelcontextprotocol/conformance@0.1.16` with the checked-in expected-failure
baseline; this version has no 2026-07-28 server scenarios. Add
`--spec-version 2026-07-28` when a release supports them. Fixture-only tools and
unsupported MCP features remain baseline failures.

### OpenTelemetry

When tracing is enabled, MCP carries its HTTP trace parent into the inner REST
request through W3C headers. It retains cancellation and the existing context-value
isolation. See [OpenTelemetry configuration](observability.md).
