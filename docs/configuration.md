# Configuration and providers

The default configuration directory is `~/.config/zzrouter` on Linux,
`~/Library/Application Support/zzrouter` on macOS, and `%APPDATA%\zzrouter`
on Windows. `ZZROUTER_CONFIG_DIR` overrides it. Service installers use a shared
machine directory: `/etc/zzrouter` on Linux and `%PROGRAMDATA%\zzrouter` on Windows.

Run `zzrouter-node config init` to create configuration. Set the listen address
before starting; the embedded node template binds all interfaces and requires
inference authentication. Installers default to loopback. Keep credentials in
protected environment configuration, rather than copied into examples.

| File | Purpose |
| --- | --- |
| `node.yaml` | Bind, authentication, cluster, model storage and update settings |
| `cli.yaml` | Client address, TLS and credential environment-variable selector |
| `providers/settings.yaml` | Global provider settings |
| `providers/<kind>/<name>/config.yaml` | Provider endpoints, runtime and parameter defaults |
| `providers/<kind>/<name>/schema.yaml` | Extended parameter schema |
| `keys.yaml`, `teams.yaml` | Virtual key hashes and team policy |
| `model_groups.yaml` | Logical model routes |

Provider kinds are `on-demand`, `external`, `cloud` and `registries`.
Shipped provider files are embedded in the node; initialization
materializes this tree. The coordinator owns provider writes. Workers receive
managed copies through peer synchronization.

Whether a node runs an on-demand or external provider is that node's choice:
`nodes.<node>.enabled` in the provider's `config.yaml`, over the top-level
`enabled`, which is the default for nodes that have not chosen. Installing,
uninstalling, or `PATCH /zzrouter/v1/providers/{name}?node=<node>` with
`enabled` records the choice for that node. Without `node`, the request goes to
every node and each records its own choice. A worker asks the coordinator to
write it, and the change is in effect once the provider sync reaches the
worker. Sync never carries cloud providers or registries, so each node keeps
its own copy of those and its top-level `enabled`.

### Install Strata through zzRouter

The shipped provider targets [Niko1221/Strata](https://github.com/Niko1221/Strata).
Like the Ollama provider, it starts disabled with a loopback endpoint,
`http://127.0.0.1:8080`, so each node reads only its own daemon. Install it on
a capable node through the ordinary provider install flow; a successful install
enables it on that node:

```text
POST /zzrouter/v1/providers/strata/install/preflight?node=localai
POST /zzrouter/v1/providers/strata/install/plan?node=localai
POST /zzrouter/v1/providers/strata/install?node=localai
GET  /zzrouter/v1/providers/strata/service/status?node=localai
POST /zzrouter/v1/models/scan?node=localai
```

Installation fetches an official release and lets upstream setup choose a model
size for that node. Setup can download more than 100 GB, so follow the returned
job ID until completion. zzRouter starts the resulting daemon through the
provider service lifecycle. Uninstall stops and detaches the managed service;
model data and release files are retained for recovery.
Setup picks its recommended model family and size, and installs the image
encoder only when the provider declares the `vision` feature. It runs with the
controlled environment managed Python installs use plus Strata's provider
`environment`, never zzRouter's own credentials.

Current upstream Linux releases publish no engine archive, so on NVIDIA nodes
setup compiles the engine with a CUDA toolkit: 13.0 for compute capability
12.x, 12.0 otherwise. Setup finds the newest toolkit itself, including
versioned `/usr/local/cuda-*` installs beside an older default; to pin one,
set `STRATA_NVCC` to its `nvcc` in Strata's provider `environment`. Install the
toolkit as the host administrator first. Setup cannot install it from
zzRouter's unprivileged service, so the job then fails at upstream's engine
step, before the model download, with upstream's instructions in its log.

Once `/health` reports ready, rescan that node through
`POST /zzrouter/v1/models/scan?node=localai`. A node without a Strata daemon
reports the provider's inventory as unavailable, as it does for Ollama. A
successful `/health` proves connectivity; a listed model may still be
unloaded. Use `zz_chat` or an inference request to verify it serves. Service
status, start, stop and restart use the same provider service routes as other
managed daemons.

### Connect an existing Strata server

Run Strata on the same node, or edit `runtime.endpoint`
on the coordinator to the root URL of a server this node can reach. Keep the
`/v1` suffix out of `runtime.endpoint`; zzRouter adds it to API requests.

If Strata requires an API key, set its provider `runtime.api` to bearer auth
with `auth_header: Authorization`, `auth_prefix: "Bearer "`, and
`token: "${STRATA_API_KEY}"`. Set that variable in zzRouter's protected
runtime environment. The Strata key replaces the caller's zzRouter key on
upstream requests. A remote endpoint is a connection to an operator-owned
server and is outside zzRouter's local managed install and service controls.

Strata's models use the same catalog identity as every provider: `name@node`
in `GET /v1/models`, or the bare name when one node holds it. Strata owns its
model files; zzRouter manages the installed service and relays requests, but
model detail and per-model deletion are unavailable through zzRouter. The
shipped Responses endpoint is native; Messages uses the Chat Completions
translation until Strata's native tool-result image behavior is verified.
Strata's Responses API is stateless: agents must send the conversation in each
request and cannot use `previous_response_id`.

## Cache observations

Provider `schema.yaml` may declare a root `cache_log` map of startup regexes.
Roles and ordered capture groups are `tokens` (KV tokens), `concurrency` (context
tokens, multiplier), `pools` (tokens/cells per pool, pool count), `slots` (sequence
slots, context tokens, unified-cache Boolean), and `sequence_slots` (sequence
limit). Each role owns its fields: a provider must not declare two roles that
report the same field from different sources. The shipped providers comply.

Captures replace their fields without adding capacity. Identical `pools` repeats
change nothing; conflicting pairs omit both KV fields for the rest of that run,
because multiple cache allocations cannot establish one capacity. Other cache
observations remain available. A bad regex, unknown role or wrong capture count
drops the entire optional `cache_log` map with one warning naming the provider
and problem per schema validation. The remaining schema loads. Launches and
provider synchronization proceed with no cache capture installed. Capture
uses the existing startup window and never delays readiness.

## Provider endpoints

`capabilities.wire_endpoints` declares native APIs such as `chat_completions`,
`completions`, `embeddings` and `rerank`; Chat also enables derived Messages and
Responses translation shims. Run endpoints are `chat` (the default), `embeddings`
and `reranking`, paired with `chat_completions`, `embeddings` and `rerank`.
Completions uses a chat run. A provider that declares capabilities refuses a run
endpoint absent from both its base declaration and the matching feature overlay.
Runs and previews return a 400 Problem; inference returns a 400 in its API's
error envelope. Undeclared capabilities retain unknown, permissive behavior.

Tier `endpoints.<endpoint>` overlays supply parameters and environment for that
run endpoint. A provider with endpoint overlays or non-chat runtime features
keeps separate runs for each endpoint. Served Ollama models share one run across
supported endpoints; reranking is refused. Node-scoped providers accept no model
or endpoint.

`features` supports `vision`, `embeddings` and `reranking`. Runtime features can
replace `execution`, `wire_endpoints` and `health_check`, and remove launch flags
with `exclude_parameters`. Each feature applies only to its endpoint. Missing
required runtime support returns an actionable 400. See
[model features](model_features.md) for MLX installation, deployment and catalog
state details. Protocol 11 requires upgrading every node together.

## Chat usage accounting

The coordinator meters raw upstream usage before normalizing chat responses.
Reported spend is `usage.cost` plus `usage.cost_details.upstream_inference_cost`
when present. OpenRouter's account charge is zero for BYOK requests, while the
upstream inference cost carries the provider's charge. An explicit nonnegative
numeric `usage.cost`, including a zero total, is authoritative. Missing, null,
malformed or negative costs fall back to positive legacy `x_*.cost` evidence in
sorted key order, then the pricing store. A zero extension cost is not provider
evidence. Response and inference-log costs retain provider precision;
the spend ledger uses the existing whole-microUSD conversion.

Client-facing chat responses omit `usage.cost`, `usage.cost_details`,
`usage.is_byok` and `usage.prompt_tokens_details.cache_write_tokens`. Standard
token counts, cached/reasoning details and unrelated extensions remain. Buffered
costs appear in `zzrouter.cost_usd` / `cost_source`; streamed costs appear in
`usage.zz_cost_usd` / `zz_cost_source` when metadata is enabled. Cleanup also
applies with `?verbose=0`, streaming metadata disabled, or usage frames suppressed.
Workers preserve upstream accounting until the coordinator meters it.
Stream cost metadata retains its existing gate on positive `completion_tokens`.

OpenRouter returns usage automatically. Its deprecated `usage.include` option
is not injected, and no provider capability or cluster protocol change is needed.

## Node GPU policy

`node.yaml` controls memory reclamation locally; it is not part of provider sync:

```yaml
gpu:
  evict_idle_on_memory_pressure: false
```

The default is off. Enable it on each node that may stop idle zzRouter runs to
admit a launch whose GPU budget cannot fit. Restart the node after changing it.
Only unpinned running or unhealthy instances with no activity hold, active request
lease, claimed stop or queued launch waiting on them qualify. Oldest activity goes
first, and nothing is stopped unless the eligible holders cover the memory shortfall. Unknown holder
sizes count as zero. Other processes are never reclaimed.

After stopping, admission waits up to 5 seconds for asynchronous driver memory
release, checking every 100 ms with a 64 MiB tolerance, then retries once.
The existing fit error remains the refusal when reclamation is disabled or
insufficient. Keep-alive expiry still uses the same eligibility rule.

The launch reports what it stopped: its run's `evicted_runs` and its run job's
`meta.evicted_runs` list each run id, model and the MiB it held. A lookup of an
evicted run answers 404 naming the live run that evicted it.

## Parameter tiers

Effective launch settings resolve from provider defaults, model-family defaults,
model settings, node settings, node/model settings and request overrides, in that
order. Later tiers win. Inspect the result with `zzrouter providers resolved`
or `GET /zzrouter/v1/providers/:name/resolved?model=...&node=...`.

Change settings through `PATCH /zzrouter/v1/providers/:name/parameters` with
`Content-Type: application/merge-patch+json`. A null removes an override;
arrays replace. Workers redirect coordinator-owned writes with HTTP 421 and
`Location`.

PATCH and every launch take only the parameters
`GET /zzrouter/v1/providers/:name/schema` lists. A bool the engine also spells
`--no-<flag>` is listed once, with `negatable: true`: `true` passes `--<flag>`,
`false` passes `--no-<flag>`, and a `no-<flag>` key is refused with
`unknown_flag`. Another bool's `false` leaves its flag unset.

Set `keep-alive: "-1"` (or another negative duration) in the provider tier tree
or run-start `parameters` to pin a run against zzRouter idle expiry and pressure
reclaim. For example, PATCH `/zzrouter/v1/providers/vllm/parameters` with
`{"models":{"embedding-model":{"parameters":{"keep-alive":"-1"}}}}`, or
POST `/zzrouter/v1/runs` with
`{"provider":"vllm","model":"embedding-model","parameters":{"keep-alive":"-1"}}`.
A request `-1` overrides a positive tier value and pins; a positive request value
overrides a negative tier value and does not pin. The final admitted launch
settings fix the pin for the run's lifetime. A tier edit affects a live run only
after restart; inspect `parameters_status` or use `restart=affected` on the PATCH.
Inference `keep_alive` hints keep their last-hint-wins idle-timer behavior and
never pin or unpin, including negative, positive and zero hints.

Run reports expose `pinned: true` when pinned. A memory refusal remains 409
`out_of_range`, with `errors[].got.holders[].reclaim_skipped_reason: "pinned"`
and the holder's run, model and provider. If unpinned holders cannot cover the
shortfall, nothing stops. Explicit DELETE, Force replacement and restart may stop
pinned runs; restart replays request overrides. Stop before uninstalling a provider.
Pinned process runs are recorded on the launching node in `<data dir>/pinned_runs.json`
(mode 0600) and relaunched once after a node restart, including a self-update. Restore
replays request overrides and resolves current tiers. A restore or restart refused for its
parameters stays listed as failed and pinned; other failed restores are forgotten without
retry. Only an explicit stop forgets a pin; a run for its key reaching running rewrites it. Served engines, disposable runs and install smokes
are excluded. Surviving daemon models can be re-adopted; daemon unloads are not prevented.
Upgrade every worker before relying on pins: an older worker keeps `-1` as
idle-only and may reclaim it under pressure. No cluster protocol bump enforces this.

A model variant uses `models.<variant>.from: <base>` with optional parameters
and request defaults. Equal launch settings share the base process; distinct
settings get a separate process over the base weights. Variant names cannot
conflict with real weights or catalog aliases. One inheritance level is supported.

See [model features](model_features.md), [managed recipes](typed_provider_recipes.md)
and the [API reference](api.md) for deployment and installation contracts.

## OpenTelemetry

Tracing is opt-in through `observability.tracing.enabled` in `node.yaml`.
See [OpenTelemetry tracing](observability.md) for collector protocols, ingestion
headers, sampling and restart-required activation.

## Budget pricing

Set `models.pricing.enabled: true` to use the pricing table. Administrators can
POST rates to `/zzrouter/v1/pricing/overrides`; an explicit zero rate declares
free inference. Enable pricing before posting overrides.
Recognized launched engines and served Ollama default to zero, with operator
overrides taking precedence. Ollama Cloud and arbitrary external endpoints do
not inherit local-free pricing.

With a key or team `spend_limit > 0`, admission removes targets without known
rates after wire endpoint selection and before fallback. If none remain, 403
`model_unpriced` names the provider/model and administrator action. Callers with
no spend limit keep every candidate. Free traffic still counts tokens and
requests and works after monetary exhaustion; paid exhaustion remains 402.
Authorization runs before preparation, and resolution errors precede quota
checks. `/zzrouter/v1/pricing/status` counts missing-price observations both
from admission (removed targets or refused requests) and from completed unlimited
traffic. Explicit free inference creates no pricing miss. Misses are process-local;
adding an answering override clears its counter.

Upgrade: limited keys or teams routing to an external self-hosted endpoint
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
