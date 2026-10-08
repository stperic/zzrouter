# Agent error code catalog

Stable `code` values an LLM agent will encounter when driving zzRouter.
Codes are wire-stable: branch on them, do not parse `message`.

Two error envelopes are in play:

- `/v1/*` (OpenAI compat) : body is `{"error":{"message","type","code","param"}}`.
  Agents read `error.code`.
- `/zzrouter/v1/*` (management) : RFC 9457 Problem Details
  (`application/problem+json`) with a `code` field at the top level.
  Agents read `code`.

Both surfaces preserve upstream status codes across cluster hops.

## OpenAI surface (`/v1/*`)

| `error.code` | HTTP | Meaning | Recovery |
|---|---|---|---|
| `model_not_found` | 404 | The named model is not in the catalog. | List `/v1/models` to discover canonical names + SourceID aliases. If you expected a deploy, POST `/zzrouter/v1/runs` with `auto_deploy:true`. |
| `context_length_exceeded` | 400 | Prompt + max_tokens exceeds the model's context window. | Read `capabilities.max_context_tokens` from `/v1/models`; trim prompt or lower `max_tokens`. |
| `request_too_large` | 413 | Multipart upload exceeds the configured ceiling. | Split the upload or reduce file size. |
| `rate_limit_exceeded` | 429 | Per-key RPM/TPM cap or budget reservation rejected the request. | Honor `Retry-After` if present; back off. Check `/zzrouter/v1/keys/:id/usage` for current budget. |
| `invalid_request_error` (type) | 400 | Generic validation failure. `param` names the offending field. | Read `param`; fix and retry. |
| `backend_error` (type) | 502/503/504 | Upstream provider failed. | Retry with exponential backoff; if persistent, the model's `runtime.status` may be wrong : call `/zzrouter/v1/runs` to redeploy. |
| `responses_not_supported_by_provider` | 400 | The model's provider declared no native or translated `/v1/responses` support (its effective endpoints omit both `responses` and `responses_compat`). | Use `/v1/chat/completions` for inference : tools, structured outputs, and streaming all work there. Providers that natively serve `/v1/responses` (vLLM, OpenAI cloud, etc.) declare it in their `config.yaml`; agents that hit those providers see passthrough. Providers declaring `responses_compat` translate supported Responses requests to Chat Completions. |
| `chat_template_rejected` | 400 | The model's chat template refused to render the request (for example a system message after the first turn). Reported for llama.cpp; vLLM and MLX return the template's own message. | Retrying will not help. An admin installs a template that accepts the request: follow `chat_templates` in `GET /zzrouter/v1`. |

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

A `PUT` or `DELETE` on an asset that ships with zzRouter is a 403: it is
restored on every start. A body over 1 MiB, or a provider over 4 MiB or
64 assets, is a 413.

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
| 400 with `provider %q is cloud-only` | Tried to launch a cloud-backed model via `/runs`. | Use `POST /zzrouter/v1/deployments` instead. |
| 400 with `provider %q has no search.registry configured` | auto_deploy was requested but the provider can't infer a download source. | Either deploy explicitly via `/deployments` or pick a provider with a registry. |
| 421 with `Location: <coord-url>` | Worker received a write that only the coordinator may serve (e.g. provider PATCH). | Retry against the URL in the `Location` header. |

### Cross-node job lookup

Agents do **not** need to pass `?node=` on `GET /zzrouter/v1/jobs/:id`
or `…/stream` : the coordinator now fans out across peers and returns
the first owner's response. A 404 means no peer claims the job.

## Idempotency notes

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
