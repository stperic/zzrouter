# ComfyUI GPU tenancy

zzRouter launches ComfyUI as a node-scoped provider and manages its GPU reservation, idle lifetime and memory-pressure eviction. ComfyUI workflows and checkpoints stay outside the model catalog. Clients call ComfyUI directly using the run's `base_url`; zzRouter does not proxy its API.

## Install and enable

The shipped provider is disabled. Linux and a working NVIDIA GPU are required. A run uses one GPU, pinned by UUID in `CUDA_VISIBLE_DEVICES`. On a node with multiple GPUs, set that environment value to the full device UUID through the provider's node parameter tree.

The host operator installs ComfyUI into the provider install directory selected by `providers.install_dir`. Its layout is `comfyui/venv/bin/python3` plus `comfyui/source/main.py` and the rest of the source tree. The service account must be able to execute the interpreter and read the source, dependencies, workflows and checkpoints. Verify the installed source and dependencies with that interpreter before enabling it.

Provider status reports not installed until the interpreter and source entry point exist. The install API refuses this provider with the required host directory in its message. zzRouter does not download or update this source installation.

On the coordinator, enable the provider with `PATCH /zzrouter/v1/providers/comfyui` and `{"enabled":true}`. Configure node parameters through `PATCH /zzrouter/v1/providers/comfyui/parameters` using the existing `nodes.<node>.parameters` tree. Defaults and the selected node are the configuration tiers; model tiers and endpoint overlays are refused.

## Ensure a run

Send `POST /zzrouter/v1/runs` with `{"provider":"comfyui","node":"localai"}`. Omit both model fields and endpoint. Concurrent requests on one node share the run. The response returns its provider, run id, launch job and `base_url`. Follow the job for readiness; list, stop, restart and logs use the run id.

The CLI equivalent is `zzrouter run --provider comfyui --node localai --watch`. MCP `zz_run_start` and `zz_run_preview` accept the same provider without a model, and run tools return `base_url`.

Port 8188 is a firm singleton pool pin. An occupied port fails because the pool has no alternative port. Choose another singleton range in provider configuration when needed. URLs use the node address selected from bind and advertised IP settings, with IPv6 brackets when required. Keep the ComfyUI `listen` parameter consistent with the address family and reachability clients need.

## Memory and activity

`gpu-reserve-mib` defaults to 8192 and must be positive and no larger than the selected GPU. zzRouter keeps that commitment after readiness. Other launches see driver free memory minus each live reservation's unused portion. Memory-fit errors report tracked holders, including unused commitments; idle holders can be reclaimed when `evict-idle-on-memory-pressure` is enabled. Untracked processes are never stopped. Served Ollama model runs do not expose per-model process ownership to driver accounting; their daemon memory cannot currently be reclaimed reliably by this admission path. Stop or unload those models explicitly when they block a ComfyUI reservation.

The reservation parameter stays out of argv. Shared launch, preview and staleness preparation derives `--reserve-vram` as (device total minus reservation) in GiB, rounded down. This allocator hint is not a verified hard cap; test the operator's ComfyUI version and workloads in the lab. Accounting retains the reservation independently of that hint. Engines whose memory footprint cannot be measured refuse to consume unused reservations until those idle tenants are reclaimed.

The existing health scheduler samples `queue_running` and `queue_pending`. Observed-busy runs hold idle exclusion without consuming request slots. Every busy sample refreshes the idle clock, and busy-to-idle resets it. Failed, timed-out or malformed queue samples retain the previous state until they reach the health check's retry count (`runtime.health_check.retries`, or the monitor default). Reaching that threshold releases the busy hold and resets the idle clock. A successful sample resets that failure count. This bounds stale busy evidence even while `/system_stats` remains healthy. Stop cancels and drains sampling.

Observed-busy runs are protected. A direct submission between an idle sample and the next sample can race eviction. Ensure refreshes activity and rejects an already claimed idle stop, but it is not a lease over a future direct job. Run reports expose `engine_busy` and `memory_commitments_mib`.

## Access

ComfyUI has no authentication and custom nodes execute as the host account. Anyone who can reach its port can use the GPU and that account. Use `listen: "127.0.0.1"` for single-host access, or restrict network access at the host boundary. A zzRouter admin key controls the run lifecycle; it does not authenticate direct ComfyUI requests or meter them.
