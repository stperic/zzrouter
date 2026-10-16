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
pins; restart replays request overrides. Stop before uninstalling a provider.
Pinned process runs are recorded on the launching node in `<data dir>/pinned_runs.json`
(mode 0600) and relaunched once after a node restart, including a self-update. Restore
replays request overrides and resolves current tiers; failed restores are forgotten without
retry. Stop the run to forget its pin. Served engines, disposable runs and install smokes
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
