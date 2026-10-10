# Agent error code catalog

Stable `code` values an LLM agent will encounter when driving zzRouter.
Codes are wire-stable: branch on them, do not parse `message`.
MCP application/tool errors carry these codes in `structuredContent.code`.

Two error envelopes are in play:

- `/v1/*` (OpenAI compat) : body is `{"error":{"message","type","code","param"}}`.
  Agents read `error.code`.
- `/zzrouter/v1/*` (management) : RFC 9457 Problem Details
  (`application/problem+json`) with a `code` field at the top level.
  Agents read `code`.

The serving node classifies declared engine failures before forwarding them.
Relays preserve that status and code across cluster hops. Classifications apply
at startup, process exit and inference; an unmatched upstream error keeps its
status. The class table is `pkg/httperr/engine_failure.go`, and provider phrasings
live only in `runtime.failure_patterns`.

## OpenAI surface (`/v1/*`)

| `error.code` | HTTP | Meaning | Recovery |
|---|---|---|---|
| `model_node_ambiguous` | 409 | Several remote nodes hold the name, and none locally. | Choose a `name@node` reference from the detail, or set X-Node. |
| `model_not_found` | 404 | The named model is not in the catalog. | List `/v1/models` to discover canonical names + SourceID aliases. If you expected a deploy, POST `/zzrouter/v1/runs` with `auto_deploy:true`. |
| `context_length_exceeded` | 400 | Prompt + max_tokens exceeds the model's context window. | Read `capabilities.max_context_tokens` from `/v1/models`; trim prompt or lower `max_tokens`. |
| `request_too_large` | 413 | Multipart upload exceeds the configured ceiling. | Split the upload or reduce file size. |
| `capacity_exceeded` | 503 | The instance queue is full or queueing is disabled. | Honor `Retry-After`; retry or use another replica. |
| `queue_wait_expired` | 503 | The instance slot did not become available within the queue wait. | Honor `Retry-After`; retry or use another replica. |
| `rate_limit_exceeded` | 429 | Per-key RPM/TPM cap or budget reservation rejected the request. | Honor `Retry-After` if present; back off. Check `/zzrouter/v1/keys/:id/usage` for current budget. |
| `invalid_request_error` (type) | 400 | Generic validation failure. `param` names the offending field. | Read `param`; fix and retry. |
| `upstream_stream_interrupted` | 502 | The upstream stream ended before completing. | Treat the partial answer as incomplete; retry the request if appropriate. |
| `backend_error` (type) | 502/503/504 | Upstream provider failed. | Check whether replay is safe before retrying a billed or side-effecting request; if persistent, inspect `runtime.status`. |
| `fallback_exhausted` | 502 | A transport failure could have occurred after the upstream ran the request. The group stops without replaying it. | Check upstream execution before resubmitting a billed or side-effecting request. |
| `upstream_timeout` | 504 | The model instance did not begin responding within its first-byte budget. The group stops without replaying it. | Check upstream execution before resubmitting; it may still be processing the request. |
| `out_of_range` | 409 | The `memory` class: admission refused the launch or the engine could not allocate memory during startup or inference. | Release memory or reduce the configured budget before retrying. Admission details identify managed runs and outside holders by PID. |
| `unsupported_input` | 400 | The `unsupported_input` class: the engine refused an input kind. | Use a model supporting that input, or remove it from the request. |
| `responses_not_supported_by_provider` | 400 | The model's provider declared no native or translated `/v1/responses` support (its effective endpoints omit both `responses` and `responses_compat`). | Use `/v1/chat/completions` for inference : tools, structured outputs, and streaming all work there. Providers that natively serve `/v1/responses` (vLLM, OpenAI cloud, etc.) declare it in their `config.yaml`; agents that hit those providers see passthrough. Providers declaring `chat_completions` without native `responses` automatically derive `responses_compat` and translate supported Responses requests to Chat Completions. |
| `tool_calling_not_enabled` | 400 | The `tool_calling_not_enabled` class: required launch switches are missing. | Set the provider's tool-calling parameters through PATCH `/zzrouter/v1/providers/{name}/parameters`, then restart the run. |
| `chat_template_rejected` | 400 | The `chat_template_rejected` class: the model's template refused to render this request. | Install and select a template that accepts it; follow `chat_templates` in `GET /zzrouter/v1`. |

Instance overload uses 503 and `Retry-After` in OpenAI, Anthropic and Ollama responses; 429 remains caller quota exhaustion. OpenAI failures after a stream opens carry the same code in band inside HTTP 200, where no new HTTP headers can be sent.

For limited instances, omitted `queue_timeout` waits at most 30 seconds; `"0s"` fails fast and `"caller"` explicitly waits until the caller context ends. At most twice `max_concurrent_requests` requests wait; the next one is refused at once with `capacity_exceeded`. Unlimited instances do not queue.

Group retries for POST and other non-idempotent methods require evidence that no request was written (including dial, DNS and TLS handshake failures), an unreachable deployment, or an upstream refusal with 429, 402 or 503. Written resets, broken pipes, timeouts and upstream 504 responses stop the chain. GET, HEAD, OPTIONS, PUT and DELETE retain broader transport and 504 retries.

The same engine codes reach the management surface when a launch fails.
Anthropic errors carry the code in `error.code`; Ollama errors retain their flat
`error` string with an optional top-level `code`.

## Management surface (`/zzrouter/v1/*`)

### Baseline codes

Every management failure carries `code`, so branching on it never needs a
fallback. Where nothing more specific applies it is a snake_case slug of
`title`:

| `code` | HTTP | Meaning |
|---|---|---|
| `bad_request` | 400 | Malformed body or parameter. Read `detail`, and `errors[]` when present. |
| `unauthorized` | 401 | Missing or wrong `X-API-Key`. |
| `forbidden` | 403 | Key authenticated but not permitted. |
| `not_found` | 404 | No such resource. |
| `conflict` | 409 | State prevents the operation : e.g. an upgrade with instances still running, or a provider already installed. |
| `service_unavailable` | 503 | Subsystem not wired or shutting down. |

`type` carries the same identity as a URI (kebab-case, so
`problems/not-found` pairs with `not_found`). Prefer `code`.

The more specific vocabularies below replace the baseline value when they
apply.

### Install preflight (`POST /providers/:n/install`)

| `code` | HTTP | Meaning | Recovery |
|---|---|---|---|
| `preflight_failed` | 400 | Prerequisites are not met on the target node. No job was created. | Read `errors[]`: each entry names the failing `field` (the check) and, where a remedy is known, a `hint` (e.g. "install curl"). Fix and retry, or call `POST /providers/:n/install/preflight` first to see the full report including the checks that passed. |

Preflight runs synchronously on the install request path, so a 400 here
means nothing was started : distinct from a job that starts and then
fails.

### Provider parameter validation (`PATCH /providers/:n/parameters`)

Closed-enum `code` values from `pkg/httperr.ParamErrorCode`. Body is
`{"errors":[{"key","code","got","want","min","max","message"}, ...]}`.

| `code` | Meaning | Recovery |
|---|---|---|
| `unknown_flag` | The patched key isn't in the provider's schema. | Read `/zzrouter/v1/providers/:n/schema` for the valid key set. |
| `wrong_type` | Value type mismatch (e.g. string into int). | Coerce to the type indicated by `want`. |
| `out_of_range` | Numeric value outside `min`/`max`. | Clamp into `[min,max]`. |
| `unknown_node` | Tier 3 cell references a node not in the cluster. | List `/zzrouter/v1/nodes`. |
| `unknown_model` | Tier 2/3 cell references a model not in the catalog. | List `/v1/models`. |
| `coercion_failed` | Value couldn't be converted to schema type. | Send the value as the declared type. |
| `retired` | Key was removed from the schema. | Remove from your patch. |
| `unknown_asset` | An asset-typed key names a file the provider does not have. | List `GET /zzrouter/v1/providers/:n/assets`, or upload the file with `PUT .../assets/:asset` first. |

### Provider assets (`/zzrouter/v1/providers/:n/assets/:asset`)

| `code` | HTTP | Meaning | Recovery |
|---|---|---|---|
| `invalid_value` | 400 | The asset name is not a single file name of letters, digits, `.`, `_` or `-` (no leading or trailing `.`). | Rename. |
| `asset_in_use` | 409 | `DELETE` of an asset a parameter still names; one `errors[]` entry per parameter, `key` its merge-patch path. | Point those parameters elsewhere (or `null` them) with `PATCH /providers/:n/parameters`, then delete. |

A body over 1 MiB, or a provider over 4 MiB or 64 assets, is a 413.

### Model groups / routes (`/zzrouter/v1/model-groups/*`)

Same `{"errors":[...]}` envelope and `pkg/httperr.ParamErrorCode` enum
as the parameter surface.

| `code` | HTTP | Meaning | Recovery |
|---|---|---|---|
| `reserved_name` | 400 | The group name equals a literal route segment (`schema`, `events`, `preview`, `reload`); creating it would make the group unreachable by name. | Pick a name outside the list in `message`. |
| `route_unknown` | 404 | The addressed model group does not exist. | List `/zzrouter/v1/model-groups`. |
| `replica_unknown` | 404 | The addressed replica is not in the group. | Read the group for its replica list. |
| `replica_last_in_group` | 409 | Deleting the last replica is refused. | Delete the group instead. |
| `route_not_claimed` | 409 | Only a claimed auto-route has an owner to release. | Claim before releasing. |

Teams, keys and provider instances reject reserved names too
(`schema`; `schema`/`reload`; `catalog`/`instances`) but answer with
the baseline Problem envelope, `code: bad_request`, and a detail
containing "is reserved".

### Job streaming (`/zzrouter/v1/jobs/:id/stream`)

| `code` | HTTP | Meaning | Recovery |
|---|---|---|---|
| `epoch_mismatch` | 409 | The job restarted since your last seq; replay window invalidated. | Resnapshot via `/zzrouter/v1/jobs/:id` and resume from current epoch. |
| `replay_unsupported` | 400 | This job kind doesn't keep history. | Drop `?from=` and consume from now. |

### Runs surface (`/zzrouter/v1/runs`)

Errors use Problem Details. Common `title`/`code` pairs an agent will see:

| Status | What it means | Recovery |
|---|---|---|
| 400 `bad_request` with provider not configured | Preview selected a provider absent from that node's configuration. | Choose one from `GET /zzrouter/v1/providers` or configure a supported provider from `/providers/catalog`. |
| 400 `bad_request` with provider disabled and not installed | The target node's inventory has no installed copy. | `POST /zzrouter/v1/providers/:name/install` on that node, follow the job, then retry. |
| 400 `bad_request` with provider disabled and installed | The target node has an installed copy that is disabled. | `PATCH /zzrouter/v1/providers/:name` with `{"enabled":true}` on that node, then retry. |
| 404 with `not in this provider's catalog` | The node does not list the requested model for that provider. | Download or register it; use `POST /zzrouter/v1/deployments` for a cloud model. Cataloged external and cloud models return ready without a run. |
| 400 with `provider %q has no search.registry configured` | auto_deploy was requested but the provider can't infer a download source. | Either deploy explicitly via `/deployments` or pick a provider with a registry. |
| 421 with `Location: <coord-url>` | Worker received a write that only the coordinator may serve (e.g. provider PATCH). | Retry against the URL in the `Location` header. |
| 404 `not_found` with `run not found: <id>` | No node lists the run. A stopped run is removed, whoever stopped it; when a live run's `evicted_runs` names it, `detail` names that run and its node. | Read the run's log at `GET /zzrouter/v1/runs/{id}/logs?node=<node>`; relaunch if you still need the model. |

### Cross-node job lookup

Agents do **not** need to pass `?node=` on `GET /zzrouter/v1/jobs/:id`,
`GET …/stream`, or `DELETE /zzrouter/v1/jobs/:id`: the coordinator checks
its local registry, then probes peers and returns the first owner's response.
Only definite 404 replies from every peer establish absence. No configured
peers means the local registry is the complete lookup.

| `code` | HTTP | Meaning | Recovery |
|---|---|---|---|
| `job_not_found` | 404 | No local job exists and every peer confirmed absence. Jobs live in memory on the node that runs them and are evicted after the completed-job retention window; `detail` gives the duration from `jobs.DefaultCompletedTTL`. | Check the job ID. An evicted job cannot be recovered by retrying the lookup. |
| `node_unreachable` | 503 | A peer that may own the job did not answer definitively, so absence is unconfirmed. | Honor `Retry-After` and retry the lookup. |

### Cluster update rollout preflight

| `code` | HTTP | Meaning | Recovery |
|---|---|---|---|
| `release_manifest_missing` | 409 | The exact target release has no signed `release.json` protocol manifest. No rollout was accepted. | Choose a release publishing the manifest, or perform an operator-managed upgrade. |
| `protocol_cutover_required` | 409 | The target protocol is outside the coordinator's accepted window, or its minimum would strand an unselected peer. No rollout was accepted. | Follow [the protocol cutover procedure](protocol_versioning.md); current protocol values live in `pkg/version/protocol.go`. |
| `node_unreachable` | 503 | A selected node's or the coordinator's protocol window could not be read. No rollout was accepted. An unreadable unselected peer does not refuse; it is listed in `unchecked_peers`. | Restore connectivity, honor `Retry-After`, and retry. |

## MCP tools

Application/tool failures carry `isError: true`, a human line in
`content[0].text`, and an RFC 9457 Problem Details object in `structuredContent`.
REST Problems keep all their fields, including `request_id`, `node` and
per-key `errors[]`. Non-problem REST error bodies use the status slug from
`utils.NewProblemDetails` (`bad_gateway` for 502), with a bounded excerpt as
`detail`. MCP-specific codes are declared in
`internal/server/mcpserver/errors.go`.

| `structuredContent.code` | Status | Meaning | Recovery |
|---|---|---|---|
| `mcp_invalid_input` | 400 | Handler validation or route-catalog refusal before dispatch. | Fix the named input; use `zz_api_search` for eligible routes. |
| `mcp_response_too_large` | 502 | REST response exceeded the tool's 1 MiB cap. | Narrow the query or reduce the result limit. |
| `mcp_streaming_refused` | 400 | Route requires streaming and has no admitted snapshot form. | Use a snapshot endpoint. |
| `mcp_response_decode_failed` | 502 | REST response could not be decoded or lacked the expected result shape. | Inspect the REST endpoint and provider logs. |
| `mcp_request_interrupted` | 499 | Tool request or job wait was cancelled or exceeded its deadline. | Check job state before retrying a mutation; reuse its idempotency key where supported. |
| `mcp_internal_error` | 500 | Request metadata, input encoding, route catalog or embedded OpenAPI search is unavailable or failed. | Retry through the public MCP endpoint; investigate persistent failures. |
| `mcp_upstream_error` | Original HTTP status | Non-problem REST error used a nonstandard status without a known status slug. | Read `status` and the bounded `detail` excerpt. |

SDK argument-schema and protocol failures keep the SDK's own error behavior;
this middleware leaves errors outside the typed tool-error contract untouched.
A FAILED job returned by `zz_job` is a successful state snapshot, with the
job's failure in `error`, rather than an MCP tool error. Polling wait limits
also return successful snapshots.

## Idempotency notes

These codes apply to keyed management and inference requests, using each surface's error envelope.

| `code` | HTTP | Meaning | Recovery |
|---|---|---|---|
| `idempotency_key_conflict` | 422 | The same key names a different request body. | Send the original body or choose a new key. |
| `idempotency_wait_timeout` | 504 | The original request is still in flight after the duplicate's wait limit. | Retry the same key and body shortly. |
| `idempotency_store_full` | 503 | All store slots are occupied by in-flight requests. | Honor `Retry-After` and retry the same key and body. |

Replay storage retains up to 4,096 entries and 32 MiB of response bodies.
Completed entries expire 24 hours after completion and are swept every five
minutes. Capacity eviction removes the oldest completed response first, never
an in-flight request. Replies larger than 1 MiB are served in full and forgotten,
so a later repeat runs again; duplicates already waiting must retry.

- `POST /zzrouter/v1/runs` with `auto_deploy:true` is singleflight-deduped
  per `(provider, model, file)` : a retry while a deploy is in progress
  attaches to the in-flight job rather than starting a second one.
- `DELETE /zzrouter/v1/jobs/:id` is non-blocking; the response is 202
  and the SSE stream is authoritative for the cancel outcome.

## What an agent should never branch on

- The `message` text : wording changes without a code change.
- HTTP status alone : both 400 and 422 may carry validation failures
  depending on the surface; the `code` is the stable axis.
- Absence of a `code` field on success responses : only error bodies
  carry codes.

### Model node selection (inference and management)

`model_node_ambiguous` is HTTP 409 when several remote nodes hold a name and
none locally. The reference is valid, but the catalog state needs a node
selection, so 409 fits better than a malformed-input 400. Choose a `name@node`
reference from the detail or set X-Node. A provider prefix filters replicas;
a coordinator-local replica always wins an unpinned request.

Failed node discovery does not establish that an unknown model exists on that
node. Unknown names keep the ordinary missing-model behavior; previously known
replicas on a failed node can return 503 `cluster_unavailable`. Discovery failures
are cached for 30 seconds between blocking retries. A provider observed stopped
still reports `provider_not_running` with its service-start route.
