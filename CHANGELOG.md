# Changelog

## Unreleased

### Highlights

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
