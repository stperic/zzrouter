# Changelog

## Unreleased

- Share artifact delivery, verification, extraction, link activation and release
  packaging through `zzinstall`. Keep service privileges, launcher pairing and
  rollback history in zzRouter. Local integration requires the sibling module
  until its immutable published dependency is available.

- `pinned_version` on an on-demand or external provider is one pin for every
  node. A pin sent with `?node=` to a worker used to land in that worker's copy
  of the coordinator's provider tree and be put back by the next sync. The
  coordinator now records the pin and the sync carries it; a pin sent with
  `node` answers 400 `pin_is_cluster_wide` and writes nothing. A body that
  also sets `enabled` still applies the enable to each node it names. Cloud
  providers and registries keep a pin per node.
- `service/start`, `service/restart` and a restarting `service/apply` answer
  202 with a `svc_` readiness job instead of waiting up to 30 s for the
  endpoint. Through the coordinator that wait always lost the 30 s forward
  deadline, so a start that succeeded answered 502 and a retried restart killed
  the warming daemon. The job ends done on the endpoint's first 200, or failed
  on an exit after the start, on `service/stop` or disable, on cancel (which
  stops the service) or on shutdown; it sets no deadline and its heartbeat
  reports `running` and `pid`. `service/status` names a live job in `job_id`,
  and a start or restart while one is live returns it. Refusals stay
  immediate; stop, and an apply without a restart, still answer 200, and the
  apply body drops `healthy`. A stop or restart zzRouter commands through
  systemd, launchd or NSSM is no longer recorded as an unexpected `last_exit`.
  Only `DELETE /jobs/{id}` stops a run or service now: a job reaped for
  inactivity no longer stops the run it was watching. The coordinator relays
  either answer, so the cluster protocol is unchanged.
- A provider enabled on a worker, by install, uninstall or
  `PATCH /zzrouter/v1/providers/{name}?node=...`, stays enabled: the
  coordinator's next provider sync used to put its own value back. The choice
  is now the node's, recorded by the coordinator as `nodes.<node>.enabled` over
  the provider's top-level `enabled`, which remains the default for nodes that
  have not chosen. Every enable through the API or `zzrouter-node providers
  enable` is now the node's own, including on the coordinator and standalone
  nodes, so a coordinator install no longer enables the provider on workers;
  on a worker the CLI refuses and names the coordinator request. Cloud
  providers and registries, which sync never carries, stay each node's own. A
  choice recorded but not yet delivered answers 503 `provider_sync_pending`; a
  worker relays the coordinator's status and code instead of a 500. A worker's
  finalization succeeds once the sync carrying its choice has reached it, and
  no longer waits behind another worker that is slow to answer: the provider
  sync's protocol probe no longer holds a push slot. Ollama auto-enable on a
  worker cannot record its choice at boot, before the worker reaches its
  coordinator; install it or enable it with `?node=`. Cluster protocol
  advances from 14/minimum 13 to 15/minimum 15: upgrade every node together.
  See `docs/protocol_versioning.md`.
- Retain engine-reported cache observations on REST runs and both `zz_runs` modes:
  per-pool KV tokens, optional pool count, slots, context, explicit sharing and
  fractional concurrency. Capture provider-declared startup summaries without
  changing readiness or launch decisions; omit absent or ambiguous KV evidence.
  Invalid optional cache-log declarations warn without blocking launches or
  provider synchronization. Extend the agent campaign's grounding and source
  attribution, including shared-slot overclaims
  and scheduler caps. Optional observations keep cluster protocol 14/minimum 13;
  older peers omit them correctly (see `docs/protocol_versioning.md`).
- A managed provider daemon now starts at boot when the host service-manager
  probe (nssm, systemd, launchd) answers slowly: the probe uses the provider's
  reconcile budget instead of failing after 3 s and waiting for the next 10 s
  services tick. A probe that cannot answer is an error naming the probe, never
  an absent service, and the resolved supervisor, including zzRouter itself, is
  remembered, so status reads no longer re-run host commands.
- Add a disabled, zzRouter-installable provider for Niko1221/Strata with
  managed service controls. Enabled external OpenAI-compatible providers now
  publish their `/v1/models` inventory in the catalog the way external Ollama
  providers already do, under the same `name@node` identity; zzRouter refuses
  to delete those provider-owned models. `service.manager: none` now selects
  zzRouter supervision. Strata Messages uses Chat Completions compatibility
  until its native tool-result image contract is verified. The
  cluster-internal wire schema is unchanged, so cluster protocol remains
  14/minimum 13.
- Explain unknown capacity assessments with thirteen closed causes and MCP next
  actions. Retain exact local weights rounded up to MiB on every status and
  independently known capacity on unknown rows, preserving fit and admission
  decisions. Retain vLLM Mamba-cache block counts and the logged sequence bound
  in actionable run/job failures. Add the evidence-checked explainable-fit agent
  campaign with owned-run cleanup. Cluster protocol advances from 13/minimum 12
  to 14/minimum 13: upgrade the coordinator first, then protocol-13 workers;
  older assessments normalize conservatively and protocol-12 peers must upgrade
  before rejoining. See `docs/protocol_versioning.md` for rollout instructions.
- Size supported HF hybrid attention and GGUF qwen35/qwen3moe models through
  the shared fit and admission path. Count full-attention KV, all expert weights
  and conservative float32 recurrent states using schema-declared row counts.
  Missing geometry/rows and hybrid placement across multiple devices stay unknown.
  vLLM hybrid models stay unknown (vLLM sizes its state pool by max-num-seqs).
- Share unified-memory detection across local inventory and legacy peer snapshots;
  remove the unused mesh HTTP adapter and broadcast serialization.
- `POST /zzrouter/v1/keys` checks the whole request before writing anything. A
  bad `expires_at` used to leave the key's personal team behind until the next
  restart, so a corrected retry with the same id answered 500 "team already
  exists"; a duplicate key id answered the same 500 and now answers 409.

## 0.2.3 (2026-10-16)

- Dependency updates: Bubble Tea v2.1.0, gRPC v1.84.0, Prometheus client
  v1.25.0, golang.org/x/crypto v0.58.0 and golang.org/x/net v0.61.0. The release
  workflow uses upload-artifact v7.0.2 and download-artifact v8.0.2.
- On Windows, stopping a process that has already begun exiting no longer
  reports a kill failure (TerminateProcess answers access denied while the exit
  completes).
- Cluster protocol 13 (minimum 12): upgrade workers to this release to get
  context-aware fit answers from them; older workers keep working and answer
  `unknown`.
- llama.cpp and the MLX vision runtime serve Anthropic Messages through the
  translator instead of their native endpoints, which mishandle images inside
  `tool_result` blocks (llama.cpp refuses the turn, mlx-vlm drops the image).
  Existing installs pick this up on reconcile.
- Cancelled Hugging Face downloads and peer sync publish retained manifest-only
  repositories as incomplete models before their jobs turn terminal. REST and MCP
  can list, repair or delete them. Deletion immediately refuses an active writer
  with 409 `download_in_progress`; completed catalog names stay unchanged.
- Capacity previews and launch admission share context-aware sizing: auto vLLM
  and MLX assess a declared 4096-token minimum; llama.cpp reads bounded GGUF
  geometry and native context, honors quantized KV and reports GPU or offload
  placement. MLX uses a release-owned 0.75 unified-memory fraction. Assessments
  include context and estimated affordability; unsupported evidence stays unknown.
  Native llama.cpp estimates beyond GPU plus RAM allow engine adaptation;
  numeric-context shortfalls still refuse launch. Schema-declared KV encodings
  include vLLM fp8, and bounded GGUF reads skip arrays in bulk.
  Fixed-budget failures refuse launch without eviction. Cluster protocol adds
  context evidence while retaining rolling compatibility with older workers,
  whose assessments degrade to unknown.
- Standalone provider install preflight reports unsupported OS or architecture
  as a failed `platform` check with a remediation hint, before host probes.
  Reports still return HTTP 200 and use the selected node's installer and requirements.
- Preview, start and cold inference refuse malformed or unreadable feature
  evidence with 409 `model_feature_evidence_invalid`, including when the optional
  runtime is absent. Missing or negative evidence still uses the base runtime.
- Phrase-based upstream `unsupported_input` classification requires an
  image-bearing outgoing request. Text-only errors keep their upstream status;
  classified relay codes and other failure classes are unchanged.
- Job cancellation stops local and worker-owned downloads. Downloads survive
  silent upstreams under their existing 48-hour deadline. Both TUI views and MCP
  use `job_id` for individual downloads; `deployment_id` still cancels all nodes.
  Cancelled jobs report `failed` with `meta.transfer_status: cancelled`. Completed
  files remain, unfinished catalog files stay pending for repair, and provider
  destinations remain reserved through cleanup.

- Chat accounting adds `usage.cost` and `usage.cost_details.upstream_inference_cost`
  when present, including upstream charges for BYOK requests with zero account cost.
  Explicit `usage.cost: 0` is authoritative; legacy `x_*.cost` requires a positive
  value. Later stream cost updates replace earlier costs. Client chat responses
  omit vendor accounting fields after metering, including when metadata is off;
  provider costs retain their precision in responses and inference logs.
- Spend-limited inference filters unpriced targets before dispatch and returns
  403 `model_unpriced` when none remain. Explicit zero overrides and recognized
  local engines remain free after monetary exhaustion. Admission and settlement
  share pricing lookup, and admitted rates survive live pricing changes.

- Upgrade: limited keys or teams routing to an external self-hosted endpoint
  without a managed lifecycle now receive 403 `model_unpriced` until an operator
  posts a zero (free) or real pricing override. Enable `models.pricing.enabled`
  in `node.yaml` and restart if pricing is disabled. For a free endpoint, replace
  `my-external-provider` with its configured provider name and run:

  ```bash
  curl -X POST http://localhost:9090/zzrouter/v1/pricing/overrides \
    -H "X-API-Key: $ZZROUTER_ADMIN_API_KEY" -H 'Content-Type: application/json' \
    -d '{"provider":"my-external-provider","model":"*","input_cost_per_1m":0,"output_cost_per_1m":0}'
  ```

  Use a concrete model ID and real dollar rates per million tokens when billed.
- Node health combines probe and dispatch evidence across mesh, inference, jobs
  and provider sync. Three consecutive transport failures fail fast; recovery
  uses the existing probes after a 30-second cooldown and two full successes.
  REST and MCP agree, with one missed full probe retaining dispatch grace.
  Public-only degraded and half-open recovery nodes receive no ordinary traffic.
  Caller deadlines and errors after response headers leave node health unchanged.
- Tools capabilities use model and serving-run evidence. Ollama preserves its
  capability list, llama.cpp reads the running chat template capability, and
  vLLM reports the switches its chat run launched with. MLX and cold launched
  models stay unknown. Only `details.features.tools: present` makes
  `/v1/models` `capabilities.tools` true; use `zz_chat` `probe: tools` to verify.
  Serving snapshots survive patches until restart, and groups include unknown
  replicas. Cluster protocol and minimum rise from 11 to 12 for strict provider
  schemas; upgrade every node together using the
  [protocol maintenance procedure](docs/protocol_versioning.md).
  Cloud and external models without present evidence now report `tools:false`;
  cloud models no longer claim `tools:true` by default. False means no evidence,
  not refusal, and does not prohibit tool requests.
- Compatible-node queries return confirmed model capacity fits and report per-node,
  per-provider `fit`, `no_fit` and `unknown` reasons in `metadata.assessments`.
  Run previews and MCP `zz_run_preview` expose the same capacity assessment without
  launching or evicting runs. Cached weights alone no longer imply compatibility.
  Downloads retain independent storage eligibility. Launch admission also refuses
  weights exceeding the engine's reachable memory with HTTP 409 `out_of_range`.
- Cluster protocol rises from 11 to 12, with minimum 11 retained for this optional
  assessment change. Old or missing worker evidence becomes unknown. A rolling
  upgrade updates the coordinator first, then workers to obtain capacity assessments;
  verify matching node/launcher bundles and node health after each update.
- Stopping a newly launched run no longer depends on reading its environment during
  exec. Shutdown, orphan cleanup and managed-daemon migration verify PID plus an exact
  per-platform start token: raw Linux start ticks, kernel creation time on Darwin and
  Windows. Legacy PID files retain marker-based ownership checks. Orphan records stay
  tracked after a refusal or termination error until a later pass confirms exit.

- Pinned process runs return after node restarts and self-updates, replaying their
  request overrides with current provider tiers. Stop a run to forget its saved pin.
- Interrupted registry downloads and peer syncs keep local model copies incomplete
  until their pending files arrive. MCP `zz_models` exposes `incomplete`; repair
  with `zz_model_download` of the same model to fetch only what is missing.
- `/v1/models` `capabilities.max_context_tokens` reports the context a running
  replica serves, worker runs included; it is omitted while no run reports one,
  and for a group when any replica's is unknown (a cloud replica always is).
- llama.cpp runs report GPU/CPU placement from weight buffers when CPU tensor
  overrides are logged, otherwise from layers. Managed logging includes placement.
  MCP `zz_runs` exposes `processor` and `context_length`
  so agents can identify CPU offload and reduce GPU memory pressure.
- Runs, previews and inference refuse endpoints the provider does not serve
  with HTTP 400, including Ollama reranking. A warm chat run cannot absorb an
  unsupported embeddings request. Model-group chains select the requested
  endpoint for warm runs and cold starts.
- MLX supports embeddings and reranking through mlx-vlm. Install it with
  `POST /zzrouter/v1/providers/mlx/install {"runtime":"mlx-vlm"}`, then start
  runs with `endpoint: embeddings` or `endpoint: reranking`. Deployments can
  provision the runtime with `features: [embeddings]` or `[reranking]`.
  Chat, embeddings and reranking runs coexist. MLX now declares Completions;
  llama.cpp and vLLM declare their rerank APIs.
- MLX vision readiness uses uvicorn startup logs and a serving probe, and
  excludes unsupported sampling flags. Endpoint runs no longer replace the
  model catalog's chat endpoint evidence.
- Cluster protocol and minimum rise from 10 to 11 because older provider
  loaders reject endpoint features and feature health checks. Upgrade every
  node together using the [protocol maintenance procedure](docs/protocol_versioning.md).

- Negative resolved launch `keep-alive` (for example `-1`) pins a run against
  zzRouter idle expiry and pressure reclaim. Set it through provider tiers or
  run-start parameters. Inference `keep_alive` hints still update idle timers,
  but never pin or unpin. Run reports expose `pinned: true`; a blocked launch
  keeps 409 `out_of_range` and names pinned holders with
  `reclaim_skipped_reason: pinned`. Explicit stop, Force replace and restart
  remain available; restart replays request overrides. Upgrade every worker before relying on pins:
  an older worker keeps `-1` as idle-only and may reclaim it under pressure.

- vLLM and llama.cpp size context to the GPU by default: new installs ship
  `max-model-len: auto` and `ctx-size: auto`, and the engine picks the largest
  context that fits (up to the model's own maximum). A number still sets it,
  per tier or per launch. Existing installs keep their configured value; set
  `auto` to opt in. Runs report the context the engine serves in
  `context_length`. With `gpu-memory-utilization: auto` as well, vLLM takes the
  free GPU memory; set a numeric context to share a GPU. Upgrade workers before
  using `auto`: an older worker drops it, and vLLM then starts at the model's
  maximum context.
- OpenTelemetry tracing and OTLP export are coordinator-only. Workers reject
  tracing or a collector endpoint during configuration load, save and startup;
  local Prometheus metrics remain supported. Switching a tracing/exporting
  coordinator to worker mode requires disabling tracing/export and restarting
  before the mode change.

## 0.2.2 (2026-10-14)

- Builds on Go 1.26.9 and golang.org/x/net v0.60.0, which fix reachable
  vulnerabilities in the standard library's net/http, crypto/tls,
  html/template, mime/multipart and os packages.
- The Windows release embeds provider templates and other bundled files with
  LF line endings, the same bytes as every other platform.
- Managed Python install errors use LF line endings on every platform.
- Optional OpenTelemetry tracing supports authenticated OTLP HTTP/protobuf and
  gRPC, parent-based sampling, safe HTTP metadata and connected dispatch/worker/MCP
  spans. Local metrics can remain enabled for trace-only collectors. Tracing stays
  disabled by default. See [configuration and activation](docs/observability.md).

## 0.2.1 (2026-10-14)

- Managed Linux update history reads the privileged updater's record through both the CLI and API.
- macOS process enumeration and PID lookup share bounded recovery for transient ENOMEM reads.
- Managed Python failures put pip's ERROR lines first and retain the captured command output.

## 0.2.0 (2026-10-14)

### Highlights

- ComfyUI custom-node Python dependencies can be selected by the human operator
  through `additional_packages` in the existing protected install policy. Force
  reinstall uses one hashed, wheel-only resolution; environment status reports
  drift. Node code and system dependencies remain operator-owned, and node
  `install.py` scripts are never executed by zzRouter.

- ComfyUI has a managed install on Linux/amd64 NVIDIA nodes through the existing
  provider install API, MCP and TUI. The shipped 0.40.0 source archive is SHA256
  pinned; dependencies use the hashed Python inventory. Upgrade replaces source
  and venv together, and uninstall keeps data and operator Python directories.
  See [installation and hand-install move-aside instructions](docs/comfyui.md#install-and-enable).

- ComfyUI model files can be listed, downloaded from Hugging Face, deleted and
  resolved from workflow metadata through the coordinator API, MCP `zz_api`,
  and `zzrouter providers models`. Files belong to the provider and stay outside
  the LLM catalog. See the [operator and agent recipe](docs/comfyui.md#model-files-through-the-coordinator).

- ComfyUI as a GPU tenant ([docs/comfyui.md](docs/comfyui.md)). zzRouter
  launches ComfyUI on a Linux NVIDIA node (`POST /zzrouter/v1/runs` with
  `provider: comfyui` and a `node`, no model) so its GPU memory joins the
  same admission, idle stop and eviction as LLM runs. `gpu-reserve-mib` is a
  reservation: an LLM launch that would need its unused part is refused, or
  evicts an idle ComfyUI run when eviction is on, and the reverse. While
  ComfyUI's queue is busy its run is neither idle nor evictable. Clients call
  ComfyUI directly at the run's `base_url`; zzRouter does not proxy it. The
  provider ships disabled and is enabled after a successful managed install.
- Ollama preloading supports embedding-only models through a capability-neutral empty-input request.

- MCP server for coding agents: `POST /zzrouter/v1/mcp` (coordinator, admin
  key) serves task-level `zz_*` tools for models, providers, runs, jobs and
  updates, plus a spec-searchable escape hatch. Setup in
  [docs/mcp.md](docs/mcp.md). Tool errors carry the RFC 9457 problem in
  `structuredContent`; tools that accept an `idempotency_key` honour it.
- Ollama models are runs. A model Ollama loads through zzRouter gets a real
  run id, a job, a log and a place in `/runs`, and is started, reused, stopped
  and restarted like a llama.cpp, vLLM or MLX run. `/api/ps` lists these runs.
  Models other clients load are adopted as runs (`started_by: external`); when
  Ollama unloads a model to fit another, the load that displaced it reports it
  in `evicted_runs`. A keep-alive is sent to Ollama only when one is set
  (`-1` pins as a number: the string `"-1"` was refused, so pinning through
  the API did not work before).
- Every run says what started it (`started_by`: `api`, `request` or
  `external`) and keeps its log on its node after it stops; a lookup of a
  stopped run's id says how to read it, and an evicted run's id names the run
  that evicted it.

### Behaviour changes

- Cluster protocol and minimum advance from 9/9 to 10/10 for source install
  recipes, which older strict provider schemas reject. Upgrade all nodes together
  using the maintenance procedure in [protocol versioning](docs/protocol_versioning.md).
  There is no rolling compatibility window.
- Shipped provider platforms and installation prerequisites now refresh with
  their recipes during reconciliation; enabled state and node tuning remain.
- Cluster protocol and minimum advance from 8/8 to 9/9 for provider model-store
  configuration and download targets. Upgrade all peers together during the
  maintenance window in [protocol versioning](docs/protocol_versioning.md);
  older peers refuse the new protocol. There is no rolling compatibility window.
- Managed update CLI apply and rollback submit through the API's handoff when
  run without root privileges. Root uses the same privileged executor with
  restart/health checks and refuses a busy updater. Submission permission errors
  explain the service-account and root options.
- Managed Linux updates include the operator client with the node and launcher.
  Rollback restores the matching client; legacy versions without one retain the
  working client and prune keeps its directory. New managed releases without a
  runnable matching client are refused. See [installation](docs/install.md) for
  adopting this behavior on an older service install.
- Providers can declare `run_scope: node`: one model-less run per node,
  started with `provider` and `node` and no `model_name`. Run reports carry
  `provider` and `base_url` (the node's reachable address); an `evicted_runs`
  entry always names its provider and omits `model` for model-less runs.
- The launcher applies a provider's `execution.working_dir`. The vLLM,
  llama.cpp and MLX templates carried an unused `working_dir: ./models` that
  would now break launches; it is removed, and reconcile removes it from
  installed configs.
- Cluster protocol and minimum advance from 7/7 to 8/8: older workers reject
  `run_scope`, `runtime.activity_probe` and the `reserve_mib` memory budget
  during strict provider loading. Upgrade all peers together using
  [protocol versioning](docs/protocol_versioning.md).
- Reservation allocator complements preserve exact fractional GiB, so rounding no longer gives an engine more memory than its accounted budget.
- Run launch, load and preview parameters accept JSON strings, numbers and booleans. Number literals are preserved when normalized to strings; null, objects and arrays are refused.

- Missing-job GET, cancellation and pre-stream failures return scoped
  `job_not_found` Problems at their owner. Proxied errors preserve retry metadata
  and use the public request identity. Job lists, snapshots and cancellation
  replies validate their expected success shape; transport failures use the
  shared sanitized routing error codes.
- Campaign cleanup distinguishes proven pre-dispatch download refusals from
  accepted or uncertain work. Captured acceptance markers retain conservative
  ownership checks; failed calls and confusion findings remain recorded.
- Deployments retain worker-job progress and terminal outcomes before any client
  read. Peer sync follows its own immutable job, and cancelling a deployment
  cannot stop a newer download using the same key. Per-node cancellation reports
  `cancelled`; tracking loss reports an unconfirmed worker outcome. Restart
  chains use retained placement outcomes after worker-job expiry.
- Cluster protocol and minimum advance from 6/6 to 7/7: asynchronous placement
  requires an observable job before work starts, with transfer evidence retained
  through terminal publication. Upgrade all peers together using the maintenance
  procedure in [protocol versioning](docs/protocol_versioning.md). This source
  change has no rolling compatibility window.

- Queued runs have readable logs before admission. Their log paths are fixed
  before publication, so concurrent log reads cannot race the launch.
- Launcher shutdown signals are captured before child creation. Signals and stdin
  closure share one bounded stop path, and exit removes only successfully
  published PID metadata while preserving prior metadata after a write failure.
- Readiness, lifecycle logs and audit events commit together, so a retired start
  cannot later announce itself as running. Stopping an adopted model also unloads
  it when Stop wins before readiness publication.
- Cold inference pinned to the local node binds its provider before selecting the
  native or translated API. Provider assignment by name or metadata hints happens
  during catalog publication, including models without a detected format.

- Inference retains a daemon load's failure when it finishes before startup
  handoff. Completed runs keep their actual outcome after the startup deadline.
- Warm local runs retain their provider and launched wire endpoints after catalog
  invalidation, so translated Messages and Responses stay on the correct API.
- MCP startup guidance names job collections and capability probes, limits
  idempotency keys to declared tool inputs, and distinguishes source metadata
  from serving details. Vision probes allow
  up to 256 tokens per image so reasoning models can reach a visible answer.
- Run and provider refusals use one wire table for status, retry delay and
  diagnostics. Cold-load responses retain memory-budget details and explicit
  run/job extensions; streaming and native dialects keep their existing shapes.

- Run idle expiry uses one registry reaper. Held requests survive their idle
  window; zero keep-alive ends after release and negative values pin against
  idle expiry. `keep-alive` and `evict-idle-on-memory-pressure` resolve through
  provider, model, node, node × model and request parameters. Runtime keep-alive
  and the node GPU eviction option supply compatibility defaults. Settings
  changes appear in affected-run reports and apply on restart.
- Cluster protocol and minimum advance from 5/5 to 6/6 for tier-resolved run
  settings. Older workers can ignore idle overrides or forward policy flags
  to engines. Upgrade every peer together using the maintenance procedure in
  [protocol versioning](docs/protocol_versioning.md).

- MCP `zz_models` returns compact model detail for one exact name or source-id
  match. Context metadata, template presence, defaults and source-reported
  capabilities stay separate from serving probes. Same-name physical sources
  now require an exact source identity; identical names on different Ollama
  backends remain separate catalog entries. Native show preserves its selected
  backend and upstream error status.
- MCP `zz_model_delete` deletes one exact physical model with node and source
  selectors. REST, Ollama and OpenAI deletion share the same guarded path:
  held weights and configured variant references return actionable 409 errors.
  Starts, queued admission and daemon adoption share physical deletion ownership.
  Retries replay completed deletions and failures that report applied effects.
- Cluster protocol and minimum advance from 3/3 through 4/4 to 5/5: internal
  model show retains source identity, and deletion requires exact worker selection
  and frozen physical run reservations. Upgrade every peer together using the maintenance
  procedure in [protocol versioning](docs/protocol_versioning.md).
- MCP `zz_job` can list recent jobs when `job_id` is omitted. Local and
  node-selected lists share deterministic paging; waits still require an id.
- MCP `zz_chat` replaces `tools: true` with `probe: tools` and adds `embed`
  and `vision` probes alongside default chat. Results carry request-local
  evidence and next steps; classified capability refusals preserve their errors.
  `zz_params_set` now explains how to create logical variants.

- Overload answers 503 with `Retry-After` (codes `capacity_exceeded` and
  `queue_wait_expired`), not 429. A request waits at most 30 seconds for a
  run's slot by default.
- Streams have a 10 minute first-byte and a 2 minute idle deadline; a stalled
  engine stream is cut and frees its slot. A stream that ends early ends with
  an error in the client's dialect, never a fabricated `[DONE]`; buffered
  answers are whole or a 502.
- A failed request is replayed to another replica only when the upstream
  never received it; a written failure answers 502 and still cools that
  replica down. An explicit `max_retries: 0` means no retry.
- Ollama: a second provider on the same daemon gets 409 for a model another
  provider's run holds; installing, upgrading or uninstalling Ollama is
  refused while it has runs, as for every engine. Run listings gain
  `expires_at` and `evicted_runs`.
- A launch that must wait for another one still claiming GPU memory is queued
  and reported as `launch_in_progress`. vLLM's automatic memory fraction is
  sized from what the model needs.
- A node refuses to start on a provider config that fails to load, and
  `start -d` waits until the node serves or fails.

### Fixes

- Rollback to a release containing the ownership-receipt reader repairs managed
  provider keys before strict loading and archives newer-only shipped providers.
  Supported operator edits remain. The compatibility floor is v0.2.0;
  v0.1.0 and other pre-reader builds require
  [manual configuration recovery](docs/install.md#manual-recovery-for-targets-before-v020).
- Provider preparation failures count as self-update boot attempts and refuse
  startup, including repair I/O failures whose remaining config still decodes.
- Provider sync checks fresh worker protocol evidence over mTLS before each
  round. An incompatible or unreadable peer receives no provider POST and stays
  missed in sync reports, even when cached as UP. Receiver validation still
  refuses incompatible bytes before disk writes if a worker changes after probing.
- GPU memory refusals explain why reclaim skipped a run (busy, in use or not idle),
  and unused commitments omit process IDs. Evicted-run lookups name a model-less
  evictor by its provider, using the same display name as the CLI.
- A request-tier `keep-alive` parameter no longer reaches a launched engine's
  command line; `keep_alive: 0` now ends a run when its last request finishes.
- A managed daemon whose start was refused at boot (the previous node's
  daemon still exiting) is retried until it starts.
- A finished download, a run start or stop and every catalog mutation publish
  the catalog before they answer; a model without its weights is never served.
- One resolver decides which node serves a model name; one table decides how
  every typed failure answers.
- A rollout that would split the cluster protocol is refused; a sleeping
  unselected peer no longer blocks one.

### Configuration, protocol and build

- Add optional `readiness_probe.stall_timeout` to detect silent engine starts.
  vLLM now allows 900s overall and fails after 240s without output; existing
  installs receive these values through managed provider reconciliation.
- Cluster protocol and minimum advance from 2 to 3: older workers reject
  `readiness_probe.stall_timeout` during strict provider configuration loading.
  Upgrade all peers together following
  [the protocol maintenance procedure](docs/protocol_versioning.md).

- Add opt-in node GPU memory reclamation through
  `gpu.evict_idle_on_memory_pressure` in `node.yaml` (default off). Launches that
  cannot fit stop eligible idle runs in activity order, wait briefly for memory
  release, and retry admission once. Keep-alive expiry shares the eligibility
  check and cannot claim a run with an active lease.

- Derive Messages and Responses translations for all providers declaring
  Chat Completions without the corresponding native API. Shipped provider
  configs now declare native endpoints only, and reconciliation carries that to
  existing installs. An explicit `*_compat` entry still loads and means the same
  thing. Catalogs and running snapshots report the same effective endpoints.
- Provider-declared engine failure classes now cover startup, process exit and
  inference. Memory exhaustion returns 409 `out_of_range`; input, template and
  tool-calling refusals return their specific 400 codes. Generic server errors
  keep their status. Existing installs receive the declarations through reconcile.
- Cluster protocol and minimum advance from 1 to 2: older provider schemas
  reject `runtime.failure_patterns`. Upgrade all peers together following
  [the protocol maintenance procedure](docs/protocol_versioning.md).

- Removed `model_defaults`, the release-managed per-model-family tier. No
  release shipped data in it after 0.1.0. Per-family settings belong in the
  operator's `models` tier, which accepts glob patterns. `/resolved` no longer
  reports a `model-default` tier. An install that still carries the block has
  it removed on start.
- Built with Go 1.26.8 (compiler, runtime and `net/http` bug fixes; no security
  fixes over 1.26.6).

## 0.1.0 (2026-10-08)

First public release.

- Peer sync uses the downloader's flat repository layout, preserving feature
  roles and reusing held files; forced sync fetches them again.

- Distributed model management through an authenticated API and terminal client.
- OpenAI, Anthropic Messages and Ollama compatible inference.
- Managed llama.cpp, vLLM and MLX runtimes, external Ollama and cloud providers.
- Virtual keys, teams, model access, quotas and usage accounting.
- Model features, provider parameter tiers and variants.
- No chat templates ship. When a model's own template refuses a request,
  llama.cpp errors come back as 400 `chat_template_rejected`, and
  `chat_templates` in `GET /zzrouter/v1` gives the API steps to install one.
- Cluster mTLS and signed updates from `stperic/zzrouter`.
- Every cluster node must run the same cluster protocol (`zzrouter-node version --json`).
- Built with Go 1.26.6; `govulncheck` reports no reachable vulnerabilities.
- Reproducible release builds: every platform of a tag carries the same version.

No private release history is included. Until 1.0, review compatibility notes
before upgrading.
