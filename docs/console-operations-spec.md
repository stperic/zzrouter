# Console operations specification

Status: implemented, validated and independently reviewed on 2026-11-14. This extends the shared console interaction contract in
`web-console.md`, using the existing management APIs and panel owner.

## Operator outcomes

1. Read measured memory usage beside each compute-node icon and recognize
   observed activity without confusing a loaded service with active work.
2. Inspect models/services running on any node, start an available model or
   node-scoped service, stop an exact run, and follow installation/download jobs.
3. Understand reported node, provider and route failures within the current
   details panel, including available evidence and server-supplied guidance.

The API owns lifecycle, activity aggregation, freshness, policy, routing and
admission. The browser formats observations, collects explicit user choices,
and sends existing operations. No inference, download, installation, eviction or
mutation happens merely because an operator opens a screen or a preview.

## Overview and node details

- Compute cards contain the icon, its top-right status dot and measured
  used/total memory in GiB with tabular numerals: RAM and discrete GPU cards are
  separate; unified memory appears once. A compact map can show two GPU cards
  and a count linking to the remaining cards in details. The details panel lists
  every card with its identity and individual reading.
- Unknown usage is explicitly unavailable, including older peers and unsupported
  hardware probes. Measured zero is `0`, never unavailable. Never calculate used
  memory as total minus free/available, or add RAM and unified GPU memory.
- Samples carry source age, coordinator-relative observation time and API-owned
  freshness. Down/degraded node readings are last reported. An inventory read
  must not renew an old sample's timestamp. Partial GPU observations remain
  per-card; they cannot imply a fully measured aggregate.
- A gentle pulse means fresh, positive admitted-request or engine-probe evidence.
  Loaded models, running services, pending jobs and a failed activity probe do
  not establish active work. Unknown and stale activity never pulses. The map
  uses the dot only; list and details text says `Active at observation`,
  `No observed work` or `Activity unavailable`.
- The API supplies the remaining validity duration. A client deadline removes
  the pulse even if polling pauses or fails. Reduced-motion preferences, page
  visibility and Pause updates stop animation. Status and activity remain
  readable without motion or color.
  Anchor that duration to read start, not response receipt, so network delay
  cannot revive an already expired activity observation.
- Metric changes keep graph positions, zoom and pan. Inventory/label changes may
  use the existing ELK layout. Metrics and activity marks are graph rendering
  elements attached to the same physical node, not additional inventory nodes.
- List view and Nodes expose the same metrics and activity semantics. Node and
  provider details link to matching Workloads context.

## Workloads

One workspace uses the existing list plus right-side panel. Runs, Jobs and
Calibrations use a compute-node selector and search to preserve selection context.
All nodes reads use explicit node requests with bounded concurrency. Nodes
reported down are skipped with a quiet offline count, and reads resume after
health recovers. Unexpected read failures remain visible with the owning node.
Keys for selection include both owning node and immutable run/job ID.

### Runs

- Show model or service, provider, node, reported status and activity. Retained
  pins (`record_only`) are clearly stopped records, not running processes.
- Details show current status, active request count, endpoint, start time,
  reported memory commitments/cache evidence, failure/refusal information,
  evicted runs and any authenticated native-browser link returned by the API.
  Do not expose environment values or dump launch commands.
  Per-run activity uses the same measured observation contract as node activity;
  legacy `engine_busy` is an eviction hold and is not a display observation.
- Start opens the same panel as a form. Choose a node and an available local
  catalog model; the provider comes from the published assignment. Declared
  node-scoped providers can be selected without a model. Incomplete downloads
  and cloud models are not presented as local launch targets. Endpoint choices
  are the public run API's chat/embeddings/reranking values; the API validates
  support. Keep the exact node/provider/model in preview and start.
  Use the canonical reported run lifecycle (`process` or `served`) and run scope for offered targets. A
  non-cloud catalog entry alone does not establish a managed-run lifecycle.
- Preview displays the API assessment, its reason and unknowns. Editing a choice
  invalidates the preview. Start is explicit, uses a per-operation idempotency
  key and `auto_deploy:false`. Preview is advisory; real start rechecks state.
- A successful start shows acceptance, follows its job when supplied, and reads
  the exact run to establish readiness. Job completion alone is insufficient.
  Reused runs and expired/missing jobs retain explicit semantics.
  There is no `reused` response flag: show reported status without claiming a
  new process. Unsupported/no-run previews remain explicit. If start returns no
  run ID, show that result and an inspection action; never request an undefined
  ID or wait forever for readiness. Start's browser timeout is 120 seconds to
  accommodate preparation; recovery also covers receiving no identity at all.
- Stop shows an inline confirmation naming the exact node and run and explaining
  interruption/pin removal. Send DELETE for that identity, allow the server's
  drain timeout, and read current state afterward. Selection changes can never
  retarget a pending Stop to a replacement run.
- Interrupted or uncertain writes become a read-only recovery state with the
  operation identity and Inspect current state action. Never replay automatically
  or silently unlock an unchanged form. A server-reported committed mutation
  remains visible even if subsequent catalog publication/readback fails.
  Preserve the submitted target and idempotency key in that recovery state even
  when no run/job ID arrived; an empty list does not prove the operation failed.

- Ready runs offer **Change settings**. **Save for this model on this node**
  uses the model-on-node parameter editor. **Restart affected runs after saving**
  adds `?restart=affected` to the settings PATCH and the effect section links to
  any returned restart job. **Try on this run only** uses the same schema fields
  to prefill the run's request parameters and environment. The start preview
  reviews that request tier. **Replace the running run** opts in to replacement
  on Start. A `run_settings_differ` refusal offers the same choice and requires
  another preview before submission. When the editor was opened from a
  calibration draft, a confirmed ready replacement takes the old run's place
  in that draft and the console identifies the replacement.

### Jobs

- Show node, job kind, subject, phase, current step and actual progress. Support
  installation step counts and download bytes/speed when present. Missing
  percentage is indeterminate unless known done/total bytes provide progress.
  The legacy event omits a zero percent, so do not invent zero from its absence;
  a present number or zero done bytes with a positive total is measured progress.
- Per-node lists retain API pagination. Latest pages refresh; older pages load
  explicitly, deduplicate by node/job ID, and disclose that changing job order
  can shift page boundaries. Partial failures never become empty successful
  lists. Job event epoch/sequence prevent older events replacing newer ones.
- Details show latest event time, step, bytes, errors/warnings and retention
  deadline. Quiet but ongoing installs/downloads are not classified as stale
  from their last progress time. Missing/expired jobs show outcome unavailable.
- Observe snapshots using scoped GET polling. Lists and details have Pause/
  Resume updates and an explicit Refresh. No new SSE transport is needed for
  the requested snapshot UI. Polling has one owner, no overlapping reads, pauses
  when hidden or writing, and stops on teardown. Failed reads show their state
  and an explicit retry; polling is not a mutation/refusal retry mechanism.

### Calibrations

- Calibrations uses the Jobs reader filtered by kind, including pagination,
  explicit node ownership and offline handling. Open the form with **New
  calibration**, **Calibrate** on a ready supported run, or **Start and calibrate**
  on a completed download's node. The download entry waits for exact-run readiness
  through the ordinary preview and start flow.
- Use discovery workload kinds and limits, carry selected run revisions, and show
  served-context checks, the editable step grid, default requests and review
  notices. Associate Problems with their controls. A refresh keeps the draft and
  its selected revisions. Submit explicitly with one idempotency key.
- Calibration details share job polling, pause, refresh and cancellation. Show
  reported step progress, per-run counts and the time of the latest resource
  sample while running. A terminal report
  shows resource ownership and measured steps with an accessible data table and
  throughput chart. Null measurements are unknown, including unattributed unified
  memory. Reports can prefill another run, show a settings diff before a scoped
  save, or compare with a retained report from the same node.

## Diagnostics

- Node details show sanitized last error, last successful observation and
  transport health context. A public health response cannot establish a working
  authenticated cluster connection. Unknown cause stays unknown.
- Provider details display the canonical lifecycle state, service observation,
  backoff, readiness job and last exit where available. The provider's existing
  lifecycle owner supplies state; the UI does not reconstruct it from enabled
  and installed booleans. A missing service observation is not stopped.
- Route details make the existing saved-route `Preview as team` visible as a
  diagnostic. Show authoritative denied/skipped reasons, candidate destinations,
  team context and projected cost, with friendly text for known reason codes and
  the original code available. Preserve route-version invalidation and all
  unchecked categories, especially readiness, memory and actual inference.
- Shared API errors retain safe Problem fields: code, node, request ID,
  per-field message/hint and structured evidence. Render as text, with bounded
  expandable evidence. Do not interpret prose, run suggested commands, render
  arbitrary HTML, or show credentials/environment payloads.

## Verification requirements

- RAM used differs from total minus available in a regression fixture; zero and
  missing values remain distinct. Cache expiry is independent for disk, RAM and
  GPUs. NVIDIA partial/N/A readings retain valid sibling observations.
- Worker health to mesh to public nodes preserves samples and activity, with
  old-peer omission, invalid ages, stale probes and down nodes covered. The new
  optional observations do not change routing/admission or protocol 23 gating.
- Busy-probe failure makes display activity unknown while existing protective
  eviction holds keep their established behavior. An admitted request supplies
  positive activity; an idle service does not.
- DOM and actual-graph tests cover stable metrics layout, expired pulses,
  reduced motion, pause/resume, list parity, selection and safe text rendering.
- Workload tests cover two nodes sharing names/IDs, partial list failure,
  pagination/event ordering, launch preview/start/job/run, exact-node stop,
  missing jobs, stale completions, authentication changes and uncertain writes.
- Browser journeys use actual assets with disclosed synthetic API data on
  desktop and mobile, with keyboard navigation, no horizontal overflow, and
  no replacement of a focused control or dirty draft during refresh.
- Focused Go race tests, full console tests, build/vet, baseline-comparative
  lint and a fresh independent whole-arc review must pass. Live cluster effects
  require a separately authorized rollout; deterministic worker-hop integration
  proves this source change without modifying the running installation.
