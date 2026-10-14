# ComfyUI GPU tenancy

zzRouter launches ComfyUI as a node-scoped provider and manages its GPU reservation, idle lifetime and memory-pressure eviction. ComfyUI workflows and checkpoints stay outside the model catalog. Clients call ComfyUI directly using the run's `base_url`; zzRouter does not proxy its API.

## Install and enable

The shipped provider starts disabled. Managed installation requires Linux/amd64, Python 3.12 (the declared range is `>=3.12,<3.13`), a working NVIDIA GPU and at least 20 GB of free space. Allow additional space for models and outputs. A run uses one GPU, pinned by UUID in `CUDA_VISIBLE_DEVICES`. On a node with multiple GPUs, set that environment value to the full device UUID through the provider's node parameter tree.

Send one request to the coordinator with an admin key, replacing `<name>` with the target node:

```http
POST /zzrouter/v1/providers/comfyui/install?node=<name>
X-API-Key: <admin-key>
```

The response returns HTTP 202 with `job_id` and the owning `node`. Watch `GET /zzrouter/v1/jobs/<job_id>/stream?node=<returned-node>`, or poll `GET /zzrouter/v1/jobs/<job_id>?node=<returned-node>` through the coordinator. Wait for terminal success and check `GET /zzrouter/v1/providers/comfyui/status?node=<returned-node>`. Successful installation verifies and activates the payload before enabling the provider on that node. Acceptance alone does not mean installation succeeded. The TUI install wizard and MCP `zz_install` use the same path; hook-only plans require server execution through Run All or individual steps.

zzRouter installs the release-pinned ComfyUI 0.40.0 archive into `<install_dir>/comfyui/source/` and its dependencies into `comfyui/venv/`, where `<install_dir>` comes from `providers.install_dir`. It verifies the shipped archive SHA256 before extraction, resolves the archive's requirements through PyPI and installs the accepted inventory with `--require-hashes --no-deps --only-binary=:all:`. Reproducibility follows vLLM's current standard: the source bytes are pinned, and the dependency resolution is hashed at install time. Dependency versions can change between resolutions as PyPI changes. Launch does not fetch or install dependencies.

Source and venv are staged, checked with the installed interpreter and activated together. A failed install or upgrade restores the previous payload and records. Installed version comes from the managed completion record. A new zzRouter release with a new source pin supplies the upgrade target; use the existing upgrade or force-install flow after stopping ComfyUI. The source declaration cannot be overridden through install requests or node configuration, and disposable source installs and launches are refused.

An incomplete managed record reports `managed installation record is incomplete; reinstall with force`. Force can repair a managed payload with valid, matching recorded ownership. A different recorded working directory refuses install even with force: uninstall first. Uninstall uses the source directory recorded by the installation, keeps data and preserves unrelated directories. Missing or unsafe recorded ownership cannot authorize deletion or be inferred from a stray version file.

Mutable files stay under `comfyui/data/`. The existing `runtime.data_dir: data` expands `--base-directory ${DATA_DIR}` to that absolute path; `model_store.directory: models` selects the model root. The installer creates only a missing data root and declared model-store categories. ComfyUI creates its input, output, user and temp folders itself. Install, upgrade and uninstall preserve existing data, including models, custom nodes and workflow files; uninstall removes only the managed payload and records and also keeps any operator `python/` directory. No data purge is implied.

Managed source and venv belong to the service account; existing data keeps its ownership and permissions and must be writable by that account. With the default `harden_venv` policy, source and venv are read-only, so ordinary extension pip installs at runtime fail; data remains writable. This preserves the existing provider permission model: the same owner can change permissions, so hardening is not a separate privilege boundary. Custom nodes remain operator-managed in `data/custom_nodes/`; managed custom-node installation is deferred to phase 4. The installer does not install custom nodes or ComfyUI-Manager.

An existing hand installation is refused even with force: `Operator installation found in <provider dir>; stop it and move source and venv aside before a managed install. Data and python are kept.` Stop its zzRouter run or operator-managed process first. On the target host, move the existing source and venv into an operator backup outside the provider directory, leaving data and python in place. For a layout containing both payload directories:

```sh
provider_dir=/path/to/install_dir/comfyui
backup_dir="${provider_dir}.operator-backup-$(date +%Y%m%d%H%M%S)"
mkdir -m 700 "$backup_dir"
mv "$provider_dir/source" "$backup_dir/source"
mv "$provider_dir/venv" "$backup_dir/venv"
```

If only one payload exists, move that directory. Retry the install request after moving the payloads aside; existing data stays in use. Configure node parameters through `PATCH /zzrouter/v1/providers/comfyui/parameters` using the existing `nodes.<node>.parameters` tree. Defaults and the selected node are the configuration tiers; model tiers and endpoint overlays are refused.

## Model files through the coordinator

Use the coordinator with an admin key; every operation names one node. The filesystem inventory works while ComfyUI is stopped. Provider files do not appear in `/v1/models`, the LLM catalog, deployments or variants. The template's closed categories are `diffusion_models`, `text_encoders`, `vae`, `loras`, `checkpoints`, `controlnet`, `clip_vision`, `upscale_models`, `embeddings`, `audio_encoders`, `model_patches` and `latent_upscale_models`.

```sh
zzrouter providers models list comfyui --node localai
zzrouter providers models download comfyui --node localai \
  --category diffusion_models \
  --source 'Comfy-Org/flux1-schnell#flux1-schnell-fp8.safetensors' --watch
zzrouter providers models resolve comfyui --node localai --workflow flux-workflow.json
zzrouter providers models resolve comfyui --node localai --workflow flux-workflow.json --download
zzrouter providers models delete comfyui --node localai \
  --category diffusion_models --name flux1-schnell-fp8.safetensors
```

List accepts an optional `--category`. Download accepts an optional `--name`, defaulting to the source basename. Delete requires both category and name. Names are relative paths inside the category; absolute paths, traversal, bookkeeping names and symlink escapes are refused. Stores must stay inside the provider directory and cannot overlap the LLM models root in either direction. Downloads do not overwrite files: `file_exists` and `download_in_progress` are 409 refusals. A file copied in during a transfer is preserved; the job fails with `meta.error_code: file_exists`. Delete removes only a regular file and refuses a reserved destination. A cancelled transfer retains its reservation until the producer unwinds, then cleans partial data. Known download sizes reuse the disk-space preflight; an unknown size skips it and disk errors surface through the job.

Sources are exact Hugging Face `org/repo#file` references or HTTPS `/resolve/<revision>/<file>` URLs. The exact query `?download=true` is discarded; other queries and other URL hosts are refused. Gated repositories use the existing HF token on the downloading node. `--watch` follows the existing job stream on the returned owning node and reports a failed job as a command error. Listing, deletion and resolution print the complete response data, including item reasons and job IDs; global `-o json` and `-o yaml` are supported.

Resolution reads only UI `nodes[].properties.models[]` entries `{name,url,directory}`, including a submission envelope's `extra_data.extra_pnginfo.workflow`. It deduplicates `(category,name)`; conflicting sources are reported, never guessed. Raw API prompts without that metadata return 400. A positive-size regular file is `present`. Other items are `missing`, `downloading`, `unresolvable` (with `invalid_model_category`, `source_unsupported`, `conflicting_source` or `missing_source`), or `failed` (the typed problem code from a refused start). An accepted earlier job remains in the response if a later start fails. Resolve returns 202 if it accepted any job, otherwise 200. The request limit is 4 MiB. Downloads can run while ComfyUI is up: ComfyUI rescans folders on request, and zzRouter keeps no model-file cache to invalidate.

### Agent recipe

There is no additional MCP task tool. Discover `listProviderModels`, `downloadProviderModel`, `deleteProviderModel` and `resolveProviderModels` with `zz_api_search`. Read inventory with `zz_api_get`:

```json
{"path":"/zzrouter/v1/providers/comfyui/models","query":{"node":"localai"}}
```

Start one file with `zz_api`:

```json
{"method":"POST","path":"/zzrouter/v1/providers/comfyui/models","idempotency_key":"flux-schnell-localai","body":{"node":"localai","category":"diffusion_models","source":"Comfy-Org/flux1-schnell#flux1-schnell-fp8.safetensors"}}
```

To fetch a workflow's requirements, call `zz_api` on `/zzrouter/v1/providers/comfyui/models/resolve` with `{"node":"localai","workflow":<workflow JSON object>,"download":true}` in `body`. Both POST routes accept `idempotency_key`; retry with the same body and key. Preserve each returned job ID, follow it with `zz_job` using the returned node, and inspect every unresolved or failed item. Verify the resulting inventory before calling `zz_run_start` with provider and node; submit the workflow directly to its `base_url`. For deletion, use `zz_api` with method `DELETE` on the models route and put node, category and name in `query`.

## Ensure a run

Send `POST /zzrouter/v1/runs` with `{"provider":"comfyui","node":"localai"}`. Omit both model fields and endpoint. Concurrent requests on one node share the run. The response returns its provider, run id, launch job and `base_url`. Follow the job for readiness; list, stop, restart and logs use the run id.

The CLI equivalent is `zzrouter run --provider comfyui --node localai --watch`. MCP `zz_run_start` and `zz_run_preview` accept the same provider without a model, and run tools return `base_url`.

Port 8188 is a firm singleton pool pin. An occupied port fails because the pool has no alternative port. Choose another singleton range in provider configuration when needed. URLs use the node address selected from bind and advertised IP settings, with IPv6 brackets when required. Keep the ComfyUI `listen` parameter consistent with the address family and reachability clients need.

## Memory and activity

`gpu-reserve-mib` defaults to 8192 and must be positive and no larger than the selected GPU. zzRouter keeps that commitment after readiness. Other launches see driver free memory minus each live reservation's unused portion. Memory-fit errors report tracked holders, including unused commitments; idle holders can be reclaimed when `evict-idle-on-memory-pressure` is enabled. Untracked processes are never stopped. Served Ollama model runs do not expose per-model process ownership to driver accounting; their daemon memory cannot currently be reclaimed reliably by this admission path. Stop or unload those models explicitly when they block a ComfyUI reservation.

The reservation parameter stays out of argv. Shared launch, preview and staleness preparation derives `--reserve-vram` as (device total minus reservation) / 1024 in GiB, preserving the exact fractional value. ComfyUI treats it as an allocator limit: in the lab (ComfyUI 0.39.2) a 16 GiB reservation held a Qwen Image render that peaks near 29 GB unreserved to about the reserved amount, offloading instead. It is not an OS-enforced limit, so retest after ComfyUI upgrades. Accounting retains the reservation independently of that hint. Engines whose memory footprint cannot be measured refuse to consume unused reservations until those idle tenants are reclaimed.

The existing health scheduler samples `queue_running` and `queue_pending`. Observed-busy runs hold idle exclusion without consuming request slots. Every busy sample refreshes the idle clock, and busy-to-idle resets it. Failed, timed-out or malformed queue samples retain the previous state until they reach the health check's retry count (`runtime.health_check.retries`, or the monitor default). Reaching that threshold releases the busy hold and resets the idle clock. A successful sample resets that failure count. This bounds stale busy evidence even while `/system_stats` remains healthy. Stop cancels and drains sampling.

Observed-busy runs are protected. A direct submission between an idle sample and the next sample can race eviction. Ensure refreshes activity and rejects an already claimed idle stop, but it is not a lease over a future direct job. Run reports expose `engine_busy` and `memory_commitments_mib`.

## Access

ComfyUI has no authentication and custom nodes execute as the host account. Anyone who can reach its port can use the GPU and that account. Use `listen: "127.0.0.1"` for single-host access, or restrict network access at the host boundary. A zzRouter admin key controls the run lifecycle; it does not authenticate direct ComfyUI requests or meter them.
