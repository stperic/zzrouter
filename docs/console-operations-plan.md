# Console operations implementation plan

Status: implemented and validated; final independent whole-arc review passed on 2026-11-14.
Base: `feat/console-operations` from the merged console on `main`.
Working checkout: `zzrouter-oss-wt-console-operations`.

## Intent and specification

Implement `console-operations-spec.md` in full: observed memory/activity on the
Overview, running models and installation/download jobs with API start/stop,
and actionable node/provider/route diagnostics in the shared details panel.
The operator should be able to inspect, act and verify without changing the
existing selection/edit interaction or duplicating API decisions in JavaScript.

Complexity is high because source evidence crosses hardware sampling, worker
health, coordinator snapshots and browser lifetimes. No new package dependency
or new authorization model is needed. Existing routes/teams/budgets and protocol
23 enforcement remain canonical. This task does not authorize deployment,
committing another session's changes, or initiating install/download work merely
to generate screenshots.

## Current evidence and ownership

- `pkg/discovery/hardware/memory.go:15` discards OS-reported Used; retain it as
  optional evidence in HardwareInfo and use the context-aware gopsutil call.
- `pkg/discovery/gpu/live_card_metrics.go:54` converts N/A to zero. Preserve
  individual memory-value validity and convert every consumer together.
- `pkg/host/inventory/cache.go:23` shares disk and RAM expiry. Split those stamps;
  existing cache getters return a sample stamp with their data under one lock.
  `types.go` and `probes.go` own typed memory/card observations for HTTP clients.
- `internal/server/health.go:103` sends derived legacy RAM usage;
  `pkg/cluster/mesh/connector.go:426` drops used values. Add an explicit optional
  memory sample to health and preserve it end-to-end. Old legacy fields are not
  evidence for the new sample. Anchor source age to coordinator probe start.
- `pkg/prov_apps/instance/lease.go:27` owns admitted request counts;
  `pkg/prov_apps/health/monitor.go:157` owns activity-probe observations. Preserve
  separate known/fresh evidence without changing protective activityHold policy.
  A cheap ProviderAppManager projection aggregates instances by provider/node.
- `internal/server/hosts.go:340,575` shapes nodes. Publish memory samples and
  node/provider activity here and through authenticated health; no expensive
  GetLocalRuns/catalog reads in health. Public lifecycle DTOs retain the existing
  LocalProviderInfo state/service rather than re-deriving it in the browser.
- `runs_controller.go:83,213,282` already owns list/start/stop; jobs list at
  `jobs_controller.go:135` is per-node and paginated. Per-node UI reads expose
  partial failure, keeping `(node,id)` identities. Start uses existing catalog,
  preview, idempotency and exact-node run readback.
- `web/console/details.mjs` is the sole panel/draft owner; `api.mjs:60` is the
  session/CSRF/pending-write transport. Extend safe Problem preservation and
  optional Idempotency-Key there; do not add another request wrapper.
- `web/console/overview.mjs:124` currently pauses while selected. Pulse expiry
  must be independent of polling. Read-only updates must not rebuild focused
  controls or overwrite drafts. `topology.mjs` owns Cytoscape rendering and ELK
  layout; attach metric/dot rendering to existing nodes with fixed footprints.
- `route_preview_controller.go:119` defines explicitly unchecked categories.
  `routes.mjs:142` already performs the preview and version check. Improve this
  diagnostic in place and preserve its limits.

## Design decisions

1. Extend existing observations, not the legacy synthetic Apple GPU resource
   tracker. A typed memory sample includes measured total/optional-used bytes,
   source age and public freshness metadata. Unified memory renders one pool.
2. Activity is a domain observation with `active`, `idle` or `unknown`, counts,
   observation age and remaining validity. Probe failure and terminal state
   invalidate display evidence without changing eviction policy. The browser
   applies the supplied validity duration and stops pulses independently.
3. Use the existing Cytoscape package for attached metrics/activity marks. Keep
   them outside ELK's logical topology and counts; reserve a stable footprint.
   Both metrics and pulse target the physical node's shared details panel.
4. Workloads adds Runs/Jobs tabs, node filtering and context links from nodes and
   providers. GET polling uses one cancelable owner per reader and the same
   pause/visibility rules. Failed reads remain disclosed. No SSE implementation
   or background mutation retries are necessary for this feature.
5. Start/stop use the existing APIs, with explicit target identity and inline
   confirmation. An uncertain operation retains a read-only recovery state.
   Success requires authoritative run readback, not job acceptance/completion.
6. Diagnostics render canonical state, server messages/hints and structured
   codes. No browser policy, capacity, routing or availability decision engine.

## Research references

- [Cytoscape animation](https://js.cytoscape.org/#collection/animation) and
  [style](https://js.cytoscape.org/#style): use existing renderer APIs and stop
  owned animations on teardown/visibility/motion changes.
- [W3C Pause, Stop, Hide](https://www.w3.org/WAI/WCAG22/Understanding/pause-stop-hide):
  provide a pause control for updates and motion.
- [WAI-ARIA progressbar](https://www.w3.org/TR/wai-aria-1.2/#progressbar): omit a
  numeric value for indeterminate progress; retain a readable status.

## Execution order and validation

### 1. Preserve measured memory and source freshness (spec outcome 1)

- Update hardware Used collection, per-card optional readings, inventory cache
  stamps and snapshot-returning getters. Convert every sibling consumer.
- Extend inventory/health/mesh/node shaping and OpenAPI optional observations.
- Test non-subtraction usage, actual zero, N/A/missing partial cards, disk-first
  cache expiry, local/worker parity and old-peer omission.
- Validate `go test -race ./pkg/discovery/gpu ./pkg/discovery/hardware ./pkg/host/inventory ./pkg/cluster/mesh` and focused server node/health tests.

### 2. Expose actual activity and lifecycle evidence (outcomes 1 and 3)

- Add per-instance probe observation state and a cheap manager aggregation.
  Wire monitor success/failure/terminal transitions without changing hold policy.
- Carry optional activity through health/mesh/public node reads, including
  provider context, per-run InstanceInfo and API-owned expiry. Retain lifecycle/service fields in the
  public provider view. Preserve worker age and healthy-transport boundaries.
- Validate instance/health/provider race tests plus worker-hop API integration.
  Mutation-check the unknown/failed-probe and partial-memory regressions.

### 3. Render metrics and owned live updates (outcome 1)

- Add measured-memory formatting and detail blocks reused by Nodes/Overview.
- Extend existing topology rendering for right-side metrics and activity dots;
  keep metrics changes out of layout invalidation. List view gets equivalent text.
- Add scoped polling/pause ownership reused by Overview and Workloads; activity
  expiry remains independent. Keep existing shared panel and draft protection.
- Validate all console tests plus actual Cytoscape layout and motion tests.

### 4. Add Workloads and explicit lifecycle actions (outcome 2)

- Extend shared API for idempotency/Problem fields. Add Workloads navigation,
  Runs/Jobs lists with bounded per-node reads and explicit partial failures.
- Add run/job details and a shared-panel start form using exact catalog targets,
  preview, explicit start, job observation and authoritative run readback.
  Use reported run capability/scope, not non-cloud status alone, for choices.
  Start/preview allow 120 seconds for preparation. Handle no identity/no managed
  run explicitly and do not infer a `reused` flag from a message string.
- Add exact-run stop confirmation/readback. Preserve unknown-outcome recovery,
  retained-pin semantics, epoch/seq ordering and pagination disclosure.
  Stop requests use a 60-second browser timeout beyond the current 30-second
  cluster action timeout; a transport timeout can still follow a claimed stop.
  Missing job percent is indeterminate unless known byte progress supplies a
  ratio. Do not change the job wire contract merely to invent a zero percent.
- Validate DOM lifecycle tests and browser-admin API/worker-hop integration.

### 5. Complete diagnostics (outcome 3)

- Extend shared Problem rendering with code/node/request ID, hints and bounded
  safe evidence. Show service backoff/readiness job/last exit in provider details.
- Improve saved-route preview presentation in place with clear skipped/denied
  reason explanations, team context and the existing unchecked disclosures.
- Test escaping, obsolete responses, API failures, route-version mismatch and
  observation freshness. Verify context links return to the same panel pattern.

### 6. Review and completion audit (all outcomes)

- Run `node --test internal/server/web/console/tests/*.test.mjs`.
- Run `go build ./...`, `go vet ./...`, `go test -race -timeout 15m ./...` with this
  branch's launcher first on PATH; compare lint findings with clean base.
- Test actual assets in Chrome at 1440px and 390px, including keyboard access,
  pause/reduced motion, partial workers, active-to-idle, preview/start/job/run,
  stop, errors, pagination and diagnostics. Fixture data is synthetic and labeled.
- Fresh independent whole-arc review after implementation and each fix round,
  including new files, security/lifecycle boundaries and all acceptance criteria.
- Update `web-console.md`, CHANGELOG and this plan with actual evidence and net
  production-line change. Audit every spec requirement against current source,
  tests and rendered results before declaring the goal complete.

## Progress and amendments

- Two read-only surveys traced the memory/activity and workload/diagnostic owners.
- First independent cold review completed before this draft. Its requirements
  for independent pulse expiry, exact-operation recovery, partial lists and
  unchanged route-preview limits are incorporated above.
- Second-pass amendments explicitly cover per-run activity evidence, omitted
  zero job percentages, 120-second start preparation, missing run identities,
  unsupported local-run lifecycles and read-start anchored pulse expiry.
- Both cold planning passes are complete and their contract findings are
  incorporated.
- The public provider projection reports the existing canonical `run_lifecycle`
  (`process`, `served`, `none` or `unknown`). The older load/unload flag is
  Ollama-specific and cannot establish process-run support. Start choices use
  that lifecycle and the reported scope, without a browser provider allow-list.
- Public-only degraded probes retain the last authenticated snapshot and its
  original observation time. ProbeResult quality remains the canonical gate;
  liveness, breaker behavior and ordinary dispatch eligibility are unchanged.
- First implementation review found pagination, native gateway link, degraded
  snapshot, focus preservation, idle expiry and download-speed issues. Those
  findings are fixed. Source age now survives activity projection/aggregation.
  Follow-up checks cover pause during a pending read, cancelling pagination on
  node changes, and refreshing node choices without broadening the filter.

## Validation evidence

- Console suite: 159 tests passed, including actual vendored Cytoscape and ELK,
  shared panel/draft behavior, lifecycle operations, observation expiry and
  the new pagination/refresh regressions.
- Actual headless Google Chrome: desktop 1440px and mobile 390px journeys passed
  against task assets and a labeled synthetic cluster. Checked graph/list/detail
  navigation, memory, pause/reduced motion, keyboard selection, preview/start,
  job-done-before-run-ready, exact stop, older jobs, provider diagnostics and
  horizontal overflow. No live node, provider, model or credential was changed.
- Worker contract integration uses a synthetic TLS worker through the real
  connector and public node projection. Local health and public node samples
  agree; old/invalid/untrusted observations remain unavailable.
- Three isolated compiling mutations failed the intended assertions: replacing
  measured RAM with total-minus-available, retaining positive activity after a
  failed probe, and coercing missing GPU usage to zero. Restoring each original
  implementation passed its focused test.
- Build and vet passed. The installed linter reports exactly the same 15 findings
  on this branch and clean base (1 errcheck, 4 gosec, 10 nolintlint); none are new.
- The first full race run caught a full-quality fixture whose Connection quality
  was unset. Checking the canonical ProbeResult quality fixed it; targeted
  REST/MCP parity, browser contracts and mesh race tests passed. The next full
  race run passed.
- Second implementation review reproduced two defects: nested observations
  aliased retained registry state, and an obsolete tab-changing selection could
  open a second panel. Registry inputs and outputs now share one deep-copy
  boundary; a Workloads selection generation guards asynchronous continuations.
  Input/output isolation tests cover remote, local and registered snapshots.
  Selection tests cover both same-tab and different-tab replacements. All
  console tests and focused snapshot race tests passed after these fixes.
- Final build/vet, baseline lint comparison, actual Chrome journeys and full
  `go test -race -timeout 15m ./...` passed after the final fixes. Lint retains
  the 15 unchanged baseline findings described above.
- The third fresh whole-arc review found no remaining should-fix defects. It
  independently passed all 159 console tests, focused race tests across mesh,
  inventory, instance and GPU packages, and `git diff --check`. The reviewer
  traced measurement/freshness, probe-hold separation, exact-node recovery,
  polling/pagination, panel ownership and existing authorization boundaries.

## Requirement audit

| Contract | Implementation and evidence |
| --- | --- |
| Measured RAM, actual zero, partial GPU readings and unified memory once | Hardware/cache/sample owners, typed health projection, memory and GPU regression tests, worker-hop integration, console formatting tests and rendered Overview. |
| Activity evidence and conservative expiry | Instance/monitor owns evidence separately from eviction holds; source-age tests, failed-probe compiling mutation, request-start browser deadlines, graph and DOM expiry/motion tests. |
| Exact node/run selection and start/stop | Existing run APIs, canonical lifecycle/scope, shared panel form, per-operation idempotency, explicit confirmation, exact readback and read-only uncertain recovery; DOM and actual Chrome start/job/run/stop journeys. |
| Complete, honest workload lists | Bounded per-node reads, partial failure labels, immutable composite identities, paginated jobs, event ordering and indeterminate progress; pagination, changing-node, missing-node and focus tests. |
| Diagnostics without browser business rules | Canonical provider lifecycle/backoff/job/exit observations, sanitized node errors, saved-route preview reasons and unchecked limits, bounded text-only Problem evidence; existing route/version tests and new diagnostics/browser checks. |
| Consistent interactions and lifetimes | Existing shared details/draft/API owners, reusable snapshot polling, pause/visibility guards, selection generations, desktop/mobile layout and keyboard checks. |

Production source changes total +1,276 / -199 lines, net +1,077 (Go, MJS, CSS
and HTML; tests, docs, schemas and vendored files excluded). The increase adds
the requested Workloads view and measured observation contracts. Seven focused
browser modules reuse the existing API/panel/draft owners; there is no new
dependency, service layer, route policy engine or mutation retry mechanism.

At the implementation handoff, the branch was uncommitted and not deployed.
Browser acceptance used synthetic data and the worker contract used an isolated
TLS fixture. Older workers lacking optional observations correctly show
unavailable measurements until upgraded.

## Authorized rollout follow-up, 2026-11-14

The user authorized continuing with commit, merge and deployment, and confirmed
that no other deployment session is running. The pre-merge lifecycle/observation
stress selection passed with `-race -count=20`. Lint with the current CI toolchain,
Go 1.26.11, reproduces the same 15 unchanged baseline findings. Deployment builds
come from a separate clean checkout; the shared checkout's unrelated staged and
unstaged changes remain untouched. Each target retains its prior binary set and
must report the deployed commit, protocol and measured observations after restart.

## Overview card refinement, 2026-11-14

The overview hides providers explicitly disabled by the API. Inventory and node
settings retain every provider, including disabled entries and their deep links.
Unknown enablement remains visible when provider inventory cannot be read.

`topology-data.mjs` owns the display projection and card footprint. The existing
Cytoscape renderer owns a single clickable compute-card surface with the icon,
health label and measured memory inside it. Its attached metric text and activity
dot share that card's selection; they have no separate selection backgrounds.
ELK reserves the whole card, while metric refreshes preserve layout and viewport.
Activity uses the existing expiring pulse, with explanatory text in details and
the accessible list, rather than an unavailable label on every graph node.

Reuse the current API observations and memory formatter. Compact memory summaries
omit GPU devices with neither measured usage nor a reported capacity; full details
retain unavailable measurements. No backend, inference or policy changes.

Validate filtering, disabled-provider management/deep links, metric containment,
selection, long names and refresh stability with the existing graph/DOM suite and
rendered desktop/mobile browser checks before updating the deployed console.

Workloads uses refreshed `/nodes` health to skip known-down nodes for Runs and
Jobs, with one quiet offline summary. Unexpected read failures remain visible.
When a node goes down, discard its previous list rows and pagination; when it
returns, the normal refresh reads it again. No backend error-message matching.

Validation: all 162 console tests pass, including offline/recovered Runs and Jobs,
focused controls during a health refresh and disabled-provider deep links. The
focused Go console asset/role integration tests pass with the race detector.
Headless Chrome checks pass at 1440px and 390px: card and metric click targets,
long-title and multiple-GPU containment, provider filtering, skipped offline
requests, details panels and horizontal overflow. The production delta is +11
lines, reusing the existing renderer, inventory and read owners.
