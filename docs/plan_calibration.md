# Plan: calibration

Implementation plan for [the calibration specification](calibration-api-spec.md),
written 2026-11-19 against `main` at `9c8f544` after six read-only checks of the
current tree. The spec is the design; this file is the order of work, the
package boundaries for Codex briefs, and what each package must preserve.

Every package is implemented by Codex in its own worktree, branched from
`feat/calibration`, under a production-line budget. Claude writes the brief,
runs the suites and live checks, and reviews. `CLAUDE.md`, `AGENTS.md` and
`.claude/` are gitignored, so briefs point implementers at the main checkout's
copies.

## Decisions this plan takes

These are the plan's defaults. Change them before the briefs go out.

1. **`memory-admission: report` also skips reclaim.** The operator waived the
   check that reclaim would satisfy (refactor 7).
2. **A start with no `node` routes by model** through preview's resolution,
   but never broadcasts: no owner is a refusal asking for `node` (refactor 1).
3. **The absolute-KV path records no commitment**, like every vLLM launch today.
   Recording one would change how commitment-only refusals are classified for
   other runs (refactor 6).
4. **Restart after saving is an option chosen before the PATCH**
   (`?restart=affected`), not a button after it (Console).
5. **No `Location` header.** `POST /calibrations` answers like the other
   job-creating routes.
6. **One protocol bump, 26 with minimum 26** (main is 25/25 since provider
   operations), made once on `feat/calibration`
   before it merges to `main`. Packages before that point are tested together
   on that branch, never deployed alone to a mixed cluster.

## Coordination

Both changes this plan waited on are on `main` (2026-11-19) and merged into
`feat/calibration`:

- **The warm-route fix** removed `NodeResourceCache` and the fallback chain's
  `filterByMemory`, so nothing routes on `ResourceTracker` any more (WP5's
  Mac risk is now display only). It also touched the chain deps, the console's
  `parameters.mjs` (the node-model PATCH also sends `models: {<model>: {}}`)
  and `workload-details.mjs` (an "Edit launch parameters" block after a failed
  launch). WP4, WP5 and WP8 build on that tree.
- **`feat/coordinator-state`** opens the SQLite access store
  (`pkg/access/sqlstore`) in `server.go` and `server_factory.go`, and the
  shared test node builder disables pricing. WP7 wires its services beside
  that. The access database holds access records and audit only; calibration
  writes neither.
- **Provider operations** (same merge) are reused, not duplicated: WP3's
  `observe` read goes through their bounded `protocol.Target` read, and
  calibration binds runs by the `target_revision` operations check. MCP tools
  now register with schema-checked input and structured output.

## Work packages

| Package | Spec section | Depends on | Overlaps | Production lines (estimate) | Budget |
|---|---|---|---|---|---|
| WP1 One run start | Refactor 1 | none | none | +100 / −330 Go, −85 OpenAPI | net ≤ −150 Go |
| WP2 Memory admission | Refactors 6, 7 | none | none | +80 / −8 | ≤ +120 |
| WP3 Engine observation and admitted count | Refactors 3, 5, shipped provider files | none | none | +190 / −90 | net ≤ +150 |
| WP4 Instance request path | Refactor 4 | none | chain files | +75 / −30 | net ≤ +80 |
| WP5 Resource snapshot | Refactor 2 | none | `ResourceTracker` consumers, server wiring | +560 / −570 | net ≤ +50 |
| WP6 Calibration core | `pkg/calibration` | none | none | 1,000 to 1,700 | ≤ 1,200 |
| WP7 Node runner, API, MCP, protocol | API, Execution, refactor 8 | WP1, WP3, WP4, WP5, WP6 | server wiring | 800 to 1,000 Go, about 250 OpenAPI | ≤ 1,000 Go |
| WP8 Console | Console | WP1, WP7 | `parameters.mjs`, `workload-details.mjs` | 1,000 to 1,300 | ≤ 1,200 |
| WP9 E2E, docs, lab acceptance | Contracts, Acceptance | all | none | docs and tests | not applicable |

The refactors come to roughly 30 lines fewer together. The calibration is a new
capability and adds about 3,000 to 4,200 lines (core, wiring, OpenAPI,
console). The budgets cap that growth, and each package reports its net change
against its base.

## Waves

- **A, now, in parallel:** WP1, WP2, WP3, WP6. They touch disjoint files, and
  WP6 depends only on interfaces it defines.
- **B:** WP4 and WP5. Their blocker landed on `main`; they wait only for
  Wave A to merge, so their briefs are written against that tree.
- **C:** WP7 with the protocol bump, then WP8 and WP9.

## WP1 One run start

**Change order (the tree builds at each step):**
1. **Run policy** (`pkg/prov_apps`).
   - Add a sentinel and a typed error carrying the run id to `errors.go`.
   - Add request-tier fields to `Want` (`types.go`) and `RunView`
     (`ensure_run.go`).
   - Add the `run_settings_differ` case in `decideExisting` just before the
     default reuse. Compare parameters and environment as a pair, keyed by
     flag name (`FlattenParameters` of the request tier, as
     `parameters_status.go` does). A raw map compare would raise false 409s.
   - Add the same check to the join case (`Joined && Force`).
   - The rule applies only when the want states settings after pin replay
     (`m.replay` in `pinned_runs.go`).
2. **Error contract.** Add a `failure_table.go` row with a typed `extra`, the
   `error_status_guard_test.go` entry, and `docs/agent_error_codes.md`.
3. **Rename and `replace`.**
   - `LaunchRunRequest` (`runs_service.go`), the internal start struct
     (`runs_executor.go`, `Force: req.Replace`), `POST /runs/batch`
     (`runs_handlers.go`), MCP `startInput` and the `zz_run_start`
     description (`agent_tools.go`), and the OpenAPI `createRun` schema.
   - Test bodies and `test/agents/campaign.py`.
   - Stored records (pin records, `instance.Config`) keep `env_vars`.
4. **Preview move.**
   - A standalone `PreviewRunRequest` (today an alias of `LoadModelRequest`,
     carrying the `model` alias and the endpoint oneof). It refuses `force`.
   - `RunsService.PreviewRun`, which takes the model router, and
     `RunsExecutor.PreviewLocalRun` with its internal handler.
   - The controller calls the runs service.
5. **Start routing.** A start with no `node` uses preview's model resolution
   (holder, else the coordinator when it resolves a provider) and never
   broadcasts; no owner is a closed-code refusal asking for `node`.
6. **Client.**
   - Add `StartRun` and its response type (`id` replaces `instance_id`).
   - Switch `internal/cli/clientcli/run.go` and
     `tui/views/tui_models_actions.go`.
   - Split `--force` (match selection) from `--replace`.
7. **Delete the load path:**
   - `RunsController.LoadModel` and the controller's `loadService` field;
   - `LoadService.LoadModel`;
   - `LoadExecutor.LoadLocalModel` and `HandleInternalLoadModel`;
   - the wiring in `server_wiring.go`, `routes_public.go` and
     `routes_internal.go`;
   - `asyncRoutes`;
   - `autoDetectProvider` and its failure rows, `findMatchingModels`;
   - `apipath.RunsLoad` and its guard entry;
   - the client's Load types;
   - the OpenAPI path and schemas.
8. **Docs and the project guide's run-start passage.**

**Preserve:**
- **Idempotency.** Only 2xx responses replay, and `replace` is part of the body
  hash (`runs_idempotency_test.go`, `mcp_idempotency_contract_test.go`).
- **Reuse job id and retention** (`job_reuse_test.go`).
- **`auto_deploy` chaining.** `Force` rides `launchReq` into
  `handleAutoDeployLaunch`.
- **Worker forwarding parity** (`launch_tier_target_test.go`,
  `launch_features_test.go`, `variant_conflicts_test.go`).
- **Endpoint oneof** (`endpoint_contract_test.go`) and strict decode on the
  public body.

**Failure paths:**
- `ErrStopping` 503 with Retry-After beats `replace`.
- Admit-before-stop refusals keep 409 `out_of_range` with `kept_run` and
  `credited_mib`.
- A run held by another provider stays 409.
- The CLI prints a `--replace` hint on `run_settings_differ`.

**Tests:**
- **Run policy:** settings differ, equal, absent, key spelling, the join case,
  served-run re-ensure.
- **`replace`:** with a refused admission, the old run keeps serving.
- **CLI:** the `--replace` flag.
- **Existing tests to rewrite:** `load_*_test.go`, `ensure_run_test.go`,
  `run_parameters_test.go`, `nodes_capacity_test.go`,
  `integration_model_replica_routing_test.go`,
  `integration_mcp_api_test.go`, the internal-API integration test, the client
  and CLI tests.

**Risk:** the served-run (Ollama) re-ensure paths may start answering 409. Run
`go test ./pkg/prov_apps ./internal/server` after step 1, before going on.

## WP2 Memory admission

**Change order:**
1. **`runsettings`.**
   - Add `memory-admission` (`enforce` or `report`) to `Settings`, `Parse` and
     `IsParameter`, plus the constant in `config/run_settings.go`.
   - Add a schema spine entry, an enum.
   - Add it to the argv exclusion (`process/command.go`), the served prepare
     (`engine.go`), `launch_features.go` and `browser.go`.
2. **`schema.MemoryBudget.KVBytesParameter`** (`diagnostics.go`).
   - Validation requires a `fraction_total` budget. vLLM's `schema.yaml`
     declares `kv-cache-memory-bytes`.
   - The diagnostics decode is strict, so older workers reject the key: this is
     one reason for the protocol bump.
3. **`AdmitMemoryBudget`** (`process/memory.go`). When the parameter is set,
   take one path:
   - `admitFraction` with `sized=false`, for the startup requirement and `auto`;
   - then, per device, `WeightsMiB / len(selected) + kvMiB + safetyMargin`
     against free memory.

   A refusal comes from `memoryFitError` with the failing parameter. Skip
   `weightMemoryFit`, `sizedMemoryFit`, `needFraction` and `affordableContext`.
   Record no commitment. With unknown weights, only the startup check runs.
4. **Warning type.** An instance-side warning type, because `instance` cannot
   import `process`. It goes on `AdmissionEffects`, on `InstanceInfo` (through
   `ToInfo`), and on `LaunchRunResponse`, filled in `runs_executor.go`.
5. **The one branch** in `admitMemory`, after the holder annotation:
   `report && !capacity && errors.As(err, &fit)` moves the fit into effects
   and clears the error. Pass `!report` into `admitWithReclaim`.
6. **Projections.** The MCP field list and descriptions, OpenAPI, and docs.

**Preserve:**
- The thirteen unknown causes; unknown weights reuses `weights_unavailable`.
- Replacement refuses before the stop, and `memoryNeedsStop` stays pure.
- One reclaim round.
- `ErrMemoryBudgetInvalid` is 400.
- A refused launch never reaches `SetResolved`.
- Capacity and preview keep reporting `no_fit`.

**Tests:**
- Reproduce the lab numbers: total 97,887 MiB, fraction 0.2 (startup
  19,578), weights 952, KV 2,048, margin 4,895, hold 7,895.
- A 20 GB model at fraction 0.1 with KV set is admitted.
- The refusal names the failing parameter.
- Unknown weights.
- `report` converts the refusal, records nothing new, skips reclaim, and
  surfaces the warning on the start response and the run.
- An invalid `memory-admission` is 400 on PATCH, start and preview.
- Replacement under `report`.
- Existing: `process/memory*_test.go`, `manager_memory_test.go`,
  `manager_reclaim_test.go`, `manager_capacity_test.go`,
  `replacement_test.go`, `schema/diagnostics_test.go`,
  `providers_memory_test.go`.

## WP3 Engine observation and admitted count

**Change order:**
1. Export `config.ValidHTTPPath`.
2. **Schema.** Add `schema.Observe`, plus `YAMLSchema.Observe` and
   `ProviderSchema.Observe`, validated in `Merge` as hard errors:
   - unknown keys and roles;
   - `busy` only with `json`;
   - format from a closed set.
3. **Reader** in `pkg/prov_apps/health`.
   - It reads `schema.Observe` through `protocol.Target` on the run's port
     (`ReadJSON`, plus a sibling bounded text read in `pkg/prov_apps/protocol`).
     Delete `engineRequest` and the monitor's private size-limited read.
   - Prometheus is parsed with `prometheus/common` (promoted to a direct
     dependency): counts sum over label sets, fractions take the maximum.
   - JSON reads dotted paths; `busy` means a non-empty array.
   - It returns typed values and a typed cause.
4. **Monitor and ComfyUI move together** (a half-moved config type does not
   build).
   - The health monitor uses the reader, gated on `len(Observe.Busy) > 0`.
   - Rename `CheckConfig.ActivityProbe` to `Observe`, set from the declared
     schema in `manager_lifecycle.go`.
   - ComfyUI's `schema.yaml` gains `observe`, and `config.yaml` drops
     `activity_probe`.
   - Remove `ActivityProbe` from `app_runtime.go`, `provider_ondemand.go`,
     `provider_convert.go`, `apps_config_load_new.go` and
     `config/schema/on_demand.schema.json`.
   - Keep `activity_probe` in `managedSpineKeys` as a retired key.
5. **Admitted count.** An atomic count in `lease.go` (`Acquire`), and
   `RequestsAdmitted` in `InstanceInfo` and `ToInfo`.
6. **Shipped files.**
   - `observe` for vLLM, llama.cpp, mlx-vlm and Strata, per the spec's
     declarations table.
   - `calibration.output` for vLLM and llama.cpp.
   - llama.cpp `config.yaml` sets `metrics: "true"`.
   - Strata `schema.yaml` declares `batch` and `batch-mtp`.
7. **`/providers/{name}/schema`** projects `observe` and `calibration`; update
   the OpenAPI and `docs/comfyui.md`.

**Preserve:**
- **Busy semantics.** The same signal drives the hold and the display.
  `ActivityUnknown` releases the hold only after `maxFailures`. The validity
  window is `2 × interval + timeout`, and the hold clears on stop.
- **Payload cap** of 1 MiB (vLLM measured 56 KB under load, 2026-11-20).
- **No credential** on engine requests (`health/activity_test.go`).

**Tests:**
- Rewrite `health/activity_test.go`, `activity_hold_test.go`,
  `config/node_scope_test.go` and `instance/activity_test.go`.
- **Reader:** label combining, missing series as null, 501, JSON paths.
- **Schema validation.**
- **Reconcile** strips `activity_probe` and the config loads (precedent:
  `reconcile_ownership_test.go`).
- **Shipped templates** merge with valid `observe` (`templates_test.go`).
- **Lease count** is monotonic across refused and cancelled `Acquire`.
- **Strata** `batch` passthrough (`strata_test.go`).

## WP4 Instance request path

**Change order:**
1. **`wire`.**
   - A `StreamObserver` interface: first token, response model and text,
     usage, and end with the stream result.
   - A recorder adapter in `wire`.
   - `CopyStream(io.Writer, observer)`. The three `SetError` calls move into
     `End`.
   - Update the `CopyStream` callers: `model_streaming.go`,
     `success_envelope.go`, `ollama_helpers.go` and `wire/commit.go`.
2. **Typed instance request** in `model_app_helpers.go`: context, path and
   query, original body, client model, hints, non-nil header and `synthetic`.
   - A constructor reads today's context values from a gin request.
   - Re-sign `openInstance`, `openRun`, `instanceRequest` and
     `sendToInstance`, and their call sites in `model_app_helpers.go` and
     `model_streaming.go`.
3. **Chain.**
   - `chain/deps.go` `LeaseInstance` and `InstanceBody` take the typed
     request.
   - `chain_server_deps.go` stops forging `CtxKeyOriginalBody`.
   - Update `fakeDeps` in `fallback_test.go`.
4. **Synthetic leases** skip `proxy.RecordAdmissionWait` (`lease.go`).

**Preserve:**
- The fallback chain's per-attempt lease, body and deadline. The first-byte
  timeout stays 504 `upstream_timeout`.
- Request defaults on chat paths only.
- Model token rewriting and `answerAs`.
- Translated wires take their path from the rewritten request.
- The worker cluster engine path is untouched.
- A cancelled `Acquire` writes nothing.
- `upstreamHeaders` stays the only header builder.

**Tests:**
- Feed one recorded stream through both observers and compare.
- A Credential upstream with an empty header does not panic.
- A synthetic lease leaves no inference-log entry and no admission-wait sample.
- `WatchStream` deadlines with the typed request.
- Rewrite the `wire` tests, `r4_failure_contract_test.go` and the tests that
  stash context values.

## WP5 Resource snapshot

**Change order:**
1. **New package `pkg/discovery/resources`:**
   - `Snapshot` and `Pool` types, using pointers for unknowns;
   - the NVIDIA sampler: one `--query-gpu` with `power.draw` and
     `pci.bus_id`, plus one `--query-compute-apps`;
   - darwin unified and host samplers (gopsutil, plus
     `kern.memorystatus_vm_pressure_level`);
   - a process-tree sampler that walks from `RunRef.PID`, using process
     children on Windows;
   - an adapter, `Pool.AsMemoryDevice`.
2. `gpu.MemorySnapshotContext` and `UnifiedMemoryDevice` become thin wrappers.
3. **`prov_apps/manager.go`** uses one snapshot seam. Delete `memorySystem` and
   `memoryInventory`; `memoryTotals` reads pool totals.
4. **`inventory.Cache`** becomes one cache around the snapshot.
   - Rework `GatherMemory` and `GatherGPU`.
   - Delete `LiveCardMetricsContext` and its copies.
   - Fix the stamp-before-probe defect.
5. **`ResourceTracker`** reads the snapshot. `ResourceMetrics` keeps its JSON
   fields, the Apple figure becomes unified available memory, and unknown GPU
   memory stops reading as zero. Delete the peer health probe's `resources`
   decode (`mesh.Connection.Resources`, `ResourcesObservedAt`): nothing reads
   it.
6. **`health.go`.**

**Preserve:**
- **Admission freshness:** no cache, a 10 s bound, and `waitMemoryRecovery`.
- **Node metrics:** one probe per cache interval.
- **Sample ages** travel source-relative.
- **The console rules.**
- **WDDM `[N/A]`** stays nil.
- **Partial NVIDIA rows** are dropped, and admission refuses incomplete rows.
- **A failed compute-apps listing** sets `ProcessReason`.
- **Missing nvidia-smi:** an error for admission, nil for inventory.
- **Bounds:** the 64 KiB and 64-device caps.

**Tests:**
- Move `memory_snapshot_test.go`, `live_card_metrics_test.go` and
  `live_metrics_test.go` to the new sampler.
- Reduce the about 89 `prov_apps` seam sites to the one seam.
- Update `probes_test.go` and `resource_tracker_test.go`.
- **New:** `power.draw` and `[N/A]` power, pressure levels, process-tree
  sums, one nvidia-smi call per kind, and a guard against total minus
  available.
- `-race -count=20` on the sampling loop.

**Risk:** the Apple figure change shifts what node views show on Macs;
nothing routes on it. State it in the CHANGELOG.

## WP6 Calibration core (`pkg/calibration`)

Pure logic with no `internal/server` imports. The sender and the sampler are
interfaces.

- `request.go`: request types, `Validate`, and the limit constants.
- `generator.go`: `chat-v1` and `embeddings-v1`, prompt sizing from the
  two warm-up requests' fitted overhead and per-character ratio, and distinct
  prefixes. It never sets
  `seed`.
- `schedule.go`: per-step in-flight pools, requests per step, timeouts,
  cancellation.
- `aggregate.go`:
  - nearest-rank percentiles;
  - ITL as `(latency − TTFT) / (output − 1)`;
  - concurrent-streams peak and overlap;
  - throughput windows;
  - slot wait;
  - public requests from the counter delta;
  - pool peaks and means.
- `outcome.go`: the run, step and overall tables, and the reasons with their
  next tools.
- `report.go`: the report types, as in the spec's example.

**Tests:** the spec's acceptance list for the pure parts, including outcome
tables (mutation-checked), percentiles at small counts, ITL with one output
token, a step timeout, cancellation keeping measured steps, and a counter going
backwards or a changed `target_revision` as `run_changed`. Run
`-race -count=20` on the scheduler.

## WP7 Node runner, API, MCP, protocol

1. **Job kind.**
   - `KindCalibration` with prefix `cal_`, its own ring policy and inactivity
     timeout, and a `jobOutcomeTools` entry.
   - Job lists omit `meta.report`.
2. **Node executor** (`calibrations_executor.go`).
   - Validation:
     - the run from `GetInstance` must be running;
     - `context_length`;
     - `max_concurrent_requests`;
     - the endpoint and wire endpoints from `Resolved`;
     - the run read once through `InstanceInfo` for `parameters_status` and
       `target_revision` (a given revision that differs is `run_changed`);
   - A node claim, as `claimInflightDeploy` does.
   - `Registry.Reuse` for an identical running request.
   - `StartDetached`.
   - The runner: warm-up, steps, sampling (WP3 reader, WP5 snapshot), sending
     through the WP4 path with `synthetic` leases, and a partial report on
     cancel (`context.Cause`).
3. **Coordinator.**
   - `calibrations_controller.go` and `calibrations_service.go`, using
     `routeAndParse` to the internal route, as `POST /runs` does.
   - `respondAccepted` with `job_id` and `node`.
4. **Codes.** `run_not_found`, `context_too_small`, `concurrency_above_limit`,
   `run_not_ready`, `run_changed`, `calibration_unsupported` and
   `calibration_running` in
   `pkg/httperr/route_code.go` and `docs/agent_error_codes.md`.
5. **Surfaces.**
   - The `zz_calibrate` tool binding `createCalibration` through the
     schema-checked registration in `mcpserver/tools.go`, and its idempotency
     contract if it exposes a key.
   - The discovery `calibration` block (coordinator only).
   - OpenAPI and `asyncRoutes`, including `target_revision` in the runs
     schema (the snapshot already serializes it).
6. **Protocol.** Bump `pkg/version/protocol.go` to 26 with minimum 26, plus a
   paragraph in `docs/protocol_versioning.md`.

**Tests:**
- Integration: handler to job to worker hop; idempotent replay; retried
  forward reuse; `calibration_running`; the run snapshot fields.
- A job list with 30 retained reports stays under the proxy limit.
- The OpenAPI drift and operationId tests; `TestMCPOperationBindings`;
  `TestRouteCatalog_AsyncFlagMatchesTheSpec`.

## WP8 Console

- **`calibrations.mjs`:**
  - the form: runs, workload presets, step grid, review;
  - the live view;
  - the report, with an inline SVG chart;
  - the compare view.
- **Registration** in `browser_console.go` and its content-type test.
- **Workloads tab** and the `kind` parameter in the job-page reader.
- **Job details** hand-off in `workload-details.mjs`.
- **Run details** gain Calibrate and Change settings.
- **Run form** gains settings for one run (`parameters`, `environment`) and
  `replace`.
- **Parameter editor** gains the restart-after-saving option.

**Tests:** DOM tests in `internal/server/web/console/tests` covering the grid,
presets, validation display, both settings paths, live view, report, compare,
keyboard use, narrow widths, and a draft surviving refresh.

## WP9 E2E, docs, lab acceptance

- An e2e suite drives `POST /calibrations` to a success and an error through
  `Node.HTTPClient()`; regenerate the coverage ledgers.
- The spec's Contracts list: docs, `CLAUDE.md`, CHANGELOG.
- Live acceptance on the lab, per the spec:
  - vLLM pilot steps;
  - two vLLM runs with `kv-cache-memory-bytes`;
  - llama.cpp offload;
  - MLX and llama.cpp on Apple Silicon;
  - Ollama;
  - ComfyUI resident.
- The agent campaign scenario.

## Gates for every package

- **Brief.** The Codex brief states failure outcomes as surviving tests, keeps
  real rigs, forbids memory edits, and runs `codex exec … < /dev/null`, gating
  on its exit code.
- **Checks Claude runs:**
  - the full race suite;
  - `GOTOOLCHAIN=go1.26.10 golangci-lint run`;
  - `-race -count=20` on new concurrency tests;
  - mutation checks on outcome and policy rules;
  - a Go-idiom review;
  - a cold review of the package diff.
- **Before `feat/calibration` merges to `main`:**
  - a fresh whole-arc cold review, repeated after each fix round;
  - the protocol bump;
  - live acceptance;
  - the user's go-ahead;
  - CI green after the push.

## Live acceptance (2026-11-20)

Build `2f7e9e8`, then `83dce04` with the sizing fix, on the coordinator (Apple
Silicon), localai (RTX PRO 6000) and PC-ERIC, all at cluster protocol 26/26.

Passed:
- **vLLM pilot** (Qwen3.8-27B, 32,768 context, `max-num-seqs` 4,
  `gpu-memory-utilization` 0.8, 30,000 input and 256 output, steps 1/2/4/8):
  verified at 1, 2 and 4, queued at 8 (streams peak 4); TTFT p50 5.5, 7.9,
  11.8 and 40.4 s; KV peak 11, 23, 46 and 45%; GPU about 99% at 525 to 564 W;
  ownership verified with the vLLM process attributed; output forced to 256.
  The first run found the input sizing defect (29,994 tokens for 30,000),
  fixed before the rerun (30,060 measured).
- **Public traffic:** three chat requests during step 1 counted as
  `public_requests: 3` with the step still verified.
- **Two vLLM runs sharing the GPU** with `kv-cache-memory-bytes` (27B at
  fraction 0.62 with 8 GiB KV, 0.5B at 0.15 with 2 GiB KV), each alone and
  together: verified; the small model fell from 2,048 to 901 output tokens
  per second beside the 27B while the 27B held 34 to 35; minimum free GPU
  25.9 GB; both holders attributed.
- **Unified memory:** an MLX run on the coordinator verified; the unified
  pool listed once, per-run memory null, engine figures null, output not
  forced.
- **Ollama served model:** verified at 1 and 2, queued at 4 (the daemon's
  parallelism); GPU ownership shared with the vLLM run named.
- **Interruptions:** a run restarted mid-step gave inconclusive with
  `run_changed` (38 failed requests attributed to the change); a 5 s step
  timeout gave `step_timeout` with `change_workload`; cancellation kept the
  measured step and failed the job with `jobs: canceled`.
- **Validation and claims:** stale revision 409 `run_changed`,
  `context_too_small`, `concurrency_above_limit`, `run_not_found` (also for
  a run on another node), limit field errors, typed `run_id`/`job_id`;
  identical retry reused the job; a different request got
  `calibration_running`; a fresh request after cancellation was accepted.
- **Cluster:** calibrations on the coordinator itself and on a worker.

Passed later the same day (2026-11-20, build `991feb8`):
- **llama.cpp offload:** 8 of 28 layers on the GPU; verified, with 5.4 to 6.5 GB
  of host memory and about 7 cores attributed to the run per step and 1.77 GB
  on the GPU. With `metrics=true`, `observe` reported running and waiting
  (no KV series, as declared).
- **Embeddings:** an Ollama embedding model calibrated as an embeddings run,
  verified at concurrency 1 and 4.
- **ComfyUI resident:** listed beside a run with a workload, verified, with
  its process attributed; its activity went from `unknown` to `idle` at the
  first poll.
- **Agent campaign** (`calibrate-two-models`, vLLM on localai, the models
  absent at the start): Codex and Claude Code (Haiku) each downloaded both
  models, chose settings, calibrated them together and reported a verified
  outcome with `zz_*` tools alone, leaving no runs behind. Haiku made three
  calibrations (each alone, then both) where one suffices; the `zz_calibrate`
  description now says concurrency 0 keeps a run idle in a step, and the
  campaign's retry check no longer counts distinct successful calibrations.

Not covered live: Strata pinned host RAM (not installed on the lab),
llama.cpp on Apple Silicon (not installed on the coordinator), Windows WDDM
ownership, and the console journeys against live data (the console needs an
operator sign-in; the Chrome journey with fixtures passes).

## Follow-ups

- Done (`991feb8`): ComfyUI is no longer reclaimed or idle-expired while its
  `observe.busy` read is unknown (`activity_unknown`), and served runs report
  the endpoint the model serves from daemon capability evidence.
- Done: `zzrouter run --endpoint embeddings` starts an embeddings run from the
  CLI (`--dry-run` previews it the same way).
