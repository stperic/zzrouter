# Console UX cleanup

## Outcome and scope

Implement the complete console UX review accepted on 2026-11-14. An operator
must be able to understand the cluster, select an item, inspect it, edit it,
and recover from a failed operation without losing context or hunting for
controls. Preserve the existing green visual identity and shared detail panel.

The source base is the deployed console at `4f0194e`. Work is isolated on
`feat/console-ux-cleanup`; the main checkout contains another session's changes.
No public push or unrelated backend changes are included. Local browser
acceptance and a verified local console deployment are part of completion.

## Ownership and contracts

- `console.mjs` owns the authenticated shell, navigation and retained Chat
  session. `API` still owns authority invalidation, CSRF, request cancellation,
  uncertain writes and session verification. Ordinary navigation may retain
  Chat state; authority clearing and pagehide must destroy it synchronously.
- `details.mjs` continues to own one nonmodal panel, focus restoration, content
  lifetimes and dirty drafts. Refactor the shared page layout and existing
  panel CSS so the header and action footer remain visible. Do not add a
  competing drawer, duplicate navigation tree, resize polling or DOM scraping.
- `dom.mjs` owns common controls and error presentation. Form feedback must
  reveal errors, connect known invalid controls to their messages and preserve
  uncertain/applied write evidence. Field checks collect input; the server
  remains the authority for access, budgets, routing and lifecycle decisions.
- `chat.mjs` owns console model discovery and settings. Reusable
  `internal/server/web/chat` code keeps SDK/assistant-ui ownership of messages,
  composer and transport. Retention is memory-only and belongs to one verified
  browser authority. Leaving cancels inference and retains honest partial/error
  outcomes. No automatic retries, model selection or persistent prompt storage.
  Update visible Chat privacy copy and the canonical retention contracts in
  `docs/console-chat-plan.md` and `docs/web-console.md` with the implementation.
- `network-data.mjs` and `topology-data.mjs` remain the canonical relationship
  representation. Map visibility/expansion is presentation state only; stable
  identities, enabled providers, coordinator/worker roles and unique node counts
  are preserved. Keep Cytoscape/ELK, with readable zoom and explicit focusing.
- Existing feature modules retain their reads, writes and recovery paths.
  Preserve route ETags, provider revisions and save/enable distinctions, node
  partial updates, key one-time disclosure, nested team drafts, live read
  quiescence, stale-result rejection, and saved-but-unavailable readback states.

## Required reading and existing mechanisms

- `docs/console-interaction-plan.md`, `docs/console-chat-plan.md`,
  `docs/web-console.md`: existing feature and security contracts. This plan
  supersedes the Chat document's clear-on-navigation behavior only; authority
  invalidation and explicit clearing remain mandatory.
- `internal/server/web/console/{console,api,details,dom}.mjs`: page/session
  lifetimes, navigation and draft guards, shared controls.
- `internal/server/web/console/console.css`: consolidate the current shell,
  panel, form and Chat rules instead of appending a second override layer.
- `internal/server/web/console/{chat,overview,topology,topology-data}.mjs` and
  `internal/server/web/chat/{chat.tsx,transport.ts}`: session and graph seams.
- `internal/server/web/console/{access,access-form,access-policy,access-budget,
  route-form,routes,nodes,providers,clients,workloads,workload-data}.mjs`:
  preserve current feature contracts while simplifying presentation.
- `internal/server/web/console/tests/*.test.mjs`,
  `scripts/console-vendor/{package.json,build.mjs,tsconfig.json}`: existing tests
  and reproducible pinned bundles. Baseline: 183 JavaScript tests pass.

## Implementation sequence

### 1. Shared shell, panels and feedback

Bound the authenticated application to the available viewport. Give ordinary
pages one scrolling content area; give list/detail workspaces a bounded panel
and independently scrollable list. Use remaining-height flex/grid ownership,
not guessed header heights. Keep page actions, panel Close, and Save/Cancel
available at desktop, laptop and phone sizes. Keep narrow selected views in
the existing workspace and preserve the nonmodal semantics.

Replace horizontal hidden mobile destinations with an explicit Navigation
disclosure controlling the same navigation element. Close it after successful
navigation and on Escape; preserve focus and rejected dirty navigation. Remove
repeated page branding, reduce header whitespace, normalize checkbox rows and
fix the sign-in contrast failure. A deep-linked panel must restore focus to
its selected item or the page heading, never BODY.

Consolidate error presentation into an actionable primary message and optional
technical details. Retain uncertain-write and mutation-applied information
prominently. Form failures receive a focused summary linked to known invalid
controls; open collapsed ancestors before focusing a field. Native validation
and API field errors must remain usable inside collapsed sections.
Local policy/budget input helpers must attach their invalid control to the
validation error; never recover the target by matching error text. Map server
error keys through form-local mappings rather than assuming UI input names
equal API paths.

### 2. Chat and topology

Retain Chat messages, settings and draft across ordinary same-authority
navigation. Cancel active inference on exit, preserve partial output with its
outcome, and clear everything on New chat, sign-out, authority change/loss or
pagehide. Validate late callbacks and failed session probes. Fill the available
content height and keep the composer visible on mobile.

Use the existing detail panel for an accessible searchable model/route selector
with native result buttons, API-supplied provider/node context and an explicit
exact-ID action. The toolbar shows the current selection. This reuses selection
and focus ownership rather than introducing a custom ARIA widget. Add role-appropriate
empty-state actions. Route details can open Chat with the route preselected;
this must not silently discard an existing conversation or bypass settings and
access enforcement.

Default narrow screens to the list; preserve explicit map/list choice and
position across refresh/details. Keep readable graph zoom rather than shrinking
labels to fit every node. Add node search/focus and expandable provider branches
for large clusters while retaining the full enabled-provider relationships.
Move connection actions into the network toolbar and simplify activity copy.

### 3. Feature presentation

- Nodes: compact provider controls with identity, enabled state and installed
  evidence aligned; RAM/GPU remain prominent; secondary activity in details.
- Providers: summarize reported instances, nodes and attention needed, without
  inventing health from saved credentials. Keep configuration and verification
  distinct and incomplete inventory explicit.
- Routes: compact destination summaries, expanded current editor, clear
  priority/fallback order, preserved advanced fields and accessible reordering.
- Teams: coherent Access, Budget and Advanced limits sections; move the large
  usage readout to Details. Keep provider/model/route policy distinctions and
  shared budget semantics explicit. Keys remain name/team/expiry with inherited
  policy and clearly labelled existing additional limits.
- Clients: key name primary, plain activity/last-seen labels; keep observation
  and authorization distinct. My Access leads with the reported key and team
  policy scopes and server-reported usage, then the connection guide. `/me`
  does not expose a resolved effective-policy object: do not calculate an
  intersection, minimum limit or remaining budget in the browser.
- Workloads: active workloads by default with an explicit stopped-record filter;
  retain selected stopped records and authoritative status. Quiet expected
  offline skips; one concise unexpected-error message with explicit read-only
  retry and expandable technical details; keep stale snapshots labelled.
- System: rename the read-only Settings view, preserve old URL compatibility,
  simplify installation guidance and offer copying relevant public values.
- Empty states: clear next action appropriate to role and missing resource.

### 4. Verification, review and delivery

Add meaningful behavioral regressions for lifetimes, focus/errors, graph
visibility, filters and navigation. Browser-test actual bundled assets with
synthetic APIs at 1440x900, 1280x800, 390x844 and 320px width. Reuse the prior
review scenarios, correcting fixture shapes against current API types. Keep
browser evidence and tests reproducible, and never use production credentials
in fixtures. Check all affected views, not only Chat and Overview.

Run fresh architecture review before implementation, review the drafted plan,
then independent full-diff code/security/accessibility reviews after changes
and after consequential fixes. Build the release from this isolated tree and
verify the locally deployed identity and assets. Do not stage, merge or deploy
the unrelated changes in the primary checkout.

## Acceptance checklist

- [x] All shared editors show Close and Save/Cancel within the available viewport.
- [x] Mobile navigation exposes every authorized destination; dirty navigation
  rejection preserves the current screen and controls.
- [x] Chat conversation, settings and draft survive ordinary navigation; Stop,
  partial results, regeneration and route preselection behave predictably.
- [x] Auth loss/change/sign-out/pagehide clears all retained Chat state and
  rejects stale callbacks; no inference is retried automatically.
- [x] Chat composer remains visible at desktop and phone sizes.
- [x] Model picker supports keyboard use, API context and exact identifiers;
  empty and failed discovery offer relevant recovery actions.
- [x] Four-node and 20-node topology remain readable; focus/branch expansion,
  enabled providers, list alternative and selection/refresh preservation work.
- [x] Node/provider summaries show useful, truthful observations and compact
  controls, with no disabled-provider clutter in the overview tree.
- [x] Route creation/editing/reordering retains every saved advanced field;
  Try in Chat and team assignment context work.
- [x] Teams expose Access/Budget/Advanced cleanly; keys inherit policy without
  duplicating business rules or dropping existing additional limits.
- [x] Form errors are visible and linked to controls; pending, uncertain,
  conflicting and applied-but-unreadable writes remain safely recoverable.
- [x] Workload filters, stopped selection, offline skips, partial failures,
  explicit retries and stale-observation labels remain correct.
- [x] Clients/My Access, System and all empty states use clear terminology and
  appropriate next actions; old view links remain valid.
- [x] Keyboard focus returns after normal and deep-link panel closes; sign-in
  contrast passes; responsive layouts and reduced-motion behavior are checked.
- [x] Required tests, bundle/type checks, build/vet, fresh reviews and local
  deployment verification are complete, with net production-line change reported.

## Validation commands

From the isolated worktree:

```sh
node --test internal/server/web/console/tests/*.test.mjs
npm --prefix scripts/console-vendor run check
npm --prefix scripts/console-vendor run build
git diff --check
go test -race ./internal/server -count=1
go vet ./internal/server
go build ./cmd/zzrouter-node
```

Run focused behavioral tests after each owner change, the complete listed
checks after integration, and browser acceptance against the final assets.
Include short-height and software-keyboard-sized Chat, expanded mobile
navigation, stacked verification/error notices, rejected dirty-settings
navigation and picker switching. Picker search is uncommitted input; only an
explicit result or exact-ID action changes the model, and closing the picker
restores focus to its Change control.
If Go code changes, run its owning packages' race tests and lint as well.
Document pre-existing failures against an unchanged base instead of weakening
checks. No numeric line budget: prefer removal/reuse and explain necessary growth.

## Research references

- https://www.nngroup.com/articles/ten-usability-heuristics/: consistency,
  visible status, recognition and error recovery.
- https://design-system.service.gov.uk/components/error-summary/: focused
  summaries connected to invalid fields.
- https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html: contrast.
- https://www.w3.org/WAI/WCAG22/Understanding/reflow.html: responsive reflow.
- https://www.w3.org/WAI/ARIA/apg/patterns/combobox/: keyboard/ARIA picker contract.

## Progress and amendments

- Baseline confirmed: local health reports build `0.2.3+11142227` and source
  `4f0194e`; isolated worktree is clean; all 183 baseline console tests pass.
- The architecture plan is recorded under `docs/` to follow this repository's
  existing console plans and shared instruction location, rather than creating
  a duplicate plan under the generic PIV skill's `.Codex/plans` path.
- Initial cold review found no architectural blocker. Keep the existing
  in-form sticky actions so nested team creation cannot submit the detached key
  form. Map structured validation keys to controls explicitly, including route
  destination indices, and handle native `invalid` events as well as submit.
- Chat exploration confirmed the pinned assistant-ui runtime's public
  `thread.cancelRun()` and composer reset APIs. Retain the mounted canvas/runtime
  between views rather than exporting a snapshot before cancellation settles.
  Route metadata is `owned_by: zzrouter-route`; missing provider metadata alone
  must not label an entry as a route. A route handoff changes only the next
  request's model, preserving the conversation and draft without sending.
- Topology exploration confirmed that initial hidden canvases need deferred
  camera placement, and search of the coordinator must target its compute
  representation explicitly. Expansion must reveal selected provider branches
  and remain separate from the complete List view and management identities.
- Draft-plan cold review identified explicit local-validation targeting,
  scope-preserving My Access wording, canonical retention documentation, and
  constrained-height/dirty-picker acceptance. These are included above.
- Browser evidence showed that expanding every provider branch by default on a
  laptop clipped the client and provider levels at a readable zoom. Initial
  expansion now also considers the actual available canvas height. Selecting a
  node reveals its branch; List view always retains the complete enabled tree.
- Shared field feedback maps server paths explicitly, handles native invalid
  events and clears obsolete links after destination reordering. Chat retains
  one mounted runtime per authority, with short-height chrome and a keyboard
  accessible conversation scroller.

- Integrated validation: 190 console unit tests pass; seven real-Chrome browser
  journeys cover all views, native/local/server validation, focus, retained Chat,
  cancellation, authority clearing, 20-node maps and constrained viewports.
  TypeScript, bundle generation, server race tests, server vet, Go build and
  diff checks pass. Independent final review reports no remaining findings.
- Runtime production change: +380 lines, excluding tests, generated assets,
  documentation and build tooling. Growth implements authority-owned Chat
  retention, shared form feedback, graph visibility/camera behavior and responsive
  controls through existing feature owners. No backend policy path was added.
- Local release `0.2.3+11150155` is deployed from source `df7c9f6`.
  Public health reports the expected source and healthy coordinator identity;
  all 58 served console assets match the tested files byte for byte. The
  installed launcher is unchanged and matches the server's embedded integrity
  hash. The primary checkout's pre-existing Git status is unchanged. A rollback
  copy of the prior server is retained outside the repository.
