# ComfyUI GPU tenancy

zzRouter launches ComfyUI as a node-scoped provider and manages its GPU reservation, idle lifetime and memory-pressure eviction. ComfyUI workflows and checkpoints stay outside the model catalog. Clients call ComfyUI directly using the run's `base_url`; zzRouter does not proxy its API.

## Install and enable

The shipped provider is disabled. Linux and a working NVIDIA GPU are required. A run uses one GPU, pinned by UUID in `CUDA_VISIBLE_DEVICES`. On a node with multiple GPUs, set that environment value to the full device UUID through the provider's node parameter tree.

The host operator installs ComfyUI into the provider install directory selected by `providers.install_dir`. Use sibling directories: `comfyui/source/` for the ComfyUI code and working directory, `comfyui/venv/` for the Python environment, and `comfyui/data/` holding `models`, `custom_nodes`, `input`, `output`, `user` and `temp`. The interpreter is `comfyui/venv/bin/python3` and the entry point is `comfyui/source/main.py`. The template declares `runtime.data_dir: data` once: the `--base-directory ${DATA_DIR}` argument expands to that absolute directory, and `model_store.directory: models` selects its model root. Keep mutable data separate so a future managed install can replace source and venv without touching it. Create the data layout before enabling the provider. The service account must be able to execute the interpreter, read the source and dependencies, and read and write the data directories. Verify the installed source and dependencies with that interpreter before enabling it.

Provider status reports not installed until the interpreter and source entry point exist. The install API refuses this provider with the required host directory in its message. zzRouter does not download or update this source installation.

On the coordinator, enable the provider with `PATCH /zzrouter/v1/providers/comfyui` and `{"enabled":true}`. Configure node parameters through `PATCH /zzrouter/v1/providers/comfyui/parameters` using the existing `nodes.<node>.parameters` tree. Defaults and the selected node are the configuration tiers; model tiers and endpoint overlays are refused.

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
