# Model features

A provider declares how it supplies vision in its `config.yaml`. The supported
feature name is `vision`. Provisioning has three forms:

| Form | Declaration | What deployment and launch do |
| --- | --- | --- |
| Files | `files` preference globs and `flag`, optionally `default: true` | Add matching files to the weights repository; inject their path into an unoccupied launch flag. |
| Runtime | `runtime`, `execution`, optional `when`, `wire_endpoints` and `exclude_parameters` | Install through the managed installer; qualifying local weights select the installed execution overlay. |
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

An explicit `features: {}` provider block disables shipped declarations and
survives configuration saves and reconciliation. Shipped declaration bodies refresh on later releases, including removal of
obsolete fields. Their `default` policy and all model/node feature policies
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
`details.features.vision`:

| State | Meaning |
| --- | --- |
| `present` | Node evidence establishes usable vision with the effective policy and running process. |
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

Inference uses the catalog cache's exact model-or-source/provider/node index,
without scanning the full model list for each replica.

`details.wire_endpoints` reports effective runtime endpoints or the existing
running snapshot. An empty list explicitly declares none. Missing evidence falls
back to the provider's static endpoints. Native and translated Messages/Responses
routing uses this model/provider/node evidence.

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

`pkg/httperr` classifies explicit unsupported-image backend 500s as invalid-request
400s before retry or cooldown. Genuine server failures keep their status and
class. Errors before a stream opens are HTTP errors even when streaming was
requested. Already-open cold-progress streams keep HTTP 200 and emit the error
in-band; translated Responses emits `response.failed`, with no success event.

## Cluster compatibility

Use protocol 7 nodes throughout the cluster. Provider feature declarations
require compatible configuration on every launching node. Follow the
[protocol maintenance procedure](protocol_versioning.md) for incompatible upgrades.
