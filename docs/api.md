# API reference

The embedded [OpenAPI specification](../internal/server/openapi.yaml) describes
request fields, schemas and responses. The running node serves its API catalog
at `GET /zzrouter/v1`; use discovery to inspect capabilities before an operation.

| Surface | Authentication and purpose |
| --- | --- |
| `/health/*` | Health checks without authentication |
| `/zzrouter/v1/*` | Management APIs; admin API key or supported browser session, with explicit owner permissions |
| `/inference/v1/*` | User-scoped operation and native connection discovery |
| `/v1/*` | OpenAI compatible APIs and Anthropic Messages |
| `/api/*` | Ollama compatible APIs |
| `/zzrouter/v1/internal/*` | Cluster transport, authenticated through mTLS |

MCP supports default-enabled OAuth browser authorization on coordinators. See [MCP login](mcp.md#connect-with-browser-login).
OAuth discovery and protocol endpoints live under `/.well-known/*` and `/oauth/*`
unless `auth.oauth.enabled` is false. The default issuer uses localhost at the
configured node port; use `auth.oauth.issuer_url` for a remote HTTPS origin. OAuth tokens are limited to their
MCP resource and cannot authenticate direct REST calls.

Client keys use `X-API-Key` or `Authorization: Bearer`. Management and inference
permissions differ. An app key inherits its team’s provider/model access, routes, requests, tokens,
concurrency and spend rules. Workers expose inference only on their cluster
listener. The coordinator applies client policy before dispatching to workers.

Cluster connection direction, enrollment and replication are described in
[cluster networking](cluster_networking.md). Only the coordinator initiates cluster connections.

## Provider operations and endpoint discovery

The fixed operation interfaces expose provider-specific behavior through trusted
definitions. Both `/zzrouter/v1` and `/inference/v1` provide operation list,
description and execution routes plus ordinary connection discovery. Existing
administrator and user audience rules remain separate. Discovery lists only
operations the caller can execute; the first exact-run JSON diagnostic adapter
requires an administrator.

`GET {prefix}/connections` publishes native and routed addresses for visible
providers and runs. It is a read-only facility for direct use, diagnostics or
comparing provider requests with routed requests. The reply distinguishes actual
listener scope from advertised node addresses, and does not claim that a remote
client can reach a loopback listener. It neither starts a run nor changes network
configuration, and excludes credentials.

See the [provider extensibility specification](provider-extensibility.md) for
definition and target revisions, schemas, execution limits and compatibility.

## Native mutations and stored responses

Ollama pull, create, copy, push, delete and blob upload require an administrator
key, including when inference permits anonymous callers. Existing provider and
route policy still applies. Version and account handshakes remain usable by
route-restricted callers whose provider policy permits Ollama; the account
handshake then returns a synthetic unauthorized response without querying the
upstream account.

For a worker import, send the same `X-Node: worker-name` header on every
`HEAD /api/blobs/{digest}`, `POST /api/blobs/{digest}` and `POST /api/create`.
Pull and create also accept `model@node`; if both forms are supplied, they must
select the same node. Selecting a worker only on the final create request does
not move earlier uploaded blobs. Without a selector these operations stay local.
The Ollama compatibility API rejects `?node`, file selectors and qualifiers for
another provider. Unknown or unreachable nodes fail without falling back to the
coordinator. Blob bytes and native upstream statuses retain streaming behavior.
The coordinator's `node.max_request_size` also bounds streamed uploads and
defaults to 10 MiB. Set it explicitly above the intended GGUF size and restart
the coordinator before importing a larger file. Exceeding the limit returns
413; a declared oversized body is refused before upstream contact, while an
unknown-length body may already have been partially forwarded. See
[request and upload limits](configuration.md#request-and-upload-limits).

Native stored Responses require an authenticated caller. `store:false` remains
available under existing anonymous inference policy. Retrieval, deletion,
cancellation, input-item reads and continuations use the original caller/tenant
and the connection or exact run captured before dispatch. Unknown or cross-owner
IDs return `response_not_found`; changed or unavailable origins return
`response_origin_unavailable`. Continuations cannot change the original caller
model. No missing binding is resolved through a default backend or replacement
run. Translated Responses remain stateless. Bindings are in memory and expire;
they are not a durable cross-restart response store.

## Compatibility and native options

Native inference preserves provider-specific JSON options. A Messages or
Responses translation shim reconstructs the equivalent Chat Completions request
and preserves additional native options without converting JSON numbers through
floating-point values. `extra_params` supplies native Chat options during
translation, but cannot override translated fields such as model, messages,
tools, stream, token limits or reasoning effort. Conflicting controls are refused.

Responses translation maps `reasoning.effort`, strict function tools,
`text.format` JSON schemas and `text.verbosity`. Requested behavior that the shim
cannot represent returns a parameter error before inference. Examples include
stateful Responses controls, reasoning summaries, Messages thinking/cache/service
controls, multiple output choices, log probabilities and audio output. Use an
advertised native endpoint for those capabilities. Native forwarding does not
apply translation restrictions. The Messages token-count fallback reserves its
own one-token output limit regardless of supplied token-limit aliases.

`POST /v1/completions` accepts a string, a nonempty string array, a nonempty
array of nonnegative integer token IDs, or a nonempty array of nonempty token-ID
arrays as `prompt`. Omitted, null and empty-string prompts retain native default
behavior. Mixed arrays, fractional IDs, negative IDs and empty arrays are invalid.
The router forwards valid prompt data unchanged; the selected provider still
determines which forms it supports.

Realtime WebSockets and the `/v1/realtime/sessions`,
`/v1/realtime/transcription_sessions` and `/v1/realtime/client_secrets` HTTP helpers
all use `openai_compat.realtime_backend`. They share opaque forwarding admission
and provider credentials. Session request bodies are forwarded unchanged,
including GA `session.model` and transcription requests without a root `model`.
These helpers do not use model aliases or route groups. Policies requiring model
inspection or usage accounting cannot be enforced by this opaque surface and
are refused. A client secret permits a later direct upstream session; zzRouter
does not meter that later session.

## Browser sessions and provider connections

When coordinator OAuth is enabled, `GET /zzrouter/v1/session` bootstraps a CSRF
proof and browser cookie. Account sign-in uses `POST /zzrouter/v1/session/password`
with `username` and `password`. The advanced `POST /zzrouter/v1/session` accepts
`{ "key": "..." }`. Both require the exact issuer `Origin`, cookie and
`X-CSRF-Token`; `DELETE /zzrouter/v1/session` logs out with the same origin/proof. The opaque session remains HttpOnly. `GET /me` returns the
current identity, and `GET /me/connections` or `DELETE /me/connections/{id}`
operates only on its MCP grants. These paths use the `/zzrouter/v1` prefix.
Bootstrap/login also return the fixed `issuer`, coordinator `node_name` and
`sharing` assessment. The `/access/*` APIs cover one-time setup, invitations,
account recovery, People administration and onboarding; see the
[onboarding/access specification](onboarding-access-spec.md) and OpenAPI for
payloads, proof requirements and role gates.
`/me` explicitly projects account or key identity, effective team limits, model
allowances and own usage; unavailable usage is null. It exposes no credential fingerprint or key
metadata. The [console](web-console.md) is served at `/console/` when coordinator
browser authentication is enabled.

Protected admin browser sessions can call management and access-control APIs.
Mutations require Origin/CSRF; API-key clients retain their existing contract.
MCP tokens, browser cookies and general API keys remain distinct authorities.
Cluster pairing and worker/internal paths do not accept browser authority.

`GET /zzrouter/v1/providers/{name}/connection` returns the declared field schema,
nonsecret values, secret presence, ownership mode and revision. `POST` to its
`/test` subpath accepts either candidate `values` or `{ "stored": true }` to test
the saved coordinator connection without resubmitting secrets. The modes are
mutually exclusive; neither saves or enables the provider. Saved testing resolves
credentials and destination together, including configured disabled providers.
An absent connection returns `provider_connection_not_configured`; an unavailable
probe returns `unknown` / `probe_unavailable`, not a successful verification.
`PUT` replaces the
complete connection with `expected_revision`, `values` and optional `activate`.
`DELETE` with `expected_revision` removes managed values and suppresses external
fallback. `POST` to `/use-external` explicitly restores external resolution.
All require a coordinator administrator, reject node selectors and use no-store
responses. Inspect `saved`, `durable`, `active`, `published` and `outcome` after a
mutation; saving and activation are separate effects. See OpenAPI for schemas.
`saved` and `durable` describe the encrypted credential change. Provider setup
can partially apply before reporting an error, so `saved_activation_failed` may
coexist with `active: true`. Refresh current state before retrying; provider
configuration spans multiple files and does not promise an atomic transaction.
`GET /providers/catalog?kind=all` includes registries and disabled providers.
Each row's `connection_supported` indicates a declared editable connection.
Read list entries from `data`; list responses need not contain `success`.
Unavailable configuration returns 503, while a validated empty catalog returns
`data: []`. Connection refusals have distinct codes for unsupported connections,
unavailable state, stale revisions, absent startup protection and failed writes.

When a provider declares no connection-test endpoint, `/test` returns
`status: unknown` and `code: probe_unavailable` without making a provider request.
Credentials can still be saved, but only a real model request can verify them.

Model search and cards retain native provider fields and add typed `discovery`
metadata. A missing storage size stays unknown, separate from parameter count.
Hugging Face variants retain `shards` and `features` inside the `data` envelope.
`GET /zzrouter/v1/registries/ollama/tags?id=llama3.2` supplies catalog tags through
the authenticated coordinator, without client-side upstream scraping.

A provider with an explicitly unavailable model catalog returns HTTP 501 with
`code: model_discovery_unavailable` for search, popular-model and card requests.
Register a known provider model ID instead. This does not mean inference is
unavailable and does not substitute a successful catalog result.

## Discover, execute, observe

List providers with `GET /zzrouter/v1/providers` and models with
`GET /v1/models` or the detailed management catalog `GET /zzrouter/v1/models`.
Use only inference endpoints advertised for the selected model.

Provider-owned files use `/zzrouter/v1/providers/{name}/models` with a required
node, plus `/models/resolve` for saved workflow metadata or an official template ID/title. `/providers/{name}/templates` lists the searchable official ComfyUI catalog; `/providers/{name}/templates/{template}` reads one UI graph without provisioning. Submit that received graph as `workflow` to resolve its exact dependencies without refetching upstream. `download:true` on resolve starts existing per-file jobs for missing dependencies, including those in nested subgraphs. Catalog model provisioning remains unverified until inspection; it does not certify execution compatibility. The discovery document's
`provider_models` field points to the [ComfyUI operator and agent recipe](comfyui.md#model-files-through-the-coordinator).

Installation, downloads and launches may return HTTP 202 with a job ID.
Poll `GET /zzrouter/v1/jobs/:id` or stream `GET /zzrouter/v1/jobs/:id/stream`.
A terminal success and the resulting resource state establish completion;
acceptance alone does not. Cancel individual downloads and other work with
`DELETE /zzrouter/v1/jobs/:id`; omit `node` to discover the owner, or supply a
node name. The nonblocking 202 reply carries `accepted` and `job_id`. Observe
the job afterward: a cancelled download is `failed` with
`meta.transfer_status: cancelled`. Already-terminal success remains success.
Use `DELETE /zzrouter/v1/deployments/:id` to cancel all deployment nodes, or
`DELETE /zzrouter/v1/deployments/:id/nodes/:node` for one node.

Provider installs have no default job silence limit. Their owner controls operation
timeouts; steps without a declared timeout keep their parent-context behavior.
Explicit inactivity overrides, cancellation, shutdown and terminal retention still apply.

Downloads outlive the accepting request, with a 10-minute body-read stall
deadline and a 48-hour overall cap. Every successful byte read resets the stall
deadline; ten minutes without a byte fails the transfer with
`no data received for 10m0s`. Downloads are exempt from default job inactivity
reaping; explicit inactivity overrides and
terminal retention still apply. For Hugging Face catalog downloads and peer sync,
terminal cancellation follows physical unwind and catalog publication. Completed
files stay in place; unfinished catalog files remain pending. A retained repository
with no completed weights is listed as `incomplete: true`, even with only its
manifest. Repeat the download to repair missing files, or delete its exact catalog
identity through `DELETE /zzrouter/v1/models`. Deletion refuses an active writer
immediately with 409 `download_in_progress`: cancel its job and await the terminal
outcome first. Matching dead partials are removed by the next serialized fetch.
Provider-store destinations remain reserved until physical cleanup finishes.
Ollama owns its blobs and their cleanup.

Finished jobs stay readable for 24 hours after the later of completion and the
latest hand-out of their ID for an operation, including warm run reuse. Creation
counts as a hand-out; reading, listing and replaying an acceptance do not extend
retention. Pending and running jobs have no expiry. Terminal snapshot, list and
SSE events carry `expires_at`, and reads at or after it return not-found even
before cleanup. Sparse terminal event rings release unused slots without dropping
events or imposing a job count cap. The duration is the idempotency replay
window; replay is anchored at request arrival, so every replayed acceptance
names a still-readable job.
Accepted run and download replies do not predict a completion timestamp. A
reused run whose job has expired or is absent returns no job ID; inspect `zz_runs`.
Job IDs are omitted when unavailable, including from warmup refusals. An
auto-deploy with no observable download job returns 503 and points to `zz_models`;
it starts no run chain. Existing download and sync work with no observable job
also returns 503. Install reuse with a missing job takes the normal new-install
path. Sync acceptance omits an absent parent job ID and keeps its local job ID.

Jobs live only in the serving node's memory and disappear on restart. Update
rollout history is separate, persisted and count-bounded: its outcomes survive
job expiry and restart without an expiry or renewal. Update status includes
`rollouts` both locally and when querying selected nodes.

Job GET, cancellation and pre-stream failures use Problem Details. A confirmed
absence within the selected node or complete searched scope returns
404/`job_not_found`; retention cleanup or a node restart can cause absence.
Its `errors[].hint` names `zz_providers` for install/service IDs, `zz_models` for
downloads, `zz_runs` for runs, and `zz_update_status` for updates. Sync, firehose
and unknown IDs have a generic outcome-unavailable hint. Absence does not tell
whether the operation succeeded.
If a possible owner did not answer, lookup returns 503/`node_unreachable` with
`Retry-After: 5`. Proxied refusals preserve status, code, node and retry metadata,
with the public request ID and path. Malformed successful replies return
502/`upstream_shape`; transport failures name `node_unreachable` or
`upstream_timeout`. Client cancellation closes the request without another reply.

Management failures use RFC 9457 Problem Details and a machine-readable `code`.
Inference envelopes follow the selected protocol. See the
[error catalog](agent_error_codes.md) and [agent quickstart](agent_quickstart.md).

### Retained deployment outcomes

Deployment GET/list return canonical per-node snapshots, including fractional
progress, speed, bytes, source and the worker's immutable `job_id`. Completion
is captured without polling a download key, including peer sync. Active-only
lists exclude retained terminal deployments. Per-node cancellation reports
`cancelled`; unknown/lost job observation reports `failed` with an explicit
unconfirmed-worker diagnostic, which does not claim work was rolled back.

Cancellation sends only the recorded immutable job ID, so an old deployment
cannot stop a newer operation using its `download_id`. Restart readiness waits
on retained node outcomes after the worker job expires. Terminal deployments
are retained for 30 minutes and swept on the next deployment creation. These
records and worker jobs are in memory and do not survive their owning process
restarting.

## Team impact, access audit and recovery scope

`Team.membership` projects app-key and person counts, lifecycle-pending invitations,
other pending enrollment grants, owner protection, `can_delete` and `can_suspend`.
These are backend-owned observations used by deletion, not permission rules for a
client to recalculate. Counts include suspended people and suspended/expired keys.

`BrowserSessionState.sharing` describes the canonical issuer: `local_only`,
`configured` HTTPS, or `unavailable`. It never asserts tested remote connectivity.

`GET /zzrouter/v1/access/audit?limit=50&cursor=...` is coordinator administrator-only
and returns `data.events`, never null, and `data.next`, absent on the last page.
Pages are newest first, at most 100 events (default 50). Optional filters combine:
`actor`, `subject` and `action` match the stored value exactly (`action` is one of
the recorded action names), `since` is an inclusive and `until` an exclusive RFC 3339
bound on the event time. Pages run newest first by event time, events sharing a
time in recorded order; an event's time is the coordinator's clock when it was
recorded. A filter that matches nothing returns an empty page, so a
mistake is refused rather than read as no events: an unknown parameter, a repeated
or empty one, a value over 256 bytes, an unknown action, a malformed time or a
`since` after `until` is refused with 400 and one `errors` entry per key
(`unknown_field`, `invalid_value`, `out_of_range`, `wrong_type`) whose `want`
names what is accepted. A cursor reads on only with the
filters it was issued under; a malformed cursor, one another store issued, or one
sent with other filters returns 409; read again without it. A history that cannot
be read returns 503. People mutation events describe committed authority changes,
not every OAuth login or logout.

`GET /zzrouter/v1/system/recovery` returns safe recovery inventory and separate
requirements to administrators. It exports no file content or credential values.
Backup/check/staged restore are local commands; see [recovery](recovery.md).

`GET /zzrouter/v1/system/state` is coordinator administrator-only and reports the
access store: `store_id`, `generation`, `schema_version`, `revision` (advanced by
every change to keys, teams, people or invitations), `sequence` (advanced by every
commit, audit-only ones included), `status` (`setup_required` until the owner
claims the installation, then `ready`), `observed_at`, `size` in integer bytes
(`database_bytes`, `journal_bytes`, 0 outside a write, `free_page_bytes`, as
the latest commit left it, and `backup_bytes`; `null` marks a measurement the node
could not take), `integrity` (the `quick_check` run when the store opened, with
`checked_at` and `outcome`) and `retained_copies`, each with `kind`
(`online_backup`), `id`, `created_at` and `size_bytes`; `backup_bytes` totals the
retained online backups. The read runs no database
query and never waits for an access write; it names no path and carries no secret.
A worker answers 421, and a store that can no longer vouch for its records answers
503 `access_store_failed` while the coordinator restarts.

`POST /zzrouter/v1/system/state/backups` backs up the access database online and
answers 201 with the backup; it takes an empty JSON object and an optional
`Idempotency-Key`, and accepts an administrator or, from the installed host, the
local proof in `X-Zzrouter-Local-Proof` (a request carrying that header, even
empty, is judged by the proof alone). `GET /zzrouter/v1/system/state/backups`
lists the retained backups and `GET /zzrouter/v1/system/state/backups/{id}` reads one, for
administrators: `id`, `created_at`, `size_bytes`, the store identity it holds,
`readable`, the archive's `sha256`, `protected_until` and `expires_at`. An unknown
or expired ID is 404 `state_backup_not_found`; a full cap or disk is 507
`state_backup_capacity_exceeded`; a copy that fails its check is 500
`state_image_check_failed`. See [recovery](recovery.md#online-backups-of-the-access-database).
