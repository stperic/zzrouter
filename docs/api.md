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

Client keys use `X-API-Key` or `Authorization: Bearer`. Management and inference
permissions differ. A virtual key can be constrained by team, models, requests,
tokens, concurrency and spend. Workers expose inference only on their cluster
listener. The coordinator applies client policy before dispatching to workers.

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
terminal retention still apply. Cancellation aborts the active transfer, but
terminal notification may precede cleanup. Completed files stay in place;
unfinished catalog files remain pending. Repeat the download to repair only
missing files. Matching dead partials are removed by the next serialized fetch.
Provider-store destinations remain reserved until physical cleanup finishes.
Ollama owns its blobs and their cleanup.

Job GET, cancellation and pre-stream failures use Problem Details. A confirmed
absence within the selected node or complete searched scope returns
404/`job_not_found`; retention cleanup or a node restart can cause absence.
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
