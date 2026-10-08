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

Installation, downloads and launches may return HTTP 202 with a job ID.
Poll `GET /zzrouter/v1/jobs/:id` or stream `GET /zzrouter/v1/jobs/:id/stream`.
A terminal success and the resulting resource state establish completion;
acceptance alone does not. Cancellation uses `DELETE /zzrouter/v1/jobs/:id`.

Management failures use RFC 9457 Problem Details and a machine-readable `code`.
Inference envelopes follow the selected protocol. See the
[error catalog](agent_error_codes.md) and [agent quickstart](agent_quickstart.md).
