# Run core: decision record

Status: accepted 2026-10-10; R1/R2 shipped 2026-10-11 and R3/R4 shipped 2026-10-12. Applies to `pkg/prov_apps` (EnsureRun, the run
policy, the start mechanism, the engines).

## Decision

Starting, reusing, queueing, evicting and idling runs is one pure policy and one mechanism:

- **Policy** (`run_policy.go`): `Decide(Want, Node) Decision` takes every start decision from what a caller
  wants (model, provider, runtime, force, origin: API, request or install) and what the node shows (lifecycle,
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
  a process run logs its engine. A load sends the run's keep-alive only when it is explicit (number -1
  pins); the daemon applies it, while zzRouter's reaper handles explicit zero and client hints. Loads use
  empty-input `/api/embed` so completion and embedding-only models share one lifecycle. Each load
  lists the daemon as it lands: what it holds is recorded, and ready runs it dropped are reported in the
  load's `evicted_runs`. The services ticker reconciles the rest (retire what the daemon dropped, adopt what
  it holds on two sightings, never while a load is in flight) and retries a managed daemon whose start was
  refused at boot. A pin survives a node restart only where the daemon runs as its own service.

## Readiness publication

Instance.mu excludes end claims and failed/stopping transitions from a successful readiness commit.
MarkRunning stamps first readiness and, for process/health readiness, activity; it commits lifecycle log/audit
before setting Running and releasing LeftStarting waiters. The manager opens/rotates log files before that
lock and invokes warnings/catalog observers afterward. Ordinary events enqueue audit before catalog callbacks.
Served, process and adopted runs share that manager publisher; health recovery retains its existing no-event
transition. A served load that loses commitment undoes its landed model. An adopted run is already resident,
so explicit Stop unloads it even if it wins before the first readiness stamp.

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

R3 resolves `keep-alive` and `evict-idle-on-memory-pressure` through the existing parameter tier tree. Nonempty `runtime.keep_alive` supplies the default tier; explicit tree settings and request overrides take precedence. The old node GPU eviction option is only an absent-value fallback. Settings stay in resolved snapshots and parameter status, and never enter engine argv. Changed lifecycle settings intentionally distinguish variant runs. Settings changes apply on restart; inference hints remain last-one-wins.

The registry coalesces membership, status, activity and ownership changes. One reaper owns a deadline heap and timer, revalidates pure `Expired` and an atomic idle-stop claim before stopping. Zero keep-alive ends after release, held leases cannot expire, and negative values never expire idle. Pressure eviction may reclaim idle pins when enabled. Served nonzero runs and adopted models remain daemon-expired. Stop cancels and drains the reaper before draining runs. Idle stops report `idle_timeout`.

R4 routes known run/provider refusals through one ordered wire table, including Retry-After and typed extra fields. Admission and cold-load producers retain typed causes; only the HTTP edge renders them. Memory-fit diagnostics survive cold loading, and generic surface defaults remain distinct. Policy messages carry no route URLs. Typed Ollama load options remain withheld. On the Windows worker with Ollama 0.32.15 (2026-10-12), loading with num_ctx=2048 and keep_alive=-1 did not preserve those settings: a later option-free /v1 request changed the context to 4096 and expiry to five minutes. The prescribed persistence prerequisite failed.
