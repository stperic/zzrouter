# Run core: decision record

Status: accepted 2026-10-10; R1/R2 shipped 2026-10-11 and R3/R4 shipped 2026-10-12. Applies to `pkg/prov_apps` (EnsureRun, the run
policy, the start mechanism, the engines).

## Decision

Starting, reusing, queueing, evicting and idling runs is one pure policy and one mechanism:

- **Policy** (`run_policy.go`): `Decide(Want, Node) Decision` takes every start decision from what a caller
  wants (model, provider, runtime, force, origin: API, request, install or restore) and what the node shows (lifecycle,
  reachability, catalog, the run holding the key, a starting blocker, the admission slot). It returns one act
  (refuse, ready, reuse, start, queue) with a typed reason that `internal/server/failure_table.go` maps
  to the wire. An unset decision is an error.
- **Mechanism** (`ensure_run.go`): one keyed flight per model and endpoint. The first caller creates it and runs
  the start on a context no caller can cancel (Stop can); the flight re-decides against the registry, ends at
  handoff (process started, run queued, or failure) and leaves the map before it answers.
- **Lifecycle** is derived from provider config: process (launches an engine), served (its daemon loads
  models, Ollama) or none (external endpoint, cloud). A provider with no runs answers ready when it lists
  the model. Process and served runs are the same runs, started, reused, leased, stopped and listed by the
  same paths; the engine interface (`engine.go`) holds what differs.
- **Served runs** are keyed by the daemon's name for the model, take no port, and log their lifecycle where
  a process run logs its engine. A load sends the run's keep-alive only when it is explicit; the daemon applies it.
  zzRouter's reaper handles explicit zero and client hints on unpinned runs. Loads use
  empty-input `/api/embed` so completion and embedding-only models share one lifecycle. Each load
  lists the daemon as it lands: what it holds is recorded, and ready runs it dropped are reported in the
  load's `evicted_runs`. The services ticker reconciles the rest (retire what the daemon dropped, adopt what
  it holds on two sightings, never while a load is in flight) and retries a managed daemon whose start was
  refused at boot. A surviving daemon can be observed and its resident models re-adopted;
  this does not reload a lost model
  or prove a pin. An adopted run derives its pin from current resolved launch settings.

## Readiness publication

The manager creates, stamps and closes each run's log before registry publication, including queued runs.
Admission reopens the same append log; the published log path stays immutable.

Instance.mu excludes end claims and failed/stopping transitions from a successful readiness commit.
MarkRunning stamps first readiness and, for process/health readiness, activity; it commits lifecycle log/audit
before setting Running and releasing LeftStarting waiters. The manager opens/rotates log files before that
lock and invokes warnings/catalog observers afterward. Ordinary events enqueue audit before catalog callbacks.
Served, process and adopted runs share that manager publisher; health recovery retains its existing no-event
transition. A served load that loses commitment undoes its landed model. An adopted run is already resident,
so explicit Stop unloads it even if it wins before the first readiness stamp.

## Process identity at shutdown

The launch records its leader PID and an opaque per-platform start token together
on the instance and in the shared JSON PID file (`start_token`). The node and
launcher ship as a pair and both call `process.StartToken`. `ClaimTermination`
carries the identity through the existing stop ownership path. The shared process
terminator compares the token exactly before graceful and forced signals,
including process-group signals. Exec preserves the token even when a procfs
environment read returns empty. A mismatching or unreadable recorded identity is
refused. The existing small check-then-signal window remains accepted.

On Linux the token is raw start time in clock ticks from `/proc/<pid>/stat`
field 22, parsed after the last `)` so spaces and parentheses in comm cannot shift
fields. No boot-time or wall-clock conversion is involved. On Darwin and Windows
it is gopsutil `CreateTime`: Darwin reads `kinfo_proc.p_starttime` via sysctl
([source](https://github.com/shirou/gopsutil/blob/v4.26.9/process/process_darwin.go#L88-L94));
Windows reads the kernel creation time via `GetProcessTimes`
([conversion](https://github.com/shirou/gopsutil/blob/v4.26.9/process/process_windows.go#L412-L418),
[kernel read](https://github.com/shirou/gopsutil/blob/v4.26.9/process/process_windows.go#L969-L984)).
These fixed kernel values are stable across inspections. Linux gopsutil creation
time instead adds a reconstructed boot time
([source](https://github.com/shirou/gopsutil/blob/v4.26.9/process/process_linux.go#L1103-L1108)),
which can shift between calls, especially with the uptime calculation in containers
([source](https://github.com/shirou/gopsutil/blob/v4.26.9/internal/common/common_linux.go#L48-L103)).

Restart-time orphan cleanup uses the same predicate and only counts a termination
and untracks its record after confirming exit. A refusal or termination error
keeps the record for the next pass. Already absent processes only lose their stale
files. Integer PID files and older launcher JSON without `start_token` retain the
environment-marker fallback, including its existing behavior when environment
inspection is unsupported. The launcher JSON still distinguishes its leader `pid`
from `child_pid`. Managed-daemon identification captures the same token with the
canonical binary and fixed arguments, allowing an unmarked identified daemon to
migrate through the same terminator. Windows keeps stdin shutdown and Job Objects.

## Waiter contract

A caller that joins a start in flight gets that start's outcome, the run or the error, judged by `Decide`
against its own want (another provider: 409; a different runtime or plan: conflict; Force shares the flight's
replacement). A caller that arrives after the start ended observes the registry; if the last start failed it
starts fresh. Readiness waiters hold their run and read its failure cause before its status. No timestamps or
generation counters decide freshness. An install smoke never joins a flight: its installer holds the gate the
flight waits on.

## Contract harness

`pkg/prov_apps/ensure_run_contract_test.go` and `internal/server/ensure_run_contract_test.go` pin this contract
on the real clock with parked callers (no synctest): success, failure before and after registration, crash
while loading, concurrent Force, cancellation, Stop while held, a detached leader, another provider joining,
queueing, a stopping run, a smoke during a flight, and a stop before the process is recorded. Any change to the
run core passes them under `-race -count=20` first. A behaviour test is never removed without a replacement that
asserts the same outcome.

## Idle policy and error projection

R3 resolves `keep-alive` and `evict-idle-on-memory-pressure` through the existing parameter tier tree. Nonempty `runtime.keep_alive` supplies the default tier; explicit tree settings and request overrides take precedence. The old node GPU eviction option is only an absent-value fallback. Settings stay in resolved snapshots and parameter status, and never enter engine argv. Changed lifecycle settings intentionally distinguish variant runs. Settings changes apply on restart. The final admitted launch settings fix a pin for the run's lifetime when keep-alive is negative. Inference hints remain last-one-wins for the idle timer and never pin or unpin.

The registry coalesces membership, status, activity and ownership changes. One reaper owns a deadline heap and timer, revalidates pure `Expired` and an atomic idle-stop claim before stopping. Zero keep-alive ends an unpinned run after release, held leases cannot expire, and negative timers never expire idle. Pinned runs never expire or qualify for pressure reclaim, even after positive or zero inference hints. The shared IdleState switch checks lifecycle/claim, pin, activity hold and leases in that order. Served nonzero and adopted timer sentinels mean zzRouter has no idle deadline, independently of pinning. Daemon unloads and displacement remain outside this protection. Explicit stop, Force replacement, restart and shutdown still end runs; restart replays request-tier settings. Stop cancels and drains the reaper before draining runs. Idle stops report `idle_timeout`.

R4 routes known run/provider refusals through one ordered wire table, including Retry-After and typed extra fields. Admission and cold-load producers retain typed causes; only the HTTP edge renders them. Memory-fit diagnostics survive cold loading, and generic surface defaults remain distinct. Policy messages carry no route URLs. Typed Ollama load options remain withheld. On the Windows worker with Ollama 0.32.15 (2026-10-12), loading with num_ctx=2048 and keep_alive=-1 did not preserve those settings: a later option-free /v1 request changed the context to 4096 and expiry to five minutes. The prescribed persistence prerequisite failed.

Memory-fit holders record `reclaim_skipped_reason` from the actual selection or atomic claim decision
(pinned, busy, in use or not idle), including after the admission retry. Unused commitment holders have no PID. Memory refusals remain 409 `out_of_range`; pinned holders are not `evicted_runs`. Run reports expose `pinned: true` separately from the mutable `keep_alive` timer.

## Pins across node restarts

The launching node records a plan-less process start by an operator when its resolved
settings pin it. Acceptance writes the request, before readiness; crashes, health failures,
idle stops, reclaim and shutdown retain it. Inference never writes a record, including when
config pins its run. Served engines, disposable-plan runs and install smokes never record
or replay a pin. A negative served timer alone is not a pin.

The manager atomically writes `<data dir>/pinned_runs.json` with mode 0600, version 1.
It contains only provider, canonical model, requested endpoint, request-tier parameters and
environment, and a stable `run_id` belonging to the record. Legacy records acquire that
handle at load and persist it. Replays and restarts retain the handle; live runs have their
own IDs. Current provider tiers, paths and ports resolve again on each launch.

A start or preview with no parameters and no environment replays the matching provider's
record. Stating either uses that whole request tier: no merge with the saved request.
For example, only `keep-alive: -1` replaces the saved overrides with just that setting.
An accepted operator start rewrites the record when pinned, or removes it when unpinned.
A live run is reused even if a start states different parameters; Force applies changes.

Only an operator stop forgets a record after successfully ending its run. This includes
`zz_run_stop` and CLI `zzrouter stop <model>`. A stop by a record handle refuses while
another run holds the key, naming its run ID. A concurrent new start's record survives the
old run's stop. Deleting weights or disabling/uninstalling a provider does not forget pins.

`RestorePinnedRuns` replays records sequentially once after `Cache.Start`, through
`EnsureRun` with `OriginRestore`, without Force or detached starts. Existing flights and
runs deduplicate it with inference. Shutdown drains these starts and retains every record.
Refusals are kept without retries. A failed registered run remains failed and pinned with
its failure; after it leaves the registry, `zz_runs` lists the record in `instances` as
stopped, pinned and `record_only`, with parameters and its in-memory `last_refusal` closed
code. Use `zz_run_start` or `zz_run_restart` to replay, `zz_params_set` to fix config,
and `zz_run_stop` to forget. A record-only entry has no run log. Corrupt or unknown-version
files are set aside as `.corrupt` evidence and are not restored.

Every start prepares before queue acceptance or replacement stop: schema, platform/GPU,
features, toolkit, assets and argv. Preparation shares preview and parameter-status logic.
As required by `docs/agent-memory.md`, slow per-model conversion/packing never runs in
prepare or command building, including while an old run still serves. A preparation
refusal leaves that run and record intact. Memory admission, weight verification and process
start remain after the stop: a memory-refused replacement leaves no live run, keeping the
old record. Preparation runs twice per start, before the stop and again in up; a config change
between those checks can also leave nothing running. Admitting a replacement before stopping is deferred.

## Deployment outcome ownership

The worker DownloadTracker owns physical execution, cancellation and generations.
The worker jobs registry delivers immutable attempt events with replay and bounded
retention. Coordinator DeploymentTracker owns retained per-node placement outcomes
and atomically aggregates them. GET/list read snapshots; they do not repair state
from reusable download keys. Service-owned observers subscribe locally or consume
the existing remote jobs SSE transport and stop on demotion, replacement or shutdown.

Completion follows feature/catalog publication. Stream loss allows one bounded
terminal snapshot reconciliation; malformed identity or framing fails closed.
Unconfirmed tracking fails placement readiness without claiming that physical work
stopped. Deployment restart and cross-node auto-deploy waits use canonical retained
outcomes after job expiry. Coordinator cancellation records intent before sending
only immutable job cancellation, including late acceptance; explicit download-key
operations remain worker execution controls. Deployment retention has one owner:
the 30-minute terminal sweep on creation. Restart durability remains in-memory.

## Persisting model settings

`zzrouter run --param` supplies request overrides. Persist model settings in the
provider's `models` tier through the TUI editor or `zz_params_set`; use
`zzrouter providers variant set` to create a named variant. The unused `run --save`
flag, `POST /zzrouter/v1/models/defaults` and `PATCH /zzrouter/v1/runs/config`
have been removed. Run-manager configuration still comes from `node.yaml`.
