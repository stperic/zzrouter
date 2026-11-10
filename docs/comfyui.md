# ComfyUI GPU tenancy

zzRouter launches ComfyUI as a node-scoped provider and manages its GPU reservation, idle lifetime and memory-pressure eviction. ComfyUI workflows and checkpoints stay outside the model catalog. Clients call ComfyUI directly using the run's `base_url`; zzRouter does not proxy its API.

An isolated node-scoped launch job completes when the engine becomes healthy and its reservation is admitted. If admission changes a model run's serving state, the job also waits for that catalog change to reach the coordinator, including an eviction whose stop fails. Unconfirmed stops do not count as successful evictions or reclaimed memory. A failed acknowledgment keeps the job failed even when the engine is healthy: `code: catalog_publication_failed`, `catalog_publication: failed` and `run_status` distinguish the publication failure from an engine failure. Inspect the returned run and existing job before launching again. Later stops consider their own effects, rather than prior launch evictions.

Only the coordinator initiates cluster connections, including enrollment, renewal, provider onboarding and model replication. Workers return typed operations through coordinator-held long polls. Catalog acknowledgment still means publication completed. See [cluster networking](cluster_networking.md). Resource samples use the existing collector and coordinator health probes. Coordinator resource observations expire after two missed health cadences, while its own metrics come directly from its local tracker.

## Install and enable

The shipped provider starts disabled. Managed installation requires Linux/amd64 or Windows/amd64, Python 3.10 to 3.14 (the declared range is `>=3.10,<3.15`, following upstream), a working NVIDIA GPU and at least 20 GB of free space. Allow additional space for models and outputs. A run uses one GPU, pinned by UUID in `CUDA_VISIBLE_DEVICES`. On a node with multiple GPUs, set that environment value to the full device UUID through the provider's node parameter tree.

On Windows, provision a supported 64-bit Python for the service account, including
pip and venv. The installer discovers PEP 514 registrations for that account and
machine-wide 64-bit installations without changing PATH. NVIDIA drivers remain a
host prerequisite. The release selects official CUDA 13.2 PyTorch wheels on
Windows; Linux keeps its existing PyPI resolution. CPU wheels and mismatched CUDA
companions fail verification. Choose an explicit node/request GPU reservation
that fits its available VRAM when the default 8192 MiB reservation cannot fit.

Send one request to the coordinator with an admin key, replacing `<name>` with the target node:

```http
POST /zzrouter/v1/providers/comfyui/install?node=<name>
X-API-Key: <admin-key>
```

The response returns HTTP 202 with `job_id` and the owning `node`. Watch `GET /zzrouter/v1/jobs/<job_id>/stream?node=<returned-node>`, or poll `GET /zzrouter/v1/jobs/<job_id>?node=<returned-node>` through the coordinator. Wait for terminal success and check `GET /zzrouter/v1/providers/comfyui/status?node=<returned-node>`. Successful installation verifies and activates the payload before enabling the provider on that node. Acceptance alone does not mean installation succeeded. The TUI install wizard and MCP `zz_install` use the same path; hook-only plans require server execution through Run All or individual steps.

zzRouter installs the release-pinned ComfyUI 0.44.0 archive into `<install_dir>/comfyui/source/` and its dependencies into `comfyui/venv/`, where `<install_dir>` comes from `providers.install_dir`. It verifies the shipped archive SHA256 before extraction, resolves the archive's requirements through PyPI and installs the accepted inventory with `--require-hashes --no-deps --only-binary=:all:`. Reproducibility follows vLLM's current standard: the source bytes are pinned, and the dependency resolution is hashed at install time. Dependency versions can change between resolutions as PyPI changes. Launch does not fetch or install dependencies.

The accepted dependency step allows 90 minutes. Provider install jobs follow the [shared job lifecycle](api.md), including cancellation and shutdown.

If an index supplies wheel metadata without an artifact SHA256, planning downloads that exact selected wheel without dependencies and computes its digest. Supplied hashes remain unchanged. Only complete, validated inventories can become install plans. Hashless source archives remain refused. The resolver deadline bounds acquisition time; an 8 GiB post-download size limit bounds accepted hashless wheels. These downloads stay inside the disposable resolver directory.

Source and venv are staged, checked with the installed interpreter and activated together. A failed install or upgrade restores the previous payload and records. Installed version comes from the managed completion record. A new zzRouter release with a new source pin supplies the upgrade target; use the existing upgrade or force-install flow after stopping ComfyUI. The source declaration cannot be overridden through install requests or node configuration, and disposable source installs and launches are refused. After a source pin changes, installed verification and environment imports of the older recipe are refused until upgrade completes. Payload rollback preserves the older runtime for ordinary launches, but does not restore diagnostic authority under the newer release.

An incomplete managed record reports `managed installation record is incomplete; reinstall with force`. Force can repair a managed payload with valid, matching recorded ownership. A different recorded working directory refuses install even with force: uninstall first. Uninstall uses the source directory recorded by the installation, keeps data and preserves unrelated directories. Missing or unsafe recorded ownership cannot authorize deletion or be inferred from a stray version file.

Mutable files stay under `comfyui/data/`. The existing `runtime.data_dir: data` expands `--base-directory ${DATA_DIR}` to that absolute path; `model_store.directory: models` selects the model root. The installer creates only a missing data root and declared model-store categories. ComfyUI creates its input, output, user and temp folders itself. Install, upgrade and uninstall preserve existing data, including models, custom nodes and workflow files; uninstall removes only the managed payload and records and also keeps any operator `python/` directory. No data purge is implied.

Managed source and venv belong to the service account; existing data keeps its ownership and permissions and must be writable by that account. With the default `harden_venv` policy, source and venv are read-only, so ordinary extension pip installs at runtime fail; data remains writable. This preserves the existing provider permission model: the same owner can change permissions, so hardening is not a separate privilege boundary. Custom nodes remain operator-managed in `data/custom_nodes/`; their Python dependencies can be approved through the protected install policy below. The installer does not install custom-node code or ComfyUI-Manager.

An existing hand installation is refused even with force: `Operator installation found in <provider dir>; stop it and move source and venv aside before a managed install. Data and python are kept.` Stop its zzRouter run or operator-managed process first. On the target host, move the existing source and venv into an operator backup outside the provider directory, leaving data and python in place. For a layout containing both payload directories:

```sh
provider_dir=/path/to/install_dir/comfyui
backup_dir="${provider_dir}.operator-backup-$(date +%Y%m%d%H%M%S)"
mkdir -m 700 "$backup_dir"
mv "$provider_dir/source" "$backup_dir/source"
mv "$provider_dir/venv" "$backup_dir/venv"
```

If only one payload exists, move that directory. Retry the install request after moving the payloads aside; existing data stays in use. Configure node parameters through `PATCH /zzrouter/v1/providers/comfyui/parameters` using the existing `nodes.<node>.parameters` tree. Defaults and the selected node are the configuration tiers; model tiers and endpoint overlays are refused.

## Custom-node dependencies

The human node operator selects additional distributions in the existing protected
file named by `providers.install_policy_file`. Provisioning, file ownership and the
service identity requirements are in [managed recipe authority](typed_provider_recipes.md#authority-and-threat-boundary).
The API and peer sync cannot select packages, change grants or override the release
source, URL, SHA256, indexes, checks or binary-only policy. Preserve unrelated runtime
grants when adding this entry on the owning node:

```yaml
runtimes:
  comfyui/comfyui:
    additional_packages:
      torchaudio: ""
      transformers: ">=5.3,<6"
```

This is an illustrative selection, not a verified node compatibility recipe. Use
normalized distribution names and plain version constraints, at most 128 roots.
An empty constraint permits resolver selection within all other requirements.
The operator reviews the requirements for the chosen nodes and engines; zzRouter
never reads requirements from `data/custom_nodes` or executes their `install.py`.
One resolver intersects additions with release requirements and installs the hashed
inventory into a fresh candidate venv. Conflicts or missing compatible wheels fail
closed and preserve the live payload. Source builds, special `--no-deps` setup and
imperative node patching are unsupported. System libraries and executables such as
espeak remain human work. Nodes execute as the service account; approval of roots
also trusts their resolver-selected transitive dependencies. Hardening is not a sandbox.

Stop ComfyUI, edit the protected map, preview `POST /zzrouter/v1/providers/comfyui/install/plan?node=<node>`
with `{"force":true}`, then execute the install route with
`{"force":true,"expected_plan_id":"<returned-plan-id>"}`. Follow the returned owning-node
job to terminal success. Policy changes invalidate accepted plans and require a fresh
preview; they do not automatically reinstall or stop a run. Keep node files and data
in place. To remove all additions, use `additional_packages: {}` and force reinstall;
packages still required by the release or other dependencies can remain.

`GET /zzrouter/v1/providers/comfyui/environment?node=<node>` reports desired roots in
`install_authority.grant.additional_packages`, installed roots in
`installed_manifest.additional_packages`, exact inventory and `recipe_drift` after
selection or constraint changes. Unrelated runtime grants do not cause recipe drift.
The status endpoint keeps the source version and run summary; a dependency-only
reinstall keeps that source version. Revoked installed additions block native diagnostic
imports until the operator restores authority or reinstalls. Existing install verify
checks packages and known torch/CUDA compatibility, including approved torchaudio;
the operator must still exercise the chosen node's lazy engines/workflows. Adding
`gitpython` can address Manager's `git` import, but does not enable its pip/uv installer.

## Model files through the coordinator

Use the coordinator with an admin key; every operation names one node. The filesystem inventory works while ComfyUI is stopped. Provider files do not appear in `/v1/models`, the LLM catalog, deployments or variants. The template's closed categories are `diffusion_models`, `text_encoders`, `vae`, `loras`, `checkpoints`, `controlnet`, `clip_vision`, `upscale_models`, `embeddings`, `audio_encoders`, `model_patches` and `latent_upscale_models`.

```sh
zzrouter providers models list comfyui --node localai
zzrouter providers models download comfyui --node localai \
  --category diffusion_models \
  --source 'Comfy-Org/flux1-schnell#flux1-schnell-fp8.safetensors' --watch
zzrouter providers models templates comfyui --node localai --search 'Qwen Image 2.1 Turbo'
zzrouter providers models resolve comfyui --node localai \
  --template 'Qwen Image 2.1 Turbo: Text to Image' --download
zzrouter providers models resolve comfyui --node localai --workflow flux-workflow.json
zzrouter providers models resolve comfyui --node localai --workflow flux-workflow.json --download
zzrouter providers models delete comfyui --node localai \
  --category diffusion_models --name flux1-schnell-fp8.safetensors
```

List accepts an optional `--category`. Download accepts an optional `--name`, defaulting to the source basename. Delete requires both category and name. Names are relative paths inside the category; absolute paths, traversal, bookkeeping names and symlink escapes are refused. Stores must stay inside the provider directory and cannot overlap the LLM models root in either direction. Downloads do not overwrite files: `file_exists` and `download_in_progress` are 409 refusals. A file copied in during a transfer is preserved; the job fails with `meta.error_code: file_exists`. Delete removes only a regular file and refuses a reserved destination. A cancelled transfer retains its reservation until the producer unwinds, then cleans partial data. Known download sizes reuse the disk-space preflight; an unknown size skips it and disk errors surface through the job.

Sources are exact Hugging Face `org/repo#file` references or HTTPS `/resolve/<revision>/<file>` URLs. The exact query `?download=true` is discarded; other queries and other URL hosts are refused. Gated repositories use the existing HF token on the downloading node. `--watch` follows the existing job stream on the returned owning node and reports a failed job as a command error. Listing, deletion and resolution print the complete response data, including item reasons and job IDs; global `-o json` and `-o yaml` are supported.

Resolution accepts exactly one `template` selector or `workflow` object. A template selector is a case-sensitive official ComfyUI catalog ID or a unique exact title compared without case. zzRouter fetches the official workflow on the coordinator; callers do not fetch JSON or choose model filenames. `GET /zzrouter/v1/providers/:name/templates?node=<node>&search=<text>` lists IDs, titles and version requirements with limit/offset pagination. API-only templates cannot provision local models. Template results include the selected identity and workflow SHA256.

Both template and workflow inputs use UI `nodes[].properties.models[]` entries `{name,url,directory}`, including a submission envelope's `extra_data.extra_pnginfo.workflow`. The same parser follows referenced subgraphs through nested `definitions.subgraphs`, supports explicit legacy graph IDs and shares equivalent repeated definitions. Unused definitions do not trigger downloads. Conflicting definitions and dangling UUID references are refused before admission. It deduplicates `(category,name)`; conflicting sources are reported, never guessed. Raw API prompts without that metadata return 400. A positive-size regular file is `present`. Other items are `missing`, `downloading`, `unresolvable` (with `invalid_model_category`, `source_unsupported`, `conflicting_source` or `missing_source`), or `failed` (the typed problem code from a refused start). An accepted earlier job remains in the response if a later start fails. Resolve returns 202 if it accepted any job, otherwise 200. The request limit is 4 MiB. Downloads can run while ComfyUI is up: ComfyUI rescans folders on request, and zzRouter keeps no model-file cache to invalidate.

### Agent recipe

For template discovery and recommendation requests, use this research sequence:

1. Shortlist with `listProviderTemplates`, using `q`, `sort=date`, `order=desc` and
   `limit=10`. Use `open_source=true` when researching local model workflows.
   Describe the returned date as a catalog date. A catalog lookup supports a
   summary of what is new; detailed recommendations require reading the graph.
2. Fetch each shortlisted ID with `getProviderTemplate`. Retain its metadata,
   source URL, `workflow_sha256` and complete `workflow` without rounding JSON
   numbers. Inspect referenced nested subgraphs, links, exposed inputs, modes and
   switches. Linked values and subgraph instance inputs override stored widget
   defaults; trace them before reporting effective sampling or model settings.
   Identify required input assets, model files, prompt processing and outputs.
3. For local model workflows, pass that exact returned `workflow` to
   `resolveProviderModels` with `node` and `download:false`. Report each declared
   file as present, missing or unresolvable, including its reason. Resolve-by-name
   fetches the source again, so use the already-read graph for this check. An
   API-only workflow needs its API prerequisites checked instead of local weights.
4. Check the named node's installed ComfyUI version against the declared minimum.
   Check required node classes through the native `/object_info` when a ComfyUI
   run is available. Otherwise leave node availability unverified. Confirm required
   input images or other assets and API access separately. Files marked present
   establish inventory presence; they do not establish version compatibility,
   custom-node availability, valid weights, output quality or execution readiness.
5. State what was inferred from metadata, inspected in the graph, checked on the
   node and actually executed. For a requested generation test, use the native
   frontend's exported API prompt and inspect the resulting output. Keep research
   read-only unless the task authorizes downloads, upgrades, installs or execution.

There is no additional MCP task tool. Discover `listProviderModels`, `downloadProviderModel`, `deleteProviderModel` and `resolveProviderModels` with `zz_api_search`. Read inventory with `zz_api_get`:

```json
{"path":"/zzrouter/v1/providers/comfyui/models","query":{"node":"localai"}}
```

Start one file with `zz_api`:

```json
{"method":"POST","path":"/zzrouter/v1/providers/comfyui/models","idempotency_key":"flux-schnell-localai","body":{"node":"localai","category":"diffusion_models","source":"Comfy-Org/flux1-schnell#flux1-schnell-fp8.safetensors"}}
```

To install by template name, discover `listProviderTemplates` and `resolveProviderModels` with `zz_api_search`. Call `zz_api_get` on `/zzrouter/v1/providers/comfyui/templates` with `query: {"node":"localai","search":"Qwen Image 2.1 Turbo"}`. Then call `zz_api` with method `POST`, path `/zzrouter/v1/providers/comfyui/models/resolve` and `body: {"node":"localai","template":"image_qwen_image_2_1_turbo_t2i","download":true}`. Existing files are kept, and concurrent requests share existing download jobs.

To retrieve the graph, discover `getProviderTemplate` and call `zz_api_get` on `/zzrouter/v1/providers/comfyui/templates/image_qwen_image_2_1_turbo_t2i` with `query: {"node":"localai"}`. The CLI equivalent is `zzrouter providers models templates comfyui --node localai --template image_qwen_image_2_1_turbo_t2i`. This read-only operation returns metadata and `workflow`, without provisioning files. Pass that exact `workflow` object to the existing resolver with `download:true` to provision it without a second upstream fetch. A later GET is an ordinary current read, not an idempotent replay. MCP can refuse a large graph through its existing response-size limit without starting downloads; REST and CLI retain the source reader's 4 MiB document bound.

The returned object is a UI workflow: load it in ComfyUI's native frontend, which owns subgraph, virtual-node and custom-widget conversion. Export API format there for direct `/prompt` calls. zzRouter does not compile UI graphs, proxy generation or execute a template from its selector. The template's `workflow_sha256` identifies the fetched source bytes; response encoding may change whitespace or escaping without changing graph content.

Catalog entries report `model_provisioning: unverified`; explicitly API-only templates report `unsupported`. Resolution reports `supported` when all declared file requirements are resolvable on the named node, or `unsupported` when any is unresolvable. These fields describe model files, not engine version, installed custom nodes or execution readiness. Follow each requirement and job even when provisioning is supported. Missing model declarations return `workflow_model_metadata_missing` and `model_provisioning: unverified`; unsupported categories remain `invalid_model_category`. Neither case guesses sources or installs custom nodes.

To fetch a saved workflow's requirements, call `zz_api` on `/zzrouter/v1/providers/comfyui/models/resolve` with `{"node":"localai","workflow":<workflow JSON object>,"download":true}` in `body`. Both POST routes accept `idempotency_key`; retry with the same body and key. Preserve each returned job ID, follow it with `zz_job` using the returned node, and inspect every unresolved or failed item. Verify the resulting inventory before calling `zz_run_start` with provider and node; submit the frontend-exported API prompt to `/prompt` at its `base_url`. For deletion, use `zz_api` with method `DELETE` on the models route and put node, category and name in `query`.

## Ensure a run

Send `POST /zzrouter/v1/runs` with `{"provider":"comfyui","node":"localai"}`. Omit both model fields and endpoint. Concurrent requests on one node share the run. The response returns its provider, run id, launch job and `base_url`. Follow the job for readiness; list, stop, restart and logs use the run id.

The CLI equivalent is `zzrouter run --provider comfyui --node localai --watch`. MCP `zz_run_start` and `zz_run_preview` accept the same provider without a model, and run tools return `base_url`.

Port 8188 is a firm singleton pool pin. An occupied port fails because the pool has no alternative port. Choose another singleton range in provider configuration when needed. URLs use the node address selected from bind and advertised IP settings, with IPv6 brackets when required. Keep the ComfyUI `listen` parameter consistent with the address family and reachability clients need.

## Memory and activity

`gpu-reserve-mib` defaults to 8192 and must be positive and no larger than the selected GPU. zzRouter keeps that commitment after readiness. Other launches see driver free memory minus each live reservation's unused portion. Memory-fit errors report tracked holders, including unused commitments; unpinned idle holders can be reclaimed when `evict-idle-on-memory-pressure` is enabled. Untracked processes are never stopped. Served Ollama model runs do not expose per-model process ownership to driver accounting; their daemon memory cannot currently be reclaimed reliably by this admission path. Stop or unload those models explicitly when they block a ComfyUI reservation.

The reservation parameter stays out of argv. Shared launch, preview and staleness preparation derives `--reserve-vram` as (device total minus reservation) / 1024 in GiB, preserving the exact fractional value. ComfyUI treats it as an allocator limit: in the lab (ComfyUI 0.39.2) a 16 GiB reservation held a Qwen Image render that peaks near 29 GB unreserved to about the reserved amount, offloading instead. It is not an OS-enforced limit, so retest after ComfyUI upgrades. Accounting retains the reservation independently of that hint. Engines whose memory footprint cannot be measured refuse to consume unused reservations until those unpinned idle tenants are reclaimed.

The existing health scheduler samples `queue_running` and `queue_pending`. Observed-busy runs hold idle exclusion without consuming request slots. Every busy sample refreshes the idle clock, and busy-to-idle resets it. Failed, timed-out or malformed queue samples retain the previous state until they reach the health check's retry count (`runtime.health_check.retries`, or the monitor default). Reaching that threshold releases the busy hold and resets the idle clock. A successful sample resets that failure count. This bounds stale busy evidence even while `/system_stats` remains healthy. Stop cancels and drains sampling.

Observed-busy runs are protected. A direct submission between an idle sample and the next sample can race eviction. Ensure refreshes activity and rejects an already claimed idle stop, but it is not a lease over a future direct job. Run reports expose `engine_busy`, `memory_commitments_mib` and `pinned: true` when pinned. A negative launch `keep-alive` pins the reservation against router idle expiry and pressure reclaim, even after inference hints change its timer. Use explicit stop to release it; tier changes apply on restart. Pins do not autostart after a node restart.

## Access

ComfyUI has no authentication and custom nodes execute as the host account. Anyone who can reach its port can use the GPU and that account. Use `listen: "127.0.0.1"` for single-host access, or restrict network access at the host boundary. A zzRouter admin key controls the run lifecycle; it does not authenticate direct ComfyUI requests or meter them.

### Catalog queries

Use the existing list endpoint for the newest ten:

```http
GET /zzrouter/v1/providers/comfyui/templates?node=localai&sort=date&order=desc&limit=10
```

Combine `q=Qwen` (`search` remains an alias), `since=2026-10-01` and `open_source=true`.
`since` includes its date; unknown dates are excluded. Omitted `open_source` includes unknown metadata;
explicit false selects API-only entries. These flags describe upstream metadata, not execution readiness.
`date` is the catalog date, not a model release timestamp. Unknown dates sort last in either direction;
equal sort values use ID ascending. `sort=id|title|date`, `order=asc|desc`; defaults remain id/asc.
Filters and sorting precede pagination. `total` counts matching entries. For the full catalog use
`limit=1000`, then increase `offset` while `has_more` is true. MCP's separate response-size limit still applies.

```sh
zzrouter providers models templates comfyui --node localai --sort date --order desc --limit 10
```

Registry model discovery uses the same `q`, `sort`, `order`, `limit` and `offset` parameters on
`GET /zzrouter/v1/search?type=models&provider=huggingface&sort=date&order=desc&limit=10`.
`date` means creation date for models. HuggingFace also accepts `createdAt`, `lastModified` and `popular` (trending);
Ollama only has approximate updated dates, so use `lastModified` there. Cloud missing dates remain unknown.
Unsupported sort/filter combinations return400. Author/tags apply to HuggingFace; since/open_source
are template-specific. HuggingFace numeric offsets consume upstream cursor pages; prefer returned
`pagination.cursor.next` via `cursor=...&offset=0` for continuation. Its totals remain inexact.
The existing `zz_model_search` tool accepts sort/order/author/tags/logic, and generic `zz_api_get`
accepts all documented query parameters. Parameter names are discoverable through `zz_api_search`;
full schemas and semantics are in `/openapi.yaml`.
