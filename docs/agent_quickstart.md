# Agent quickstart

For Claude Code or Codex tool access, see [MCP setup and tools](mcp.md).

An operator initializes the node, approves provider installation authority and
gives the management agent an admin key through a protected credential channel.
Inference clients receive separate virtual keys. Host changes remain operator work.

```sh
Z=http://127.0.0.1:9090
curl -fsS -H "X-API-Key: $ZZROUTER_ADMIN_API_KEY" "$Z/zzrouter/v1"
curl -fsS -H "X-API-Key: $ZZROUTER_ADMIN_API_KEY" "$Z/v1/models"
```

Choose a provider compatible with the node's hardware. Inspect its environment,
install prerequisites and install plan through the advertised provider routes.
Use the managed install API; follow the returned job and verify the runtime.
See [managed recipes](typed_provider_recipes.md) for package authority.

Before choosing a node, query `GET /zzrouter/v1/nodes/compatible?model=<exact-name>`
with optional `provider` and `registry`. `data` contains confirmed capacity fits;
`metadata.assessments` includes `fit`, `no_fit` and `unknown` reasons per node/provider.
Each public row's `eligible_providers` lists only providers assessed as `fit`;
metadata retains all provider assessments. If the node inventory cannot be read,
an assessment with an empty `node` reports `unknown` coverage alongside collected results.
Unknown stays HTTP 200 and is not confirmed fit. Missing worker evidence is unknown.
Capacity uses selected device totals; a busy node may still refuse launch.
`POST /zzrouter/v1/runs/preview` reports the same policy in `data.assessment`
without starting, evicting or reserving memory. Cached or published weight size
alone cannot prove runtime fit. Downloads remain possible on an explicitly
selected eligible node when capacity is unknown.

Supported dense, MoE and hybrid-attention models assess auto vLLM and MLX at the
declared minimum of 4096 context tokens. MLX has no context parameter: `max-tokens` limits generation,
and its usable unified working set uses `memory_usable_fraction`, shipped at
`0.75`. This fraction and `auto_min_context_tokens` are release-owned schema
metadata. Auto llama.cpp assesses the GGUF native context and reports
`placement: gpu` or `offload` (GPU plus system RAM).
If that native-context estimate exceeds GPU plus RAM, the result is unknown
and launch proceeds because the engine may adapt. A numeric context shortfall
still refuses launch, as does the weight-only lower bound in either mode.
GGUF sizing recognizes llama, qwen2, qwen3, qwen3moe and qwen35 architectures.
KV sizing honors schema-declared cache roles, including vLLM fp8 and llama.cpp
GGML types.
Hybrid sizing counts KV only on full-attention layers and adds fixed recurrent
states with float32 convolution and SSM elements, conservatively covering the
supported engines. Schema metadata declares `recurrent_state_rows`: vLLM uses
two rows for one assessed request, independent of `max-num-seqs`; MLX uses one.
llama.cpp's `parallel` parameter has the `sequence_slots` role and a four-row
engine default; an explicit value selects that many state rows without
multiplying attention KV. MTP layers are excluded from normal decoder cache,
and every expert's weight bytes count for MoE models. These estimates include
the existing runtime overhead and headroom, without modelling pages or pools.
Missing row metadata, unsupported geometry/dtypes, MLA, ambiguous sliding
geometry, and hybrid placement across multiple selected devices remain unknown.
Assessments report `context_mode`, `context_tokens`, and, for supported auto
cases, `affordable_context_tokens`. Affordability is an estimate within the
recognized native range, never served context or a performance guarantee.
Missing context range, geometry or device evidence remains unknown. Numeric
vLLM/llama.cpp context overrides assess larger workload requirements. Fully
evidenced failures also refuse launch; a fixed-budget shortfall never evicts
holders. Auto context keeps greedy memory allocation and the recorded `auto`
intent; actual served context remains the engine's readiness evidence.

Hosts with no GPU observation, including CPU-only llama.cpp, skip the weight
lower bound and report `unknown`. Their launches keep the existing CPU path.

For an installed llama.cpp provider, launch a model and request automatic
weight deployment when needed:

```sh
curl -fsS -H "X-API-Key: $ZZROUTER_ADMIN_API_KEY" \
  -H 'Content-Type: application/json' -X POST "$Z/zzrouter/v1/runs" \
  -d '{"provider":"llamacpp","launch_mode":"native","model":"Qwen/Qwen2.5-0.5B-Instruct-GGUF","auto_deploy":true}'
```

Follow `deploy_job_id` when present and the launch `job_id`. Poll the jobs API
or its SSE stream until successful, then confirm the run is ready. Use a node
name such as `worker-1` when targeting a paired worker; an address is not a node
selector. Select a model endpoint advertised by `/v1/models`.

Create a client key with `zzrouter keys create my-agent --rpm 60` and place the
returned credential in `AGENT_API_KEY` through your protected credential channel:

```sh
curl -fsS -H "Authorization: Bearer $AGENT_API_KEY" \
  -H 'Content-Type: application/json' "$Z/v1/chat/completions" \
  -d '{"model":"Qwen/Qwen2.5-0.5B-Instruct-GGUF","messages":[{"role":"user","content":"Hello"}]}'
```

If a model's own chat template refuses a request (`chat_template_rejected`),
`chat_templates` in `GET /zzrouter/v1` gives the API steps to install and
select a template that accepts it. zzRouter ships no chat templates.

Observe runs at `/zzrouter/v1/runs`, stop one with `DELETE /runs/:id` under that
management prefix, and branch on error codes rather than message text.
The [OpenAPI specification](../internal/server/openapi.yaml) is the field reference.

Node inventory (`/zzrouter/v1/nodes` and `/nodes/:name`) and MCP use the same
health view. Three consecutive node transport failures report `down` and refuse
ordinary traffic, including pinned inference, jobs and provider sync. HTTP provider
errors, caller deadlines and errors after response headers do not trip the node
breaker. Recovery needs no inference: after a
30-second cooldown, two successful full cluster probes restore dispatch. During
half-open recovery the node reports `degraded` and admits probes only.

One missed full probe also reports `degraded` but keeps dispatch grace; a second
miss reports `down`. Public-only `degraded` and `unknown` nodes cannot receive
ordinary traffic. A graceful goodbye marks the node down immediately.
`last_seen_at` remains the latest successful probe; `last_error` contains sanitized
probe or dispatch evidence. Restore worker connectivity or pairing, then observe
probe-driven recovery. Repeated inference requests do not recover a node.
