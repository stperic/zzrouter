# Console network overview handoff

Implementation is isolated in `feat/console-network-overview`, in the sibling
`zzrouter-oss-wt-console-network` worktree. The shared checkout and running
coordinator have not been changed. This work includes the earlier connectivity
inventory/category changes and the server-root browser redirect baseline.

## Behavior

- Overview maps Clients, Coordinator, Compute nodes and Cloud providers. Both
  coordinator representations select one node identity; machine totals deduplicate.
- Nodes and Clients have dedicated navigation and deep links. Node settings edit
  the selected node's provider enablement; + Add node uses existing enrollment.
- Pairing confirms identity separately from health and never retries automatically.
  A successful pairing survives a failed inventory readback. Explicit missing
  providers do not fall back to another provider's credential form.
- Client activity uses a bounded process-local store, admin-only projection and
  evidence-based snapshot labels. Owned OAuth authorizations remain separate.
- Overview, Nodes, Clients, Providers, Routes and Access now share a nonmodal
  panel, explicit details-to-editor transitions and aligned Save/Cancel actions. Indexed browser history, in-flight write
  guards, cancellation, auth generations and secret clearing apply.
- Project terminology is recorded in `docs/client-architecture.md`; behavior and
  limitations are documented in `docs/web-console.md` and OpenAPI.

## Topology package and drawer architecture, 2026-11-13

Typography revision completed: native Cytoscape labels use a darker system font
at 22 model pixels, with 18-pixel group headings. Provider spacing is tighter;
the desktop canvas is taller. Labels are clickable and mask connectors behind
their text. Spaces and hyphens are natural wrap opportunities without changing
entity identities. Measured label-inclusive bounds reserve ELK cell dimensions;
icons stay at the top of taller cells. The layout signature includes those
dimensions so a changed display name can trigger layout without resetting the
viewport. Native device-pixel-ratio rendering and true Fit behavior remain.

Complete-tree revision completed: every reported local provider appears beneath
its own compute node by default. The expansion toggle is removed. Selection keys
contain both node and provider identity; each opens a provider-specific docked
panel with installation, enablement, service observation and owning-node context.
This reuses inventory and node-settings owners without new APIs or telemetry.
The accessible provider list includes node names. Tests cover duplicate providers
across machines, encoded identities, missing/stale observations, deep links,
refresh, node-scoped writes, Cancel/Reload context and layout.

Replaced the custom CSS map with locally bundled, pinned Cytoscape.js and ELK.
The graph module owns rendering, layout, selection, viewport and destruction;
existing inventory helpers remain the source of node/provider/client semantics.
An explicit graph projection uses separate visual IDs for the coordinator and
its compute role, both selecting the same physical node. Counts remain unique.
Unknown observations, historical clients and configured clouds retain distinct
labels. Local-provider details stay with their owning machine.

Reused `showNode`, `editNode`, `addNode` and `showClient`. `details` has an opt-in
nonmodal docked panel, a disposable lifetime and Escape/Close handling.
The overview changes selection within its page lifetime, so choosing another
node keeps the graph and viewport alive. Existing modal detail callers keep
their behavior. Navigation and selection continue to use the existing pending
write gate and indexed browser history; a replaced panel aborts its reads and
clears secrets. Changed authority destroys both map and panel.

Status-only refreshes update graph data in place. Membership or label-size changes run the
layout again; ordinary refreshes never reset zoom/pan. Layout failure leaves an
explicit error and an accessible list using the same entity projection. Disposed
pages reject asynchronous imports/layout completions. Resize observers and graph
handlers end with the page. A list view provides keyboard and screen-reader
access to each entity and the same details/actions.

Explicit Refresh updates read-only details while retaining active editors.
Reload node joins an in-flight read before reopening its panel, retaining a local
provider selection when editing began there. Layout requests
have their own revision guard, so an older result cannot overwrite newer nodes.
There is no minimum zoom floor blocking Fit for large inventories. Native panel
scrolling waits until body content is populated; overview uses overflow:clip to
avoid a hidden scroll container when details stack below the map.
When the panel narrows the canvas, a minimal pan keeps the clicked icon and label
inside it without changing zoom. The anchor follows the clicked visual role only
after selection is accepted; rejected taps during a save cannot redirect it.

Health uses larger dots at the icon's top-right, not colored square borders.
Provider identity marks cover all 16 shipped provider definitions, including
Hugging Face; Ollama Cloud shares the Ollama mark and retains its own label.
Lobe Icons supplies 12 SVG marks; MLX, llama.cpp and Strata have project marks.
Unknown custom providers retain a generic fallback. A closed mapping is shared
by topology, list view, provider lists and node details. Health remains separate
from provider identity, enablement and configuration.

Compute-node layout leaves a wider footprint for future metrics to the right.
The plan in `docs/web-console.md` specifies reported memory used/total, separate
RAM/GPU pools, freshness and reduced-motion activity indication. Memory usage
and pulsing activity are not implemented; no telemetry is invented.

Vendor tooling is maintainer-only, with exact versions, lockfile, upstream
licenses and reproducible bundle generation. Go embeds an exact public asset
allowlist. Pins are Cytoscape.js 3.35.0, ELK.js 0.12.1, esbuild 0.28.2 and
Lobe Icons static SVG 1.102.0. No CDN, remote fonts or new API is needed.
The script CSP stays self-only; worker-src 'self' permits the bundled ELK worker.

## Client naming and access review, 2026-11-13

Client labeling stays in the existing network presentation owner. Reuse one
`clientLabel` helper for map/list/card/detail headings: `Key: {name}`, falling
back to `Key ID: {id}` when unnamed. Keep the observation's identity, routes,
status and authorization semantics intact. Names reflect the last observed
request; no key-store read, credential disclosure or invented application name
is introduced. Missing/stale activity retains the existing warning path.

The initial Keys & teams review below preceded implementation. Its four
recommendations are now implemented as described in the next section. No real
key/team policy or credentials were changed during review or acceptance.

UX review findings, in priority order:

1. **Provider access is not expressible.** `access.mjs` offers only a shared-team
   textarea of exact models/groups, with no search or inventory picker. The access
   authorizer compares model identity; node/variant routing decorations are
   discarded (`internal/server/access_schema.go`, `utils.go`,
   `pkg/access/control/authorizer.go`). A provider/node checkbox must not pretend
   that selecting the current model list enforces provider/node isolation.
   Recommend explicit All/Selected/None access modes, Local providers and Cloud
   providers sections, provider marks, searchable choices and optional model
   narrowing. Actual provider restrictions require backend policy enforced on
   resolved targets, including group/fallback candidates, before this UI ships.
2. **Budgets exist but are hard to understand and monitor.** Key/team spend limits
   already support daily, weekly and monthly UTC windows; Default means monthly.
   Zero means no limit at that scope, not cloud disabled. Both scopes apply.
   The budget covers metered requests through zzRouter, not a separate allowance
   per cloud provider or the provider account's entire invoice. Local lifecycle
   providers default to zero cost unless an operator overrides pricing. Budgeted
   unknown-priced targets are refused; explicit zero-priced requests can continue
   after paid budget exhaustion. Estimates are reserved before requests and
   actual cost settles afterward, so this is not a guaranteed invoice ceiling.
   Recommend a prominent Budget section with No limit/Set limit, USD and period
   together, used/reserved/available amounts, exact next reset and key/team scope.
   Reuse `GetKeyUsage`/`GetTeamUsage`; untracked usage is not evidence of zero
   historical cost. Provider-specific dollar caps require new backend support.
3. **The key creation default hides a lasting decision.** The form starts with
   a required API ID, exposes technical limits immediately, and defaults to a
   personal team with all models. That personal policy cannot be narrowed;
   moving to a shared team requires replacing the key. Put name and team/access
   choice first, offer creating a shared team in context, preview inherited
   policy, and explain replacement before creation. Generate an editable ID;
   move throughput/reservation settings to Advanced. Do not show Team role when
   the personal-team branch always assigns owner.
4. **Inventory lacks useful relationships.** Keys show name/ID, role and status;
   teams show name/ID, kind and status. Neither surfaces membership, allowance
   summary, spending or remaining budget. Key details show only the team ID;
   team details omit member keys. Recommend searchable tables with team names,
   access and budget summaries, member drill-down, and a right-side details
   panel consistent with Overview. Keep shared-team effects visible before Save.

Reviewed create/manage key forms, shared-team forms and personal-team policy
using actual worktree assets with synthetic records in a temporary read-only
preview. No forms were submitted. The live localhost:9090 session required
sign-in, so no claim is made about its current keys or budgets. The temporary
access-review tab/server were closed. The client-label change was checked in
the persistent overview preview, dedicated Clients cards and both detail views.

Design basis: visible choices and feedback from
[NN/g's usability heuristics](https://www.nngroup.com/articles/ten-usability-heuristics/),
and native multiple-selection controls from
[GOV.UK checkboxes](https://design-system.service.gov.uk/components/checkboxes/).
The proposed grouping and budget layout are project-specific recommendations,
not findings from a user study. A follow-up usability check should ask someone
to create a local-only client, allow one cloud provider with a monthly budget,
and identify which shared limit is stopping a request.

## Keys & teams implementation, 2026-11-13

The approved implementation follows `docs/console-access-plan.md`. It replaces
the old access form with searchable Keys/Teams lists and the shared right-side
detail panel. The form begins with a name and generated editable ID, then an
explicit shared/personal team choice. Inline shared-team creation retains the
key draft, returns to its team selector and previews inherited policy and budget.
Provider and model choices use searchable native checkboxes; local/cloud groups
reuse catalog metadata and provider marks. Advanced settings and lifecycle
actions are separated from ordinary access and budget editing.

Provider access is a validated team-domain value, not a browser-only filter.
All/Selected/None travels through persistence, service requests/responses,
browser identity, API schemas, client DTOs and create/update audits. Existing
resolver selection and target admission enforce it before routing preferences,
group fallback and dispatch. Compatibility catalogs and known opaque destinations
use the same policy. Restricted unknown destinations refuse access. Internal
Ollama process-list entries carry optional provider metadata; public responses
filter by policy and strip it. Missing metadata remains hidden for restricted
callers. Invalid persisted policies prevent startup rather than silently dropping
team restrictions. Providers are cluster-wide identities, not per-node grants.

The existing quota ledger owns all spending. A shared usage projection supplies
recorded, reserved and remaining USD, tracking state and the UTC reset, including
rollover without mutating the ledger on reads. Key details show both key and team
budgets. The UI does not treat unavailable/untracked usage as zero. Budgets cover
metered usage across allowed providers, not individual provider allowances or
the full upstream account invoice.

Independent architecture, Go and frontend reviews are complete. Corrections
cover provider/catalog identity, alias round-trips, remote Ollama permission,
invalid persisted policies, audit creation, usage precision/window consistency,
pagination, retained drafts, late recovery responses and focus ownership. Final
scoped reviewers reported no remaining findings. Local review artifacts are under
`docs/reviews/` and are ignored by git.

Browser acceptance used actual worktree assets with a synthetic in-memory API at
`http://127.0.0.1:55263/console/`. Chrome verified list and panel layout, provider
choices, retained key draft during team creation, inherited access preview,
inactive invalid-budget fields, one-time key disclosure, and the narrow layout
at 150% zoom. Browser zoom is restored to 100%. The preview remains available;
its fixture is `/tmp/zzrouter-access-implementation-preview.mjs`. No real keys,
teams or credentials were created. The live coordinator on port 9090 is unchanged.

Access validation completed:

- `node --test internal/server/web/console/tests/*.test.mjs`: 121 passed.
- `go test -race ./pkg/access/... ./pkg/model/resolver ./pkg/protocol/ollama
  ./internal/client/utils ./internal/server`: passed, including the complete
  server package. Final log: `/tmp/zzrouter-access-race-final.log`.
- Startup refusal, audit and resolver corrections also passed targeted race
  checks. The earlier full runs caught an internal process-list expectation and
  a missing team-role test fixture; both are corrected in the final passing run.
- `go vet ./...`, `make build` and `git diff --check`: passed. Build includes
  the release node with verified launcher integrity and the embedded assets.
- `make lint`: exactly the same 15 baseline findings, no new findings. Baseline
  and current diagnostics were compared after normalizing line numbers.

## Earlier topology verification, 2026-11-13

- `node --test internal/server/web/console/tests/*.test.mjs`: 109 passed.
- `go test -race ./pkg/observability ./internal/server ./internal/server/mcpserver
  -count=1`: observability and MCP passed; server completed with only two new
  Clients taxonomy registration omissions. Added the Clients segment mapping.
- After that correction, `go test -race ./internal/server -run
  'TestOpenAPI|TestIntegrationClientActivity|TestIntegrationBrowser|TestIntegrationOAuth|TestMCP.*Dispatch'
  -count=1`: passed. All earlier behavioral tests passed in the full package run.
- `go vet ./pkg/observability ./internal/server ./internal/server/mcpserver`: passed.
- Topology update: `go test -race ./internal/server -run
  'TestIntegrationBrowser' -count=1`: passed, including exact asset MIME types,
  CSP, unknown-asset refusal and worker-role gating for the bundle and all icons.
- Topology update: `go vet ./internal/server`: passed.
- `go build -o /tmp/zzrouter-console-topology-node ./cmd/zzrouter-node`: passed.
- `git diff --check`: passed.
- Clean `npm ci --ignore-scripts` and `npm run build`: all 15 generated outputs
  matched their prior SHA-256 values, including provider SVG normalization.
- `make lint`: same 15 baseline findings outside this change (1 errcheck, 4 gosec,
  10 nolintlint). No new lint findings. Baseline was independently checked before
  implementation; outputs are `/tmp/zzrouter-console-lint.log` and
  `/tmp/zzrouter-topology-lint.log`.
- Independent plan, backend and frontend reviews were completed; reported issues
  were corrected and the final scoped review found none remaining. Local reports
  are under `docs/reviews/console-network-*-2026-11-13.md` (ignored review artifacts).
- Independent topology plan and implementation reviews addressed stale detail
  refresh, concurrent Reload and the minimum zoom restriction. The final review
  found no remaining issues in the provider marks, assets, lifecycle or panel fix.
- Independent complete-tree review corrected provider selection after Reload and
  prevented rejected taps from changing the resize anchor. Final review found
  no remaining actionable issues; all 107 tests and the embedding build passed.
- Typography review found long labels could outgrow fixed layout cells. Measured
  dimensions and layout invalidation corrected it; final review found no remaining
  issues. Two new tests cover long-label clearance and relayout after label growth.
  All 109 tests and the embedding build passed after the final typography changes.

## Change size

Against the branch base, the whole console-network feature adds a net 2,169
handwritten runtime lines (Go, JS and CSS, excluding tests, docs and vendor code).
The access redesign adds 701 of those lines (1,160 additions, 459 removals)
relative to the completed topology/client-label revision. That increase adds
real provider-policy enforcement and the access workspace, while consolidating
key/team usage and replacing the old form. It reuses the existing resolver,
admission, team store, quota ledger, pending-write owner and detail panel; no
second policy store or spending ledger was introduced.
The topology revision adds 416 of those lines relative to the earlier UI draft,
including 34 for the complete tree, provider-specific panel and visibility fix,
12 for typography and measured-label layout, and one shared client-label helper.
That increase implements graph projection/lifetime ownership, accessible fallback,
in-place selection and the nonmodal panel; the old CSS connections and unused
client summary were removed. There are 37 maintainer build-source lines and 12
handwritten SVG assets. Generated graph/worker/license outputs total 2,105,596
bytes; 12 small provider SVGs are copied and normalized from the pinned package.

## Browser check and rollout boundary

The real embedded JS/CSS modules are served against an isolated sample cluster
at `http://127.0.0.1:50119/console/` for visual/interaction checks. The temporary
fixture is `/tmp/zzrouter-topology-preview.mjs`; it contains only synthetic
nodes and identities and performs no real enrollment or credential writes.

Chrome Canary rendered the actual bundled worker and SVGs under the production
CSP. Checked larger dots, provider marks in map/list/catalog, both-role identity,
panel switching without map replacement, sample provider save/reload, local
complete provider branches, client/cloud details, Back/Forward, Escape/focus restoration,
zoom/Fit, and stacked layout at 200%. Fixed opening the stacked panel at its
bottom; the title and close control now appear on node selection. Browser zoom
was restored to 100%. The sample overview remains open for review. Earlier
inventory acceptance also covered Add node and modal Nodes/Clients views.
The complete-tree check includes nine sample local providers across four machines,
vLLM/Ollama/ComfyUI branches, provider-specific details, a sample save and Reload,
selected-icon visibility, owner labels in List view and the stacked panel at 200%.
The sample map is left fitted at 100% browser zoom.
Typography acceptance checked full Fit, Fit beside the provider panel, clicking
the vLLM text label, and browser zoom at 200%. A separate synthetic fixture used
long machine names and an extended provider display name; wrapped labels did
not cover adjacent nodes or their provider icons. Its tab and server were closed.
The main sample preview remains open at 100% with the updated Fit layout.

The live service at localhost:9090 still serves its prior build. No real worker
provider setting, node pairing, runtime configuration or credential was changed.
Live worker/enrollment acceptance remains a rollout check. No commit, merge, push
or deployment was performed. Preserve concurrent changes in the shared checkout
when integrating this isolated work.


## Team-owned policy revision, 2026-11-13

The user's final clarification puts provider pickers, model permissions, spending
caps and usage limits in Teams. The unshipped per-key provider-policy draft was
removed, including its shared policy type, key persistence/API changes and
intersection code. The existing `pkg/access/teams` policy remains the sole owner.

The console key form now requires a shared team, with inline team creation for a
new or dedicated policy. It edits credential identity, role and expiry and shows
provider/model access, team budget and usage limits read-only. Key POST/PATCH
bodies omit all provider/model and quota fields. Keys lists show the inherited
team budget as primary. Existing individual key restrictions remain enforced and
visible read-only, including precise small budgets. Personal keys retain lifecycle
actions and explain replacement with a team-bound key; stored memberships and
API quota contracts are unchanged.

Form controls use consistent heights, paired inputs align when labels wrap, and
Create/Save and Close share one protected action row. Chrome Canary checked actual
assets with synthetic data at `http://127.0.0.1:55263/console/`: required team choice,
inherited provider/budget/rate preview, inline team transition, the aligned key
action row and team limit inputs, grouped local/cloud checkboxes and the full-width
drawer at 150% browser zoom. Zoom was restored to 100%; the preview is left on the
Engineering team policy. No actual credentials or access settings were changed.

Independent architecture and implementation reviews are complete with no remaining
findings. The Go review found that provider browser sockets could outlive a team
provider-policy change. The existing authorization owner now captures a cloned
policy on first authentication (including nil/all) and retires the session when
that policy changes. Its existing 15-second watcher cancels open connections;
unchanged or reordered provider sets preserve the session. Regression coverage
includes the real mTLS WebSocket lifecycle and a policy transition matrix.

Revision validation:

- Console tests: 124 passed (`/tmp/zzrouter-team-owned-js.log`).
- Full affected race suite: `go test -race ./pkg/access/...
  ./internal/client/utils ./internal/server` passed; server package 244.729s
  (`/tmp/zzrouter-team-owned-race.log`).
- The browser transition matrix and real mTLS policy-change case each passed
  20 repetitions under `-race`. Logs: `/tmp/zzrouter-team-owned-policy-stress.log`
  (1.716s) and `/tmp/zzrouter-team-owned-stress.log` (317.490s).
- `go vet ./...`, `make build` and `git diff --check` passed.
- `make lint` reports the same 15 baseline findings: 1 errcheck, 4 gosec and
  10 nolintlint; normalized issue comparison found no new findings.
- Revision production diff against `/tmp/zzrouter-key-access-before`:
  +98/-42, net +56 Go/JS/CSS lines. The increase carries read-only inherited
  summaries, aligned form behavior and browser-policy revalidation. Whole
  console feature: +2,735/-510, net +2,225, excluding tests/docs/vendor/scripts.

The live coordinator on port 9090 is unchanged. This work remains in the isolated
console worktree; no commit, merge, push or deployment was performed.

## Routes and team assignments, 2026-11-13

Implemented `docs/console-routes-plan.md` in this worktree. Routes reuse the
existing model-group store, CRUD/state/preview APIs and shared details panel.
The UI adds searchable route/destination/team rows, creation, ordered destination
editing, ownership/expiry display, team links and an expandable Preview as team.
It preserves worker-pinned clouds and all advanced fields when editing. Provider
choices use inference protocols from the catalog plus positive worker inventory
observations. Registries and browser-only tools are not new route destinations.

Teams own `route_access` (`direct`, `selected`, `none`); keys inherit it. Omission
preserves existing direct access. Go enforces exact request/resolved route
identity, provider/model destination ceilings, missing/empty/expired refusal,
opaque-surface refusal and active provider-browser revalidation. OpenAI list,
single-model and model-info discovery share authorized route metadata. Ollama
lists authorized aliases and returns route topology from show without inventing
one replica's physical metadata. Saved missing assignments remain visible and
fail closed. Explicit assignment checks and deletion guards are best effort
across the existing stores, not a cross-store transaction.

Refactors remove the copied preview strategy and replica converter. Live and
preview paths now share fallback strategies, provider cooldown filtering,
pricing admission and budget estimate/availability primitives. Preview is
read-only and explicitly reports unchecked live gates. Its route ETag binds
result labels to the evaluated configuration; any write invalidates pending and
completed previews. Route mutations check conditional headers inside the group
store lock. Stale or deleted versions return 412; assigned/automatic deletes
return 409. No new routing store, ledger or browser policy engine was introduced.

Validation:

- Independent architecture and Go/frontend reviews completed; all material
  findings were fixed and re-reviewed. Regression tests cover cloud worker
  identity, stale preview labels, catalog collisions, discovery round-trips,
  node pins, route assignment/deletion, version conflicts and quota non-mutation.
- 133 console tests pass, including nine new route tests that also pass alone.
- `go test -race ./internal/server ./internal/client/utils ./pkg/access/...
  ./pkg/fallback ./pkg/model/group ./pkg/model/resolver ./pkg/dispatch/chain
  ./tests/lint` passes. Added budget parity tests separately pass under race,
  including unknown/free prices, pending reservations and expired spend windows.
- Focused final route, OpenAPI and wire-contract tests pass under race after
  additional assignment-deletion and missing-version assertions.
- `go build ./...`, `go vet ./...` and `git diff --check` pass.
- `make lint` has the same 15 pre-existing findings: one errcheck, four gosec,
  ten nolintlint. Normalized file/message comparison with the access baseline
  confirms no new findings.
- Chrome CUA access host was unavailable. Separate headless Google Chrome tested
  the real embedded assets against synthetic API records at 1440px and 390px:
  create/edit, team assignment, conditional headers, preview invalidation,
  keyboard priority ordering, inherited key policy and Escape closure. No page
  errors or mobile horizontal overflow. Desktop and mobile screenshots were
  visually inspected. No live inference, worker configuration or real policies
  were changed.

Artifacts: `/tmp/zzrouter-routes-{race-final,contract-final,budget-test,build,vet,
js-final,lint-final,browser,keyboard}.log`; screenshots
`/tmp/zzrouter-routes-{list,details,team,mobile}.png`. A synthetic review fixture
serves the actual assets at `http://127.0.0.1:55264/console/#routes`.

Change size against `/tmp/zzrouter-routes-before`, within its captured source
owners: production +1119/-359 (net +760, 42 files); tests +553/-19 (13 files);
OpenAPI +86/-8. These counts exclude docs and a comment-only adapter contract
clarification. Existing topology and Keys/Teams changes remain intact. The main
checkout and port 9090 have not been updated. No commit, merge, push or deployment
was performed.

### Route editing discoverability

Each route row now has an explicit Edit route button beside its name and
summary, using the existing editor and API flow. The action stays in the first
column so it remains visible without horizontal scrolling on narrow screens.
Verified existing route tests (9 passing) and actual assets in headless Chrome
at 1440px and 390px: the action opens editable fields and an enabled Save route.
The synthetic preview on port 55264 serves this update; port 9090 is unchanged.


## Interaction consistency completion

The full console cleanup follows `docs/console-interaction-plan.md`. Select/read,
Edit, creation, Save/Cancel, Close/Escape and mobile layout share one panel owner.
Content scopes cancel obsolete reads and wipe secrets. Refresh and recovery
callbacks retain their initiating content revision, so they cannot replace a
newer draft. Successful writes read authoritative details; partial/uncertain
outcomes and verified one-time keys retain their existing semantics.

The final whole-arc review found conflicting provider qualification could bypass
team destination policy on a worker. `resolver.Default.Resolve` now rejects a
conflict with its trusted provider binding before lookup. Protocol 23/minimum 23
requires corrected workers. Both bare and matching-qualified allowed routes are
covered by the coordinator-to-worker integration regression.

Validation for this cleanup includes 139 JavaScript tests and a real headless
Chrome journey against the actual assets with synthetic data at 1440px and
390px: routes, key/team nesting and disclosure, provider diagnostics/readback,
node settings, client setup, keyboard selection, skip links, declined navigation
and history, and mobile Cancel/Close without horizontal overflow. No production
credentials or access policy were changed by browser checks. Full `go test -race -timeout 15m ./...`, `go build ./...` and `go vet ./...`
passed. The full lint run retains the same 15 pre-existing findings (one errcheck,
four gosec, ten nolintlint), with no new findings from this arc. A mutation overlay
removing provider binding fails both the resolver test and actual worker-hop
regression by dispatching to the denied provider. Fresh whole-arc reviewers
cleared the final code. Main integration is recorded in the final result.
