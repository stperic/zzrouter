# Model features

A provider declares model abilities in its `config.yaml`. The supported feature
names are `vision`, `embeddings` and `reranking`. Vision applies to chat runs;
embeddings and reranking each apply only to their matching run endpoint.
Provisioning has three forms:

| Form | Declaration | What deployment and launch do |
| --- | --- | --- |
| Files | `files` preference globs and `flag`, optionally `default: true` | Add matching files to the weights repository; inject their path into an unoccupied launch flag. |
| Runtime | `runtime`, `execution`, optional `when`, `wire_endpoints`, `health_check` and `exclude_parameters` | Install through the managed installer; qualifying local weights select the installed execution overlay. |
| Built in | `vision: {}` | Use model evidence from the engine or local source configuration. |

For llama.cpp, the shipped file declaration prefers `mmproj-F16.gguf`, then
`mmproj-BF16.gguf`, then `mmproj-*.gguf`, and sets `flag: mmproj`. MLX declares
`runtime: mlx-vlm` with `when: vision_config`, a Python execution overlay and
native Chat, Responses and Messages endpoints. vLLM and Ollama declare built-in
support. These are provider template choices; routing does not select by engine
name. The predicate checks local `config.json`; absent evidence remains unknown.
Implicit launch selection probes installed runtimes only, retaining the base
runtime when optional configuration is missing, unreadable or malformed.
Explicit runtime provisioning still requires readable qualification evidence.
A runtime feature's `health_check` replaces the whole base runtime health check,
just as `execution` replaces execution. MLX vision uses uvicorn startup logs
followed by a Chat serving probe instead of the mlx-lm startup pattern.

Endpoint features require a runtime and a `wire_endpoints` list containing their
API: `embeddings` for embeddings, `rerank` for reranking. They cannot use file
features. MLX supplies both through `mlx-vlm`, with `default: false` and no model
predicate. Install it with
`POST /zzrouter/v1/providers/mlx/install {"runtime":"mlx-vlm"}` or deploy with
`features: [embeddings]` or `[reranking]`. Start separate runs with the matching
`endpoint`; chat remains independent. Selection follows the requested endpoint,
not the model architecture. An unavailable endpoint runtime returns HTTP 400
with its install call instead of launching the base engine.

An explicit `features: {}` provider block disables shipped declarations and
survives configuration saves and reconciliation. Shipped declaration bodies refresh on later releases, including removal of
obsolete fields. Declarations are managed: edits to a shipped body are replaced,
and feature names the release does not ship are removed. Their `default` policy and all model/node feature policies
remain operator-owned; templates without declarations leave custom providers
untouched.

## Deployment and launch

Registry downloads and peer sync serialize writes to the same repository. A
waiting deployment rechecks the manifest, fetching only its missing files. Jobs
retain their own feature provisioning and cancellation; unrelated repositories
remain concurrent. Forced requests still refetch their selected files.

`POST /zzrouter/v1/deployments` separates the inference `provider` from the source
`registry`. Omitting `provider` requires one eligible provider on each target.
Omitting `features` selects provider defaults; `features: []` selects none;
`features: [vision]` requests vision explicitly. Missing default files can be
skipped on a text-only repository. Missing explicitly requested files fail before
jobs start. Runtime provisioning completes before deployment success.

Downloads are additive. Selected files are verified and recorded in the existing
manifest with feature roles; matching weights already present are retained.
`force: true` refetches the selected files from the registry and bypasses peer
sync. Without force, a peer must hold the whole concrete plan to serve as the
source, and only missing destination files are copied. `pkg/model/layout` owns
weights selection and complete shard grouping across downloads, scans and
launches. Feature files are excluded from weights, including declared globs on
repositories whose manifest has not tagged them yet.

Directories without weights or with missing indexed shards stay in the management
catalog with `incomplete: true`, so they can be deleted by name or re-downloaded.
They are excluded from serving lookups and refused by model admission. Launches
check the same layout rule even without a manifest; metadata alone is insufficient.

`restart: affected` schedules a chain that waits for successful deployment, then
restarts affected stale runs on successful targets and waits for readiness.
Follow deployment job IDs and `restart_job_id` through the jobs API; HTTP 202
alone does not establish success.

Live launch, preview and stale-run comparison share effective parameter/runtime
resolution on the launching node. An explicit tier or request value occupies its
flag even when it is an opt-out such as `auto` or `false`. Variants resolve feature
files against their base weights while retaining their own policy.
`GET /zzrouter/v1/providers/{name}/resolved?model=...&node=...` reports injected
flags with `tier: feature:vision` and known manifest `sha256`. A file with no
manifest digest cannot be checked by digest; replacing it with same-sized bytes
needs a deployment, which records one, to establish freshness.

## Readiness and inference

The management catalog at `GET /zzrouter/v1/models` reports per-node
`details.features`, with `vision` and any declared `embeddings`/`reranking` keys:

| State | Meaning |
| --- | --- |
| `present` | Node evidence establishes usable resources for the feature. Endpoint runtime installation establishes present; chat also respects policy and running snapshots. |
| `available` | Eligibility is known, but files/runtime or a fresh enabled launch are needed. |
| `unsupported` | The provider declares a `features` block without this feature, or evidence shows the model cannot do it. |
| `unknown` | Evidence is insufficient, including every provider that declares no `features` block. |

Missing projectors alone do not establish `available`: source evidence must show
that the model supports vision. Catalog listing does not discover remote HF
files. Installed resources can remain `available` while a tier opts out or an
existing text process retains its old launch snapshot. A provider with no
`features` block, cloud or not (an `ollama connect` endpoint, an operator's
OpenAI-compatible server), stays `unknown` and is never refused. Providers that
declare features derive `/v1/models` `capabilities.vision` from `present`;
providers that declare none keep the name-based guess.

Stopping or restarting a run answers, and a run's job completes, only after the
catalog is published on the serving node and acknowledged by the coordinator; a
publication failure returns 503 or fails the job. An inference request waits on the
run itself, never on that publication, and background changes (a crash, idle expiry)
stay announcements.

Inference uses the catalog cache's exact model-or-source/provider/node index,
without scanning the full model list for each replica.

`capabilities.wire_endpoints` declares the APIs the provider speaks natively.
For every provider kind, zzRouter derives `messages_compat` and
`responses_compat` when `chat_completions` is present and the corresponding
native endpoint (`messages` or `responses`) is absent, so declare native APIs
only. A provider with native Messages keeps passthrough. There is no way to
refuse a translation, because no shipped provider needs one. An explicit
`*_compat` entry still loads and means the same thing; it requires
`chat_completions` and cannot sit beside its native endpoint.

`details.wire_endpoints` reports the effective chat runtime endpoints or an existing
chat run snapshot, including derived shims. Embeddings and reranking runs never
replace this chat evidence or downgrade endpoint feature states. Snapshot and worker declarations use
the same derivation as provider config. An empty list explicitly declares none.
Missing evidence falls back to the provider's effective endpoints. Native and
translated Messages/Responses routing uses this model/provider/node evidence;
mixed replica groups prefer native endpoints before translation.

A local running chat instance's launched wire endpoints take precedence over catalog
or provider defaults, including while the catalog is invalidated. Its provider
identity is retained before translation; remote targets use their node-reported
catalog evidence. Nil launch evidence falls back; an empty list declares none.

Images are inspected structurally in Chat `image_url`, Responses `input_image`,
Messages `image` including nested tool results, Ollama chat message `images` and
Ollama generate root `images`. Admission occurs before endpoint selection,
replica fallback and stream opening. Present replicas are preferred; when none
is present, unknown replicas remain usable. A request whose every target is
`available` or `unsupported` gets an actionable 400 in its surface's error
envelope. Stored
Responses affinity stays pinned; ambiguous provider-only affinity remains
unknown rather than selecting another stateful backend.

On translated Messages, tool text, call IDs and error prefixes remain in tool
replies. URL/base64 images are lifted into the following user turn after all tool
replies. Native bodies remain unchanged. Count_tokens uses the same image rules
without inference metering.

Providers declare engine failures once in `runtime.failure_patterns`, as
`{pattern, isRegex, class}` entries. `pkg/httperr` compiles them at config
validation and uses the same classifier for readiness logs, process exit tails
and inference errors. A regex's first capture group, when present, supplies the
reason. Classified failures take priority over generic readiness detectors;
`logPatterns.failure` contains only unclassified startup failures. Reconcile
updates the managed declarations in existing installs.

The closed classes are `memory` (409 `out_of_range`), `unsupported_input`
(400 `unsupported_input`), `chat_template_rejected` (400 of the same code) and
`tool_calling_not_enabled` (400 of the same code). Status, code and remedy live
in one table in `pkg/httperr/engine_failure.go`. Memory admission unwraps to the
same typed cause as an engine memory failure. Inference memory failures also
use 409: the current request or allocation cannot fit, so automatic retries and
provider cooldown would be misleading. Free memory or lower the budget first.

Classification uses the serving provider's declarations, independently of the
engine's HTTP status. Generic decoding/allocation errors without a declared
match remain server failures; unmatched 4xx responses keep their status.
A failed run retains its typed cause before publishing its failed state.
The engine node classifies; relays preserve its code without adding remedies
again. Anthropic carries it in `error.code`, and the flat Ollama envelope uses
an optional top-level `code`. Errors before a stream opens are HTTP errors even when streaming was
requested. Already-open cold-progress streams keep HTTP 200 and emit the error
in-band; translated Responses emits `response.failed`, with no success event.

## Cluster compatibility

Protocol and minimum are 11, up from 10. Upgrade every node together. Older
strict loaders reject endpoint feature names and feature `health_check`.
Use nodes with the same cluster protocol throughout the cluster. Provider feature declarations
require compatible configuration on every launching node. Follow the
[protocol maintenance procedure](protocol_versioning.md) for incompatible upgrades.
