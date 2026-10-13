# Changelog

## Unreleased

### Highlights

- ComfyUI as a GPU tenant ([docs/comfyui.md](docs/comfyui.md)). zzRouter
  launches ComfyUI on a Linux NVIDIA node (`POST /zzrouter/v1/runs` with
  `provider: comfyui` and a `node`, no model) so its GPU memory joins the
  same admission, idle stop and eviction as LLM runs. `gpu-reserve-mib` is a
  reservation: an LLM launch that would need its unused part is refused, or
  evicts an idle ComfyUI run when eviction is on, and the reverse. While
  ComfyUI's queue is busy its run is neither idle nor evictable. Clients call
  ComfyUI directly at the run's `base_url`; zzRouter does not proxy it. The
  provider ships disabled; installing ComfyUI is manual for now.
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
