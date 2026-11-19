# Model calibration specification

Draft for the first implementation, including the refactors it needs. Nothing
is live, so this plan changes existing code where that gives the cleaner
design and carries no compatibility paths.

A calibration measures one to three running models on one node under a
synthetic workload, at concurrency steps the operator lists, and reports what
each model and each resource did, per step: latency, throughput, KV cache use,
and the use of every memory pool involved (GPU memory, a unified pool, host
RAM), plus GPU compute, power and CPU.

Its purpose is resource utilization. A knowledgeable operator sets every engine
parameter by hand, measures, changes one setting and measures again, until the
hardware is well used and latency stays acceptable, or until two or three models
share it and still perform. Resources are not only GPUs: llama.cpp offloads
layers and experts to host RAM, Strata pins host RAM, and MLX lives entirely in
the unified pool. The report treats every pool the same way.

zzRouter measures and reports. It never chooses a setting, never searches
settings on its own, and never decides what latency is acceptable ("Inform the
agent; don't decide for it"). A calibration starts, stops and reconfigures
nothing. Configuring and running models stay with the parameter and run APIs,
so every provider, present or future, is covered by one contract.

## The tuning loop

1. **Configure.** Set any engine parameter at any tier with the parameter
   editor (`PATCH /providers/{name}/parameters`, `zz_params_set`), or only for
   one run at start (`POST /runs` with `parameters`). The provider schema lists
   every flag, and an operator can declare a flag the release does not ship
   yet. `POST /runs/preview` (`zz_run_preview`) gives the memory assessment
   before launch.
2. **Run.** Start the model, or move it to new settings: a parameter PATCH with
   `?restart=affected` restarts every run the change made stale, and
   `POST /runs` with `replace: true` replaces a run with per-run settings
   ([refactor 1](#1-one-run-start)).
3. **Calibrate.** Measure the running models together (this specification).
4. **Compare.** Put two reports side by side, including the settings each run
   was launched with, then go back to step 1.

Settings tested through the parameter editor are already saved. Settings
tested per run are saved from the report in one explicit action
([Console](#console)).

## Refactors

Each refactor removes a duplicate or a gap the calibration would otherwise
work around. They land before or with the calibration, in this order.

### 1. One run start

`POST /runs` and `POST /runs/load` both start a model, with different
replacement rules: `/runs` silently reuses a running model and ignores new
settings, while `/runs/load` has `force`. The console and MCP use `/runs`.
The terminal client uses `/runs/load` through
`client.LoadModelWithProviderForceParamsAndEnv` (`apipath.RunsLoad`), from
`zzrouter run` (`internal/cli/clientcli/run.go`) and the TUI's model start
(`tui_models_actions.go`). The routes also disagree on a field name: `/runs`,
its internal route and `zz_run_start` say `env_vars`, while `/runs/load`,
preview and the parameter tiers say `environment`.

- Delete `POST /runs/load` (`createRunsLoad`), its internal route,
  `LoadService.LoadModel`, `LoadExecutor.LoadLocalModel`,
  `HandleInternalLoadModel`, `LoadModelRequest`, `apipath.RunsLoad` and the
  client's `LoadModelWithProviderForceParamsAndEnv`. Move run preview from
  `load_service.go` and `load_executor.go` into the runs service and executor
  beside start.
- `POST /runs` gains `replace` (boolean), mapped to `EnsureRequest.Force`. It
  stops the run on the key and starts one with the request's settings, through
  the existing admit-before-stop path, so a refused replacement leaves the old
  run serving.
- One field name: start, its internal route, preview and `zz_run_start` take
  `environment`, as the parameter tiers do.
- A start whose `parameters` or `environment` differ from the running run's
  request tier, without `replace`, answers 409 `run_settings_differ`, naming
  the run and `replace` as the next step. A start that states no settings, or
  the same ones, reuses the run as today. This removes the silent reuse.
- `zz_run_start` gains `replace` and documents the refusal.
- **Terminal client:** `zzrouter run` and the TUI start through `POST /runs`.
  `zzrouter run --force` currently means both "pick the first match" and
  "replace the running run". It splits: `--force` keeps match selection, and
  `--replace` sends `replace`.

### 2. One resource snapshot

Four code paths observe the same hardware:
- `gpu.NVIDIAMemorySnapshotContext`: memory and per-process memory, for
  admission.
- `gpu.liveCardMetricsContext`: memory and utilization, for node metrics.
- `inventory.GatherMemory`: host RAM, for node metrics.
- `ProviderAppManager.memorySystem`: total RAM, for admission.

Replace them with one snapshot in `pkg/discovery`, made of resource pools:

| Pool | Fields | Per-process attribution |
|---|---|---|
| `gpu` (one per device) | total, used (driver), free, utilization, power, vendor, UUID | Driver per-process memory, through `runOwning` |
| `unified` (Apple Silicon) | total, available, swap used, pressure level (`kern.memorystatus_vm_pressure_level`: 1 normal, 2 warning, 4 critical, readable without root) | none |
| `host` (absent when the pool is unified) | total, available, swap used, CPU utilization | Resident memory and CPU time of each run's process tree |

- NVIDIA becomes one `--query-gpu` (adding `power.draw`) plus one
  `--query-compute-apps` per snapshot. Each platform is a sampler behind the
  same pool type. AMD (`rocm-smi`, already used for inventory) becomes a
  sampler later without changing any consumer.
- Admission, node metrics, the console's memory cards and calibration all read
  this snapshot. Admission keeps its current inputs and rules; only their
  source is shared. Freshness stays with the caller: admission asks for a fresh
  snapshot, node metrics keep their cache.
- A run's host memory is the resident set of its process group, which the node
  already owns. Weights mapped from files count once touched, so this can
  exceed what the run strictly needs; the report says so. A served daemon's
  tree is shared by its models (see [Providers](#providers)).
- The console rule stays: used memory is never computed as total minus
  available, and a unified pool appears once.

### 3. One engine observation declaration

`runtime.activity_probe` (ComfyUI's `/queue`, in `config.yaml`) reads an engine
endpoint to decide whether a run is busy. Calibration needs the same kind of
read for scheduler and KV figures. Instead of adding a second declaration and
poller, replace `activity_probe` with one `observe` block in the provider's
`schema.yaml`, beside `cache_log`:

```yaml
observe:
  path: /metrics
  format: prometheus             # or json
  running: <series or field>     # requests the engine is running
  waiting: <series or field>     # requests queued in the engine
  kv_cache_usage: <series>       # fraction of the KV pool in use
  busy: [<field>, ...]           # json: nonempty fields hold the run busy
```

- Every role is optional. `busy` keeps today's meaning exactly: the eviction
  hold and the activity observation. ComfyUI moves to
  `observe: {path: /queue, format: json, busy: [queue_running, queue_pending]}`.
- One reader function in `pkg/prov_apps/health` parses a run's observation.
  The health monitor calls it on its interval, and a calibration calls it on
  its own sampling interval. There is one reader with two schedules, not two
  pollers.
- Validation follows `activity_probe`: a local path, known roles, and a format
  from a closed set.
- **Prometheus:** a role names a series. Engines label their series (vLLM adds
  `engine` and `model_name`), so the reader combines every label set of a
  series: counts (`running`, `waiting`) are summed, and fractions
  (`kv_cache_usage`) take the maximum.
- **JSON:** a role is a dotted path to a number (mlx-vlm:
  `summary.in_flight`).
- llama.cpp serves `/metrics` only with `--metrics` (501 otherwise), so its
  shipped config sets `metrics: true`. An operator who turns it off gets null
  engine figures, and the calibration form says why.
- Requests go through the engine request path the activity probe uses today,
  so a credential rule never has to be written twice.
- Series and field names come from each pinned engine's source, confirmed on
  the wire, never from `--help` or memory ([Findings](#findings)).

### 4. One instance request path, measured without publishing

A public request reaches a run through `openRun` (lease and target),
`instanceRequest` (body with request defaults) and `sendToInstance`. Its stream
is read by `wire.streamMeter` into an `llm.InferenceRecorder`, which also
publishes the inference log entry, spend and GenAI metrics. Calibration needs
the same path and the same stream reading, without publishing.

- `openRun`, `instanceRequest` and `sendToInstance` take a typed instance
  request (context, path, original body, client model, keep-alive hints)
  instead of reading gin context values. Public dispatch builds it from the
  gin request; calibration builds it directly.
- `wire.streamMeter` reports to a small observer interface (first token,
  usage, end of stream). `llm.InferenceRecorder` implements it for
  production, and a calibration request sample implements it to record
  timings and token counts. There is one SSE parser.
- Calibration requests are never published as inference: no inference log, no
  spend, no GenAI metrics, no client activity. They still take a lease, so the
  run is busy and active exactly as under public traffic.

### 5. A count of admitted requests

`Instance` gains a monotonic count of leases granted, exposed in the run
snapshot as `requests_admitted` beside `active_requests`. A calibration
subtracts its own leases from a step's increase to report public traffic.
`active_requests` is a gauge and would miss short requests between samples.

### 6. An absolute KV budget for vLLM

vLLM's `kv-cache-memory-bytes` sets the KV cache size directly and ignores
`gpu-memory-utilization`. It is the usual way to fit two vLLM instances on one
GPU. zzRouter's vLLM budget is declared only on `gpu-memory-utilization`
(`fraction_total`), so admission miscounts a run that sets it.

- `schema.MemoryBudget` gains `kv_bytes_parameter`. vLLM declares
  `kv-cache-memory-bytes`.
- vLLM 0.29 still refuses to start unless `ceil(total × gpu-memory-utilization)`
  is free, even with an absolute KV size (`request_memory` in
  `vllm/v1/worker/utils.py`, called unconditionally at worker init). Its KV
  cache then takes exactly the given bytes, and memory profiling is skipped.
- So when a launch sets the parameter, `AdmitMemoryBudget` checks free memory
  against both requirements and refuses when either is short:
  - the startup requirement, `fraction × total`;
  - the hold: known weights plus the KV bytes plus the declared safety margin
    (the one `auto` already uses).
- It records the hold, not the fraction, as the run's commitment. Missing
  weights evidence leaves the assessment unknown, as today.
- To fit a second vLLM run beside a first, the operator lowers its
  `gpu-memory-utilization` until the startup requirement fits the remaining
  free memory, and sizes its cache with `kv-cache-memory-bytes`. A refusal
  names which requirement failed.
- The release bound, holder reporting and reclaim read the same commitment.
- Measured on the lab, Qwen2.5-0.5B ran beside a 27B run with
  `gpu-memory-utilization` 0.2 and a 2 GiB KV cache:
  - It held 6,916 MiB: 0.93 GiB of weights, exactly 2 GiB of KV (174,752
    tokens), and about 3.9 GB of graphs and activations.
  - Today's admission committed 19,577 MiB for it (0.2 × total).
  - The `auto` margin, max(2,048 MiB, 5% of total) = 4,894 MiB, covered the
    overhead.

### 7. An expert override for memory admission

An operator tuning to the edge may know better than an estimate; LM Studio
lets its users load past its estimate. Add a reserved run setting beside
`keep-alive` and `evict-idle-on-memory-pressure`:

- `memory-admission: enforce` (the default) or `report`. Under `report`, a
  memory refusal becomes `admission_warning` on the start result and on the
  run, with the refusal's full evidence, and the launch proceeds. The run's
  commitment is still recorded, so later launches see it.
- One branch in `admitMemory`; `runsettings` owns the key. The PATCH, run
  start and preview validate it like the other reserved settings.

### 8. Cluster protocol

The internal calibration route, the run snapshot fields and the provider
`observe` block ride one cluster protocol bump. Current and minimum move
together, as in recent bumps, so there is no older-worker code path. The
number lives only in `pkg/version/protocol.go`.
[Protocol versioning](protocol_versioning.md)

### What the refactors remove

Roughly 300 production lines leave with the second start route and the
duplicate GPU and RAM query paths. The calibration adds:
- `pkg/calibration`: pure validation of shape, scheduling, aggregation,
  outcomes and report types.
- Its HTTP and job wiring in `internal/server`.
- The `observe` reader.

The implementation reports the net change against its base.

## API

### Endpoints

Paths are relative to `/zzrouter/v1`. Use the existing administrator gate,
browser CSRF controls, success envelopes and Problem Details.

| Operation | Contract |
|---|---|
| `POST /calibrations` | Validates and accepts a calibration on `node`. HTTP 202 through `respondAccepted` with `data.job_id` and `data.node`; `Location` is `/zzrouter/v1/jobs/{job_id}?node={node}`. Supports `Idempotency-Key`. |
| `GET /jobs/{id}?node={node}` | The job snapshot. While it runs, `meta` carries progress; the terminal event carries the report in `meta.report`. |
| `GET /jobs/{id}/stream?node={node}` | The existing SSE stream and replay. |
| `GET /jobs?node={node}&kind=calibration` | Calibrations on that node still inside retention, through the existing paginated job list. |
| `DELETE /jobs/{id}?node={node}` | Cancels: in-flight requests are aborted, and the report keeps the steps already measured. |
| `GET /providers/{name}/schema` | Gains `calibration` (output-forcing request fields) and `observe` (declared roles), so a client can say what a report will contain. |
| `GET /zzrouter/v1` (discovery) | Lists the calibration limits and workload kinds under `data.calibration`, so clients never hardcode them. |

The job is the record: a new job kind, `calibration`, with ID prefix `cal_`,
registered in `pkg/jobs/kind.go`, with an entry in `jobOutcomeTools`. The
report reaches clients through job `Meta` set before the terminal event, as a
restart job's `restarted` map does. The coordinator forwards
`POST /calibrations` to the named node over cluster mTLS through the new
internal route; job reads and cancellation use the existing job proxy.
[Cluster networking](cluster_networking.md)

### MCP

`zz_calibrate` on the admin surface takes the `POST /calibrations` body and
returns the job, dispatching into the gin engine like every `zz_*` tool. Its
description says how to get `run_id`s (`zz_runs`), change settings
(`zz_params_set` with `restart_affected`, or `zz_run_start` with `replace`)
and read the result (`zz_job`). The report is bounded by the limits, so
`zz_job` returns it whole. Every reason names the `zz_*` tool to call next.
[MCP](mcp.md)

### Create a calibration

The example measures two models sharing one GPU. Step 0 runs the first model
alone, step 1 the second alone, and step 2 both together.

```json
{
  "node": "gpu-1",
  "step_timeout_seconds": 600,
  "runs": [
    {"run_id": "run_qwen", "workload": {"input_tokens": 30000, "output_tokens": 256, "concurrency": [4, 0, 4]}},
    {"run_id": "run_coder", "workload": {"input_tokens": 4000, "output_tokens": 512, "concurrency": [0, 8, 8]}}
  ]
}
```

- `node` (required): the node holding the runs.
- `runs` (one to three): each entry names a live `run_id` on that node, with an
  optional `workload`. An entry without a workload is resident: it holds
  resources and is observed, nothing more. A ComfyUI run or a pinned
  production model can be listed this way.
- `workload.input_tokens` (at least 1): prompt tokens per request, as the
  engine counts them after template expansion.
- `workload.output_tokens` (at least 1, chat only): generated tokens per
  request.
- `workload.concurrency`: requests in flight at each step, integers of zero or
  more. Zero means idle in that step. Every workload lists the same number of
  steps, and each step has at least one non-zero value.
- `workload.requests_per_step` (optional): requests completed in each step
  where the run is active. The default is `max(3 × concurrency, 10)`, enough
  for stable percentiles. A value below that step's concurrency is refused.
- `step_timeout_seconds` (default 600): bounds each step's wall time.

The workload kind follows the run's endpoint: `chat` and `embeddings` in the
first release. Each kind has a versioned generator (`chat-v1`,
`embeddings-v1`) in `pkg/calibration`, and a new kind (reranking, images) is a
new generator, never a new request shape. Limits are named constants
published in discovery: 3 runs, 8 steps, a request count per step, and a
maximum step timeout.

Submission validation is synchronous, because every run is already up:

| Status | Code | When |
|---|---|---|
| 400 | field codes | Invalid fields, steps or token values. |
| 400 | `context_too_small` | A chat run's served context (`context_length`) cannot hold `input_tokens + output_tokens`. |
| 400 | `concurrency_above_limit` | A step's concurrency exceeds the run's `max_concurrent_requests` (zero means unlimited), so zzRouter itself would queue it. The problem names the limit and its setting. |
| 404 | `run_not_found` | A `run_id` that is not on that node. |
| 409 | `run_not_ready` | A run that is not ready to serve. |
| 409 | `calibration_unsupported` | A workload on a run whose endpoint has no workload kind, or whose wire endpoints lack the kind's API. |
| 409 | `calibration_running` | A calibration is already running on that node; the problem names its job. |
| 503 | | The named worker is unreachable. |

One calibration runs per node at a time, so two calibrations never contaminate
each other's measurements.

## Execution

### Requests

Every calibration request goes through the shared instance request path
([refactor 4](#4-one-instance-request-path-measured-without-publishing)):
- It takes the run's ordinary lease, so idle expiry and memory reclaim see the
  run as busy.
- Its body comes from `instanceRequest`, so request defaults apply as in
  production.
- It goes straight to the run, never through public routing or access control.

Chat requests stream, with usage requested through `wire.ForceUsageReporting`
and read by the stream meter. Every engine checked sends usage only when
`stream_options.include_usage` is true, so this is required, not
defensive. The meter already accepts a usage chunk whose `object` is
`chat.completion`, as mlx-lm sends it. When the provider declares
`calibration.output`, chat requests carry those fields with
`max_tokens = output_tokens`, so the engine generates the full length.
Requests never set `seed`, because mlx-lm stops batching a request that
carries one. Embeddings requests send one input each.

Public traffic is not blocked. A step's `public_requests` is the increase in
`requests_admitted` over the step minus the calibration's own leases. Public
requests can hold request slots, so each calibration request records how long
it waited for its lease (`slot_wait_ms`). Its engine timings start once the
lease is granted, which keeps zzRouter's queueing out of TTFT.

Calibration requests keep production's first-byte and idle deadlines. A
request that a deadline cuts is a real finding, so it fails with the
deadline's existing code rather than being retried or given more time.

When an engine reports no usage for a request, its token counts are null and
the lengths cannot be verified. That run's step outcome is `inconclusive`,
with `usage_unavailable`.

### Sequence

1. **Check.** Record each run's identity, resolved parameters,
   `parameters_status`, served context and engine cache report, and the pools
   it holds memory in.
2. **Warm up.** Send one short request to each run with a workload. It is not
   measured. Its reported prompt tokens give the tokens-per-character ratio
   that sizes that run's prompts, with no engine-specific tokenizer.
3. **Run the steps.** In step k, every run whose `concurrency[k]` is non-zero
   runs at once. Each keeps `concurrency[k]` requests in flight until its
   requests for the step complete or the step times out. Every request has a
   distinct prompt prefix, so prefix caching cannot flatter the result.
4. **Publish.** Write the report into job `Meta`, then end the job.

Progress `meta` carries `step`, `steps`, `requests_completed`,
`requests_total` and the latest resource sample. Job steps are `checking`,
`warming_up` and `measuring`.

### Sampling

During checking and measuring, the node takes a resource snapshot
([refactor 2](#2-one-resource-snapshot)) and each run's engine observation
([refactor 3](#3-one-engine-observation-declaration)) at a fixed interval,
initially 500 ms. The report includes `sampling_interval_ms` and
`largest_sample_gap_ms`. A slow snapshot delays the next one rather than
overlapping it. On the RTX PRO 6000 one NVIDIA snapshot takes about 145 ms
(two nvidia-smi calls of about 72 ms each), idle or with a model loaded, so
500 ms leaves headroom.

## Report

Values below are illustrative, not measurements. Step 2 is shown; steps 0 and
1 have the same shape. On Apple Silicon, `resources` holds one `unified` pool
in place of `gpu` and `host`.

```json
{
  "outcome": "verified",
  "reason": null,
  "started_at": "2026-11-19T15:00:00Z",
  "completed_at": "2026-11-19T15:06:40Z",
  "request": {"node": "gpu-1", "step_timeout_seconds": 600, "runs": ["...echoed as submitted..."]},
  "sampling_interval_ms": 500,
  "largest_sample_gap_ms": 640,
  "resources": [
    {"id": "GPU-example", "kind": "gpu", "vendor": "nvidia", "total_mib": 97887,
     "ownership": "verified",
     "holders": [{"pid": 4101, "memory_mib": 63600, "run_id": "run_qwen"}, {"pid": 4388, "memory_mib": 24400, "run_id": "run_coder"}]},
    {"id": "host", "kind": "host", "total_mib": 772044}
  ],
  "runs": [
    {
      "run_id": "run_qwen", "provider": "vllm", "model": "Qwen/Qwen3.8-27B", "endpoint": "chat",
      "resolved_parameters": {"gpu-memory-utilization": "0.65", "max-model-len": "32768", "max-num-seqs": "4"},
      "parameters_status": "current",
      "served_context_tokens": 32768,
      "max_concurrent_requests": 8,
      "cache": {"kv_tokens": 131072, "context_tokens": 32768, "max_concurrency": 4.0},
      "output_forced": true, "observed_roles": ["running", "waiting", "kv_cache_usage"],
      "workload_revision": "chat-v1"
    },
    {"run_id": "run_coder", "...": "same shape"}
  ],
  "steps": [
    {
      "index": 2, "outcome": "verified", "duration_ms": 118000,
      "throughput": {"output_tokens_per_second": 610.0},
      "resources": [
        {"id": "GPU-example", "peak_used_mib": 89120, "minimum_free_mib": 8127,
         "utilization_pct": {"mean": 93, "peak": 100}, "power_w": {"mean": 488, "peak": 571},
         "other_holders_peak_mib": 0},
        {"id": "host", "minimum_available_mib": 701200, "peak_swap_used_mib": 0,
         "cpu_pct": {"mean": 4, "peak": 9}}
      ],
      "runs": [
        {
          "run_id": "run_qwen", "outcome": "verified", "concurrency": 4,
          "requests": {"requested": 12, "completed": 12, "failed": 0},
          "public_requests": 0,
          "input_tokens": {"min": 30012, "max": 30051},
          "output_tokens": {"min": 256, "max": 256},
          "concurrent_streams_peak": 4,
          "slot_wait_ms": {"p50": 0, "p90": 0, "p99": 0, "min": 0, "max": 0, "count": 12},
          "time_to_first_token_ms": {"p50": 9100, "p90": 19800, "p99": 22100, "min": 2400, "max": 22100, "count": 12},
          "inter_token_latency_ms": {"p50": 38, "p90": 45, "p99": 51, "min": 33, "max": 51, "count": 12},
          "throughput": {"input_tokens_per_second": 3050.0, "output_tokens_per_second": 26.0, "requests_per_second": 0.10},
          "memory_peak_mib": {"GPU-example": 63610, "host": 5120},
          "cpu_pct": {"mean": 101, "peak": 118},
          "engine": {"running_peak": 4, "waiting_peak": 3, "kv_cache_usage_peak": 0.92}
        },
        {"run_id": "run_coder", "...": "same shape"}
      ]
    }
  ]
}
```

Definitions:

- **Latency.** `slot_wait_ms` is the time a request waited for its lease.
  Time to first token runs from the lease being granted to the first content
  token. Inter-token latency is `(latency − TTFT) / (output_tokens − 1)`
  per request, and a request's per-user rate is `1000 / ITL` tokens per second.
  Percentiles are nearest-rank over the step's completed requests, with
  `count` alongside.
- **Throughput.** A run's throughput divides its step totals (input tokens,
  output tokens, completed requests) by the time from its first request sent to
  its last completion. The step's `throughput.output_tokens_per_second`
  divides all runs' output tokens by the step's wall time: the hardware's total
  for that step.
- **Concurrency.** `concurrent_streams_peak` is the most calibration requests
  to one run emitting output at the same instant. zzRouter measures it for
  every engine that streams.
- **Memory.** `memory_peak_mib` is the run's attributed memory per pool: driver
  per-process memory for a GPU, the process tree's resident set for host RAM,
  and null for a unified pool, which has no per-process attribution.
- **CPU.** `cpu_pct` counts 100 as one core: the run's process tree for a run,
  all cores for the host. It shows offloaded layers or experts working on the
  CPU.
- **Ownership** applies to GPU pools:
  - `verified`: every process holding memory belongs to a listed run.
  - `shared`: another holder is present, listed with its PID, memory, and run
    if it is zzRouter's.
  - `unknown`: there is no per-process attribution, or the driver lists
    holders without sizes. Windows' WDDM driver does this: it reports
    `[N/A]` for every process, including the desktop compositor that is
    always present.

  `other_holders_peak_mib` is the GPU memory held outside the listed runs
  during a step. Memory held by no process (the driver's reservation, about
  25 MiB per process) is expected, and pool figures are never required to add
  up.
- **Settings status.** `parameters_status` says whether the tested settings are
  saved configuration (`current`), per-run overrides (`overridden`), or behind
  the configuration (`stale`).
- **Format.** Units are milliseconds, MiB, watts, percent, and a 0 to 1
  fraction for KV usage. Unknown values are null, measured zero is zero, and an
  empty list is empty. A run idle in a step is absent from that step's `runs`.
  The report keeps summaries only, never prompt text, response text,
  credentials, environment values or process arguments.

### Outcomes

| Run outcome in a step | Criteria |
|---|---|
| `verified` | Every request completed, with measured input at or above `input_tokens`; output equal to `output_tokens` when output is forced; and, for chat, `concurrent_streams_peak` reaching the step's concurrency. |
| `queued` | Every request completed at the requested lengths, but fewer ran at once than requested, for example a step above the engine's sequence limit. Throughput and latency are still valid. |
| `failed` | A request errored, or a length was not reached. |
| `inconclusive` | The step timed out, or the run stopped, restarted or was replaced during it. |

Embeddings have no output stream, so an embeddings run is verified when every
request succeeded at the requested input length. Public traffic does not
change an outcome: it is reported, and the operator judges it. A step is
verified when every run active in it is verified.

| `outcome` | Criteria |
|---|---|
| `verified` | Every step is verified. |
| `partial` | At least one step is verified, but not all. |
| `failed` | No step is verified, and requests failed. |
| `inconclusive` | No step is verified, and a step was cut short. |
| `cancelled` | A cancellation was requested. |

Every outcome except `verified` carries a `reason`: a `code`, a `message`, and
a `next_action` with its own `code` and the `tool` to call.

| `reason.code` | `next_action.code` | `tool` |
|---|---|---|
| `target_not_met` | `change_settings` | `zz_params_set` |
| `requests_failed` | `inspect_run` | `zz_logs` |
| `run_changed` | `check_run` | `zz_runs` |
| `usage_unavailable` | `check_provider` | `zz_providers` |
| `step_timeout` | `change_workload` | `zz_calibrate` |
| `cancelled` | none | none |

The job ends `done` whenever a report is published. Cancellation ends it
`failed` with the existing cancellation error, after the report records the
steps already measured.

## Providers

The contract is the same for every provider, and a new provider brings
declarations, never a field, a code path or an engine name in Go.

**Measured by zzRouter for every provider:** request timing, TTFT, ITL,
throughput, `concurrent_streams_peak`, public traffic, and token counts from
the engine's usage report. Every pool's figures come from the node's samplers,
not the engine.

**Declared by the provider in `schema.yaml`:**
- `observe` ([refactor 3](#3-one-engine-observation-declaration)): engine
  scheduler and KV figures.
- `calibration.output`: request fields that force full-length output.
- `cache_log`: the engine's startup KV report.
- The memory budget: how admission sizes a launch.

Without `calibration.output`, output length is reported but not required
(`output_forced: false`). An undeclared `observe` role reads null.

| Provider | Workloads | Settings an operator typically tunes (all in its schema) | Pools it uses |
|---|---|---|---|
| vLLM | chat, embeddings | `gpu-memory-utilization`, `kv-cache-memory-bytes`, `max-num-seqs`, `max-num-batched-tokens`, `max-model-len`, `kv-cache-dtype` | GPU; host for swap and offload |
| llama.cpp | chat, embeddings | `ctx-size`, `parallel`, `n-gpu-layers`, `n-cpu-moe`, `fit`, `fit-target`, `batch-size`, `ubatch-size`, `cache-type-k`, `cache-type-v`, `flash-attn` | GPU and host (layers and experts); unified on Apple Silicon |
| MLX | chat (mlx-lm); chat and embeddings (mlx-vlm runtime) | `max-num-seqs`, `decode-concurrency`, `prompt-concurrency`, `max-kv-size`, `kv-bits`, `prefill-step-size` | Unified |
| Strata | chat | `max-context`, `kv`, `kv-resident`, `expert-cache`, `prefill` | GPU and pinned host RAM |
| Ollama | chat, embeddings | model options and daemon settings in its schema | GPU and host, attributed through a daemon zzRouter manages |
| ComfyUI | resident only | `reserve_mib` | GPU; host |
| Cloud providers | not eligible | none | none |

The declarations each shipped provider gets, from the pinned source and, where
marked, confirmed on the wire on 2026-11-19:

| Provider (pinned) | `observe` | `calibration.output` | Streamed usage | Notes |
|---|---|---|---|---|
| vLLM 0.29.0 (wire) | prometheus `/metrics`: `running` = `vllm:num_requests_running`, `waiting` = `vllm:num_requests_waiting`, `kv_cache_usage` = `vllm:kv_cache_usage_perc` (0 to 1) | `{ignore_eos: true}` (48 of 48 tokens on the wire) | final chunk with `include_usage` | Metrics are on by default. `disable-log-stats: true` leaves the engine series unpopulated, so the form warns and the report reads null. |
| llama.cpp b10549 (source) | prometheus `/metrics`: `running` = `llamacpp:requests_processing`, `waiting` = `llamacpp:requests_deferred`; no KV usage series exists | `{ignore_eos: true}`; `max_tokens` maps to `n_predict` | final chunk with `include_usage`, plus `timings` | `/metrics` answers 501 without `metrics: true`; the shipped config sets it. |
| mlx-lm 0.31.3 (wire) | none: no metrics route | none: output reported, not forced (10 of 48 tokens on the wire) | final chunk with `include_usage`, `object` `chat.completion` | Batches only requests without `seed`; `decode-concurrency` 32 and `prompt-concurrency` 8 by default. |
| mlx-vlm 0.7.6 (source) | json `/metrics`: `running` = `summary.in_flight`, `waiting` = `server.request_queue_depth`; no KV usage | none | final chunk with `include_usage` | Embeddings return usage. |
| Strata 0.1.44.1 (source) | prometheus `/metrics/prometheus`: `running` = `vllm:num_requests_running`, `waiting` = `vllm:num_requests_waiting`. Its `vllm:kv_cache_usage_perc` is the running request's share of its context, not KV pool use, so `kv_cache_usage` is not declared. | none: output reported, not forced | always on the final chunk, not gated by `include_usage`, plus `timings` | Serves one request at a time unless the engine gets `--batch N`; see below. No embeddings. `/health` takes no lock, so it answers during a long prefill. |
| Ollama 0.40.16 (wire) | none: no metrics route; `/api/ps` gives context and residency | none: output reported, not forced (23 of 48 tokens on the wire) | final chunk with `include_usage`, plus `timings` | Embeddings return usage. |
| ComfyUI | json `/queue`: `busy` = `queue_running`, `queue_pending` | not applicable | not applicable | Resident entries only. |

**Strata concurrency.** Strata's server runs one request at a time behind a
FIFO lock unless its engine was started with `--batch N` (alias `--slots`).
Batched requests skip that lock. The engine may grant fewer slots than asked;
it grants at most 8 unless `--batch-mtp` is set. Its server config key
`parallel` only turns into `--batch` and does nothing when `--batch` is already
among the engine arguments (`parallel_args` in `serve/server.py`). zzRouter's
launcher passes parameters through as engine arguments, so the shipped Strata
`schema.yaml` declares `batch` (an int) and `batch-mtp`. The launcher needs no
change, and an operator can declare them through the operator schema overlay
before that release. Without `batch`, every Strata step above concurrency 1
reports `queued`, which is the honest result.

A served daemon (Ollama) shares its process tree across its models. Its GPU
and host memory count for a listed run only while the daemon's loaded models
are exactly the listed served runs on it, as `/api/ps` reports. Otherwise
they are reported as the daemon's, and the GPU pool is `shared`. zzRouter
adopts the models an Ollama daemon has loaded as runs, so they have `run_id`s
either way. An Ollama daemon zzRouter does not manage has no ancestry to
follow, so its memory appears as another holder.

**Future providers and hardware:**
- A new engine declares `observe`, `calibration.output`, `cache_log` and a
  memory budget kind.
- A new endpoint type (reranking, images, audio) adds a workload generator in
  `pkg/calibration`.
- A new accelerator adds a resource sampler in `pkg/discovery`.

None of these changes the request, the report shape or the console.

## Console

The console uses the shared right-side panel, its Edit/Save footer, the job
polling owner and the existing DOM helpers. It adds one module,
`calibrations.mjs`, and no dependency.

### Entry points

- **Run details** (Workloads, Runs) gain **Calibrate** for a ready run whose
  endpoint has a workload kind. It opens the form with that run selected.
- **Workloads** gains a **Calibrations** tab: the job list filtered by
  `kind=calibration`, with the same per-node reads, pagination and offline
  handling as Jobs, plus **New calibration**.
- **Download and test:** after a download completes, the panel offers **Start
  and calibrate**. It runs the existing start (preview, explicit start,
  readiness read), then opens the form with the new run selected.

### Form

1. **Runs.** Choose the node, then up to three ready runs on it. Each row shows
   model, provider, endpoint, the pools it holds memory in, pinned state,
   active requests and its `parameters_status`. **Settings** expands its
   resolved parameters read-only, and **Change settings** offers both paths of
   the loop (below).
2. **Workload.** Each run gets input and output tokens. A preset menu fills the
   numbers (short chat, long context sized from the run's served context,
   retrieval, embeddings), and the operator can edit them. The form checks
   tokens against the served context as the operator types.
3. **Steps.** A grid with one row per step and one column per run; each cell is
   the run's concurrency. Presets fill it, and every cell stays editable:
   - **Ramp:** 1, 2, 4… up to twice the engine's reported sequence slots or
     concurrency, or up to 16 when the engine reports neither.
   - **Each alone, then together:** for two or three runs.

   A requests column shows the default count per step.
4. **Review.** Total requests, plus notices from the provider schema and the
   run: public traffic on a listed run, `observe` roles or output forcing not
   declared, a unified pool without per-process attribution, and host memory
   in use by a run (offload).
5. **Start.** One idempotency key per submission, as for run start; the panel
   then follows the job. API validation errors render with the shared Problem
   display, beside their controls.

### Changing settings

Both paths use the provider schema's field renderer, so every parameter is
editable by hand, with its description, type and bounds:

- **Save for this model on this node:** opens the parameter editor at "Model on
  a compute node" scope. Its "Effect on running models" section gains
  **Restart affected runs**, which sends the PATCH with `?restart=affected`.
- **Try on this run only:** opens the run form prefilled with the run's
  request tier. The form gains a **Settings for this run** section and sends
  `POST /runs` with `parameters` and `replace: true` after the existing
  preview. A `run_settings_differ` refusal explains itself.

### Live view

While the job runs, its details show:
- the step in progress and the requests completed per run;
- the latest resource sample: GPU memory against total, utilization, power,
  host available memory and CPU, or the unified pool.

These use the existing job polling, Pause and Refresh. **Cancel** uses the
existing job cancellation and confirmation.

### Report

A finished calibration renders its report in place of the generic job body:

- **Header:** the outcome, and the reason with one button for its next action.
  `zz_params_set` opens the parameter editor for that run; `zz_runs` opens run
  details; `zz_logs` opens runtime logs; `zz_calibrate` opens the form,
  prefilled.
- **Resources:** one card per pool. GPU cards show peak memory against total,
  minimum free (the headroom), utilization and power. The host card shows
  minimum available memory, peak swap and CPU. A unified card shows minimum
  available memory and swap. Ownership appears with any other holder named.
- **Steps table:** one row per step and run, showing concurrency, outcome
  badge, output tokens per second, per-user tokens per second, TTFT p50 and
  p99, ITL p50 and p99, KV cache peak, memory per pool, CPU and public
  requests. With several runs, each step also gets a total row with the
  combined throughput.
- **Throughput chart:** an inline SVG per run plotting per-user rate against
  total rate across its steps, the trade-off curve. The table stays
  authoritative and accessible.
- **Settings tested:** each run's resolved parameters and `parameters_status`.
  For a run with per-run overrides, **Save as model settings on this node**
  opens the parameter editor prefilled with those values and shows the diff
  before the PATCH.
- **Actions:** **Run again** (prefilled with the same request), and **Compare
  with…**, which picks another calibration on the same node still in
  retention and shows both step tables side by side, with each run's parameter
  differences highlighted.

### Changes beside the new module

- **Parameter editor:** **Restart affected runs**.
- **Run form:** **Settings for this run** and **Replace the running run**.
- **Run details:** **Calibrate**, and **Change settings** with both paths.
- **Job details:** hands calibration jobs to the report view.
- **Workloads:** the Calibrations tab.

Tests follow the console operations requirements: synthetic API data, DOM
tests for the form grid, presets, validation display, both settings paths,
live view, report and compare, keyboard use, narrow widths, and no replacement
of a draft during refresh.

## Contracts and documentation

These change in the same work, and the generated files are regenerated, not
edited by hand:

- **`internal/server/openapi.yaml`.**
  - Add `createCalibration`.
  - Add `replace` and `run_settings_differ` to `createRun`.
  - Remove `createRunsLoad`.
  - Add the run snapshot fields `requests_admitted` and `admission_warning`.
  - Add the `calibration` and `observe` schema blocks.
  - Add the discovery limits.
- **Route catalog and MCP bindings.** The route catalog
  (`routes_catalog.go`) and the MCP tool bindings change too: `zz_calibrate`
  names `createCalibration`, so the operationId test covers it.
- **Shipped provider files.**
  - `observe` and `calibration.output` blocks per the declarations table.
  - vLLM's memory budget gains `kv_bytes_parameter: kv-cache-memory-bytes`.
  - llama.cpp's config sets `metrics: true`.
  - Strata's schema declares `batch` and `batch-mtp`.
  - ComfyUI's `activity_probe` moves to `observe`.
- **Closed error codes.** Register the new codes where the closed codes live
  and list them in `docs/agent_error_codes.md`: `run_settings_differ`,
  `context_too_small`, `concurrency_above_limit`, `run_not_found`,
  `run_not_ready`, `calibration_unsupported` and `calibration_running`. List
  the report's reason codes too.
- **Documentation.**
  - `docs/mcp.md`: `zz_calibrate`, and `replace` on `zz_run_start`.
  - `docs/api.md`.
  - `docs/web-console.md` and `docs/console-operations-spec.md`: Calibrations,
    both settings paths, and Restart affected runs.
  - `docs/comfyui.md`: `observe` replaces `activity_probe`.
  - `docs/protocol_versioning.md`: the bump.
  - `CHANGELOG.md`.
- **Project guide.** In `CLAUDE.md`, update the ComfyUI and run-start
  passages, and add a short calibration entry under Architecture pointing
  here.
- **Generated coverage.** Regenerate `docs/api_coverage.md` and
  `docs/api_examples.md` from an e2e run that drives the new route.

## Retention and restart

The report lives in its job for the existing replay window: 24 hours after the
later of completion and the last hand-out of the job ID. A node restart ends a
running calibration and its job ID answers 404; the runs follow their own
lifecycle. Durable knowledge lives in saved parameters, not in reports.

## Decisions

1. **A calibration measures running models.** Configuration and lifecycle stay
   with the run and parameter APIs, so a calibration starts, stops and
   reconfigures nothing, and every provider is covered by one contract.
2. **Every memory pool counts.** GPU, unified and host memory share one
   resource snapshot and one report shape, so offload and unified-memory
   engines are measured as fully as dedicated GPUs.
3. **Explicit steps.** The operator lists every step, including steps above
   the engine's limits, to see where throughput stops rising. Nothing ramps on
   its own.
4. **Report, don't refuse.** Ownership, offload and public traffic are
   measured and shown; only invalid requests are refused.
5. **One path per concern.** One run start, one resource snapshot, one engine
   observation, one instance request path and one stream parser serve both
   production and calibration.
6. **The job is the record.** A second store with the job's lifetime would be a
   copy.
7. **No latency targets, no automatic search.** Percentiles are reported; the
   console or an agent applies its own targets and drives the loop.

Rejected in earlier drafts:
- **A calibration that launches its own runs.** It added a run origin, cleanup
  and reserved keys, and it duplicated the run and parameter APIs.
- **Semantic targets** (context, concurrency) translated into flags by
  per-provider adapters. That is engine code in Go, and it hides the settings
  an operator is tuning.

## Findings

Checked on 2026-11-19. Engine facts come from the pinned source tags, and the
marked ones were confirmed on the lab. Every run started for these checks was
stopped, leaving the lab as it was found.

| # | Question | Finding | Evidence |
|---|---|---|---|
| 1 | Engine scheduler and KV figures | **vLLM:** three Prometheus series, on by default; `disable-log-stats` stops them being populated. **llama.cpp:** processing and deferred counts behind `--metrics`, and no KV series. **mlx-vlm:** JSON in-flight and queue depth. **mlx-lm and Ollama:** no metrics route. | vLLM, MLX and Ollama on the wire; llama.cpp and mlx-vlm from source |
| 2 | Forcing full-length output | `ignore_eos` works on vLLM and llama.cpp. mlx-lm, mlx-vlm and Ollama have no such field and ignore it. | Wire: vLLM 48/48 tokens, MLX 10/48, Ollama 23/48 |
| 3 | Streamed usage | Every engine sends a final usage chunk only when `stream_options.include_usage` is true, so `usage_unavailable` applies to none of the shipped engines. Embeddings return usage on vLLM, llama.cpp, mlx-vlm and Ollama. | Wire: vLLM, MLX, Ollama |
| 4 | vLLM startup with `kv-cache-memory-bytes` | Free memory of `ceil(total × gpu-memory-utilization)` is still required: with 18,033 MiB free, a 0.25 fraction (24,472 MiB) failed at startup with `ValueError: Free memory on device…`, although the KV size was set to 1 GiB. With the startup check met, the KV cache takes exactly the given bytes, and the run holds weights plus KV plus overhead, far below the fraction. Refactor 6 checks both. | Source, then wire: two vLLM runs on the RTX PRO 6000, plus one direct engine start for the refusal |
| 5 | NVIDIA snapshot cost | About 145 ms (72 ms per nvidia-smi call), idle and loaded; 500 ms holds. | Wire on the RTX PRO 6000 |
| 6 | Windows per-process memory | WDDM reports `[N/A]` for every process and for `power.draw` on the RTX 4060; the desktop compositor is always listed. Ownership reads `unknown`, and power and per-run GPU memory read null. | Wire on the Windows worker |
| 7 | macOS memory pressure | `kern.memorystatus_vm_pressure_level` (1, 2 or 4) and `vm.swapusage` are readable without root. | Local check |
| 8 | Ollama attribution | Both lab daemons are children of `zzrouter-node`, so their memory is attributable; models they load are adopted as runs. | Process tree on both workers |
| 9 | Strata | **Usage:** always on the final chunk. **Output:** no forcing. **Metrics:** Prometheus at `/metrics/prometheus` for running and waiting, plus a context share that is not KV pool use. **Concurrency:** one request at a time unless the engine gets `--batch N`, which the schema will declare. **Embeddings:** none. | Source at v0.1.44.1 (`serve/server.py`, `serve/prometheus.py`) |

Strata's facts come from its v0.1.44.1 source (`serve/server.py`,
`serve/prometheus.py`). Strata is not installed on any lab node, so its wire
confirmation comes with its next acceptance. Nothing else is open.

## Out of scope

Automatic searches across settings, server-side latency targets, rate-based
(Poisson) load, recorded datasets, workload kinds beyond chat and embeddings,
routing that consumes calibration results, and a terminal client view. The
terminal client can still read reports through its existing job display.

## Acceptance

- **Tests:**
  - **`pkg/calibration` unit tests:** percentiles, ITL, the
    concurrent-streams peak, throughput windows, prompt sizing, step
    scheduling and the outcome tables.
  - **Mutation checks:** each test asserts that the break hit the rule under
    test.
  - **Stress:** new concurrency tests (scheduler, sampler, cancellation) pass
    `-race -count=20`.
  - **Integration and e2e:** integration tests cover the handler, job and
    worker hop. An e2e suite drives `POST /calibrations` to both a success and
    an error through `Node.HTTPClient()`, so the coverage ledger records it.
- **Refactors:**
  - `/runs/load` is gone; `POST /runs` with `replace` replaces a run, and
    differing settings without it answer `run_settings_differ`.
  - `zzrouter run` and the TUI start through `POST /runs`; `--replace`
    replaces and `--force` only picks a match.
  - Admission, node metrics and calibration read one snapshot, with the
    console's memory cards unchanged.
  - ComfyUI's busy hold works through `observe`.
  - Calibration requests leave no inference log, spend or metrics.
  - A vLLM run with `kv-cache-memory-bytes` is admitted on weights plus KV.
  - `memory-admission: report` launches past a refusal with the warning
    recorded.
- **vLLM on the RTX PRO 6000:** Qwen3.8 at 32,768 context, `max-num-seqs` 4
  and `gpu-memory-utilization` 0.8, steps 1, 2, 4 and 8. The report shows
  `verified` up to 4 and `queued` at 8, with percentiles, KV usage,
  utilization, power, host figures and headroom. The pilot observed four
  simultaneous requests at 30,039 input and 256 output tokens.
- **Two models:**
  - Two vLLM runs sized with `kv-cache-memory-bytes`, each alone and then
    together.
  - vLLM chat with a llama.cpp embeddings run.
  - Both beside a resident ComfyUI run.
- **Host memory:** a llama.cpp run with layers or experts on the CPU reports
  its host memory and CPU per step; a Strata run reports its pinned host RAM.
- **Unified memory:** llama.cpp and MLX on Apple Silicon report the unified
  pool once, with per-run memory null and ownership absent.
- **Ollama:** a served model, alone and beside a second loaded model (shared
  daemon).
- **Public traffic:** inference sent to a listed run during a step is counted
  in `public_requests`, and the outcome is unchanged.
- **Interruptions:**
  - A run restarted mid-step gives `inconclusive` with `run_changed`.
  - A step timeout gives `step_timeout`.
  - Cancellation mid-step keeps the measured steps.
- **Validation:** `context_too_small`, `concurrency_above_limit`,
  `run_not_ready`, `calibration_running` and idempotent replay.
- **Cluster:** the same contract on the coordinator and on a real worker.
- **Agents:** a campaign scenario that states only the task (find settings
  that let two given models share the hardware) and the result format, run
  through `zz_*` tools alone.
- **Console:** journeys for:
  - calibrating from run details;
  - both settings paths;
  - restarting affected runs;
  - calibrating again;
  - comparing;
  - saving per-run overrides.
