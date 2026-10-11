# Run core: decision record

Status: accepted 2026-10-10; R1 and R2 shipped 2026-10-11. Applies to `pkg/prov_apps` (EnsureRun, the run
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
  pins); the daemon applies it, and zzRouter arms a timer only for a client's keep_alive hint. Each load
  lists the daemon as it lands: what it holds is recorded, and ready runs it dropped are reported in the
  load's `evicted_runs`. The services ticker reconciles the rest (retire what the daemon dropped, adopt what
  it holds on two sightings, never while a load is in flight) and retries a managed daemon whose start was
  refused at boot. A pin survives a node restart only where the daemon runs as its own service.

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

## Still to come

R3: the reaper owns idle expiry (one timer heap, `Expired` in the policy); `keep_alive` and eviction on
memory pressure become tier-tree settings (`runtime.keep_alive` their default tier); a served load sends
typed options once a live check shows a later request does not reload the runner. R4: refusals reach the
wire through one table only.

## Rejected

A rewrite of the whole lifecycle (process runner, launcher, readiness probes): sound and lab-verified, high
risk, little bloat. Per-caller re-observation of the registry instead of a flight: it needs freshness stamps,
and each one tried left a race.
