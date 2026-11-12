# API reference

The embedded [OpenAPI specification](../internal/server/openapi.yaml) describes
request fields, schemas and responses. The running node serves its API catalog
at `GET /zzrouter/v1`; use discovery to inspect capabilities before an operation.

| Surface | Authentication and purpose |
| --- | --- |
| `/health/*` | Health checks without authentication |
| `/zzrouter/v1/*` | Management APIs; admin key, with explicitly supported owner permissions |
| `/v1/*` | OpenAI compatible APIs and Anthropic Messages |
| `/api/*` | Ollama compatible APIs |
| `/zzrouter/v1/internal/*` | Cluster transport, authenticated through mTLS |

MCP supports default-enabled OAuth browser authorization on coordinators. See [MCP login](mcp.md#connect-with-browser-login).
OAuth discovery and protocol endpoints live under `/.well-known/*` and `/oauth/*`
unless `auth.oauth.enabled` is false. The default issuer uses localhost at the
configured node port; use `auth.oauth.issuer_url` for a remote HTTPS origin. OAuth tokens are limited to their
MCP resource and cannot authenticate direct REST calls.

Client keys use `X-API-Key` or `Authorization: Bearer`. Management and inference
permissions differ. A virtual key can be constrained by team, models, requests,
tokens, concurrency and spend. Workers expose inference only on their cluster
listener. The coordinator applies client policy before dispatching to workers.

## Browser sessions and provider connections

When coordinator OAuth is enabled, `GET /zzrouter/v1/session` bootstraps a CSRF
proof and browser cookie. `POST` on that path accepts `{ "key": "..." }` with
the exact issuer `Origin`, the cookie and `X-CSRF-Token`; `DELETE` logs out using
the same origin/proof. The opaque session remains HttpOnly. `GET /me` returns the
current identity, and `GET /me/connections` or `DELETE /me/connections/{id}`
operates only on its MCP grants. These paths use the `/zzrouter/v1` prefix.
Bootstrap/login also return the fixed `issuer` and coordinator `node_name`.
`/me` explicitly projects key/team limits, team model allowances and own key
usage; unavailable usage is null. It exposes no credential fingerprint or key
metadata. The [console](web-console.md) is served at `/console/` when coordinator
browser authentication is enabled.

Protected admin browser sessions can call management and access-control APIs.
Mutations require Origin/CSRF; API-key clients retain their existing contract.
MCP tokens, browser cookies and general API keys remain distinct authorities.
Cluster pairing and worker/internal paths do not accept browser authority.

`GET /zzrouter/v1/providers/{name}/connection` returns the declared field schema,
nonsecret values, secret presence, ownership mode and revision. `POST` to its
`/test` subpath accepts candidate `values` without saving. `PUT` replaces the
complete connection with `expected_revision`, `values` and optional `activate`.
`DELETE` with `expected_revision` removes managed values and suppresses external
fallback. `POST` to `/use-external` explicitly restores external resolution.
All require a coordinator administrator, reject node selectors and use no-store
responses. Inspect `saved`, `durable`, `active`, `published` and `outcome` after a
mutation; saving and activation are separate effects. See OpenAPI for schemas.
`GET /providers/catalog?kind=all` includes registries and disabled providers.
Each row's `connection_supported` indicates a declared editable connection.
Read list entries from `data`; list responses need not contain `success`.
Unavailable configuration returns 503, while a validated empty catalog returns
`data: []`. Connection refusals have distinct codes for unsupported connections,
unavailable state, stale revisions, absent startup protection and failed writes.

Model search and cards retain native provider fields and add typed `discovery`
metadata. A missing storage size stays unknown, separate from parameter count.
Hugging Face variants retain `shards` and `features` inside the `data` envelope.
`GET /zzrouter/v1/registries/ollama/tags?id=llama3.2` supplies catalog tags through
the authenticated coordinator, without client-side upstream scraping.

## Discover, execute, observe

List providers with `GET /zzrouter/v1/providers` and models with
`GET /v1/models` or the detailed management catalog `GET /zzrouter/v1/models`.
Use only inference endpoints advertised for the selected model.

Provider-owned files use `/zzrouter/v1/providers/{name}/models` with a required
node, plus `/models/resolve` for workflow metadata. The discovery document's
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
