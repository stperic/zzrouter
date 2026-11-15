# Complete console and terminal companion

## Outcome

The browser console is the complete interactive management interface. The TUI
remains a keyboard-oriented companion for common work, with a direct console
launcher. The CLI retains its automation and recovery capabilities. This is the
full scope accepted on 2026-11-15, building on `docs/console-ux-plan.md`.

Source base: `e11e3c9`, isolated in `feat/console-complete`. The primary checkout
contains unrelated concurrent work and must not be edited, staged or merged.
Complete the work, test and independently review it, commit locally and verify a
local deployment. No public push is requested. Live tests must not download
models, install engines, change policies or update remote nodes merely to test UI.

## Architecture and ownership

- Existing coordinator APIs own authorization, policy, validation, capacity,
  lifecycle, pricing and update orchestration. No parallel browser business rules.
  General management already accepts protected browser sessions. Worker-local
  bootstrap and recovery still require the terminal before the coordinator exists.
- `console.mjs` owns navigation, session lifetime and page selection. Add Models
  and Logs destinations; keep pricing, updates and setup inside System. Existing
  URLs and role restrictions remain valid. Every feature uses `details.mjs` for
  inspect/edit/create, `dom.mjs` for forms/errors and `live-reads.mjs` for bounded
  observations. Avoid another drawer, generic JSON editor or workflow executor.
- `api.mjs` owns same-origin requests, CSRF, pending/uncertain writes and authority
  invalidation. Add an explicit merge-patch content type and allow only GET run
  log/job stream routes in the existing raw response path. Use explicit response
  contracts: `/resolved`, `/schema`, update history and job cancel are unwrapped.
- A shared bounded SSE reader uses the already pinned `eventsource-parser`,
  bundled alongside existing vendors. Abort on view/session exit, render plain
  text, preserve reset/error/terminal evidence and offer explicit reconnect.
  A reconnected run-log stream replaces its replay buffer, because it replays
  from byte zero. Job streams retain explicit epoch/sequence and dropped-event
  evidence; transport parsing does not impose one resumption policy on both.
  Never replay effectful requests on network failure.
- Sensitive environment values and inference payloads are revealed deliberately,
  held only by the current view, marked for existing sensitive-content clearing,
  and never stored in browser storage or URLs. Hide means clearing their DOM.
- Runtime settings use `/providers/:name/schema`, `/resolved` and the canonical
  RFC7396 PATCH writer. Scope is explicit: provider defaults, model, node,
  node/model, optionally endpoint. Typed controls follow schema metadata; null
  removes an override. Show provenance without resolving it again in JavaScript.
  `/resolved` is only effective evidence and can hide lower tiers. Extend the
  existing GET `/providers/:name/parameters` owner with an explicit `scope`
  query for a coordinator-owned exact-tier projection (defaults/model/node/
  node-model, plus endpoint). Keep the old unscoped read compatible. This small
  additive API read fills the discovered gap; never synthesize hidden tier
  values in the browser. ETags remain advisory: re-read the exact edited tier
  before a write and disclose the remaining concurrent-editor race. Preserve
  unrelated fields and secrets.
- Exact model details also bind the existing `source_id` selector through the
  public ShowModel owner. Name, provider, node, registry and source stay together;
  a repository alias must not choose a different quantization. Chat/Test retain
  the provider-qualified literal name and actual node. The inference API still
  reports unsupported physical-source ambiguity rather than guessing a copy.
- All accepted asynchronous work retains operation identity, node and returned
  job/deployment ID. Acceptance, EOF and a missing job never imply completion.
  Inspect owning resources after completion. Software updates may restart the
  coordinator or run in a privileged process; reconcile status/history instead
  of assuming a progress stream survived or installed the requested version.
- TUI and CLI use the existing connected client and platform opener. Discover
  the configured browser issuer, validate a credential-free HTTP(S) URL, and
  open it on the user's desktop. SSH/headless/no-open prints the URL. No key or
  browser login bypass is passed through the URL. Use one URL builder for menu,
  CLI and supported resource links.

## Capability acceptance matrix

| Area | Required console behavior | Existing API owner |
| --- | --- | --- |
| Models | Available inventory, filters, details, explicit Test/Ping with timing and served-by evidence, Chat/start/parameters links, exact deletion with partial outcomes | `/models`, `/models/show`, existing inference, DELETE `/models` |
| Discovery | Declared registries/providers, search/filter/pagination, model cards, grouped variants/tags, explicit node targets with advisory capacity | `/search`, `/models/card/*`, `/registries/*`, `/nodes/compatible` |
| Downloads | Review selection, submit deployment once, per-node progress, inspect/retry reads, cancel exact deployment or node | `/deployments`, `/deployments/:id[/nodes/:node]` |
| Providers | Preflight, declared components/runtime/version, reviewed install plan, full and per-step execute/verify, copy reported step commands/scripts, upgrades, uninstall, versions/pin | `/providers/:name/install/*`, `/upgrade`, `/versions`, PATCH/DELETE provider |
| Provider services | Inspect status, apply configuration, start/stop/restart exact node service; remote Ollama connection | `/providers/:name/service/*`, `/providers/instances` |
| Parameters | Searchable typed settings, environment reveal/edit, provenance, reset overrides, correct scope and endpoint | `/schema`, `/resolved`, scoped GET and PATCH `/parameters` |
| Workloads | Existing runs/jobs plus deployment access, runtime logs, exact job cancellation with truthful outcome | `/runs/:id/logs`, `/jobs/:id[/stream]` |
| Inference logs | Filtered request list, details, live feed, deliberate retained payload reveal, bounded memory and clear unavailable state | `/inference-logs`, payload and jobs stream |
| Pricing | Catalog/status, unpriced observations, create/edit/delete provider/model overrides, correct USD per million units | `/pricing`, `/pricing/status`, `/pricing/overrides` |
| Updates | Node status/history, check release, explicit target/version rollout, coordinator rollback, scheduled-update settings, inspect after interruption | `/update/status`, `/history`, `/check`, `/apply`, `/rollback`, `/settings` |
| Guided setup | Observed checklist and direct navigation through Nodes, Providers, Models, access and Chat; no fabricated completion | existing resource reads and screens |
| Terminal launch | `zzrouter console` and top-level Open web console; URL-only option and headless fallback | existing client, OAuth issuer, platform opener |
| Terminal access | Explicit shared team when issuing keys, full inherited policy summaries, team policy editing through contextual console handoff, individual legacy limits read-only | existing Keys/Teams APIs and client types |
| TUI organization | Console primary entry, common tasks prominent, advanced administration grouped; existing workflows remain reachable | existing menu and view dispatcher |

## Implementation sequence and parallel ownership

1. Record API contracts and independent initial architecture review. Review this
   drafted plan before implementation. Reuse the known panel/guard patterns.
2. Shared transport/navigation and integration are owned by the root implementer:
   `api.mjs`, `console.mjs`, `console.css`, `browser_console.go`, vendor tooling,
   workload integration, Logs, System/setup/update modules and browser tests.
3. Independent model/provider work owns `models.mjs`, `model-discovery.mjs`,
   `deployments.mjs`, `provider-lifecycle.mjs`, modifications to `providers.mjs`
   and their tests. It calls shared panels and integrates through exported
   feature functions, with no shell/API/CSS edits. Preserve accepted install
   options exactly, including `expected_plan_id`, action/runtime/components.
4. Independent parameter/pricing work owns `parameters.mjs`, `pricing.mjs`, the
   additive exact-tier projection in the existing parameter API and their tests.
   It exports panel-level parameter editing and System pricing.
   Schema selects input types; the server remains the validator. Pricing edits
   preserve cache-read rate and other existing override fields.
5. Independent terminal work owns console command/client URL discovery, TUI
   menu/navigation and key/team forms/types/tests. TUI teams become policy
   summaries with contextual create/edit handoff to the console, removing the
   duplicate policy editor. Key creation retains explicit team selection and
   credential lifecycle remains in the terminal. Reuse shared primitives and
   `tea.Cmd` effects. New keys must never silently become personal unrestricted
   teams when the operator intended shared policy. Existing personal keys stay
   inspectable and lifecycle actions remain available.
6. Integrate all features, modernize guidance/empty states, complete the TUI
   action matrix, and run full browser journeys at desktop and phone sizes.
7. Independent Go/domain and browser/code-quality whole-diff reviews. Fix scoped
   findings and recheck consequential changes. Build both client and node from
   this tree using the installed unchanged launcher integrity hash; verify
   transferred binaries, deployed source identity and every served console asset.

## Required reading

- Main checkout: `AGENTS.md`, `CLAUDE.md`, `.claude/references/stack.md`,
  `.claude/references/conventions.md`, project Go/TUI skills. These are ignored
  and absent from linked worktrees; use main's guidance, not its modified code.
- `docs/client-architecture.md`, `docs/web-console.md`, existing console plans.
- `internal/server/web/console/{api,details,dom,live-reads,workload-details}.mjs`.
- `internal/client/utils/{discovery,registries,models,deployments,provider_status,
  params_client,pricing,update,inference_logs}.go`; `internal/client/logs/`.
- `internal/server/{routes,params_executor,params_patch,update_cluster,
  update_handlers,pricing_handlers,jobs_controller}.go` and actual controllers
  for model/provider actions. Read handlers where client comments have drifted.
- TUI `tui_menu.go`, model/provider install views, key/team forms and Chat.
- Existing JS DOM fixtures, browser fixture/tests and real browser auth tests.

## Verification

Meaningful JS regressions cover exact request shapes and node selection, schema
coercion/null deletion/provenance, edited-field conflict checks, partial outcomes,
accepted-plan binding, sensitive clearing, cancelled/late reads, bounded streamed
events, update interruption, and no automatic resubmission. Go tests cover URL
validation/headless behavior and TUI team/policy round trips.

Browser fixtures exercise actual bundles and every added create/select/edit flow
at 1280x800 and 390x844, plus narrow/short screens. Include keyboard focus, dirty
navigation, disabled/pending actions, unknown outcomes and API errors. Any live
checks are read-only; synthetic operations prove request/response handling, not
real installation or download success. Report that distinction.

Commands: `node --test internal/server/web/console/tests/*.test.mjs`;
`npm --prefix scripts/console-vendor run check` and `run build`;
`CONSOLE_CHROME='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
npm --prefix scripts/console-vendor run test:browser`; affected Go package tests
with `-race`, affected lint/vet, client/server builds and `git diff --check`.
Compare pre-existing failures with the unchanged base instead of weakening tests.

External references: [eventsource-parser](https://github.com/rexxars/eventsource-parser/blob/main/README.md)
for incremental SSE parsing and [WAI disclosure](https://www.w3.org/WAI/ARIA/apg/patterns/disclosure/)
for progressively exposed detail. Reuse pinned dependencies; no framework switch.

## Completion record

- [x] Isolated source and mapped API owners.
- [x] Independent initial architecture review.
- [x] Draft-plan review; exact-tier API gap and action-level parity clarified.
- [x] All matrix workflows implemented and checked against current TUI actions.
- [x] Unit, browser, Go and vet validation complete; baseline lint compared.
- [x] Independent whole-diff reviews and fixes complete.
- [ ] Local commit/deployment/asset verification; net production-line change reported.

Implementation notes and deviations are recorded here as evidence arrives.

Validation before release build: 247 JS tests; vendor TypeScript check and bundle;
12 Chrome journeys with 85 audited desktop/phone states; full server race suite
(268.8 seconds); affected client/CLI/TUI race tests; focused parameter, exact
model-source and confirmation regressions; affected vet and diff checks. Lint
reports the same eight diagnostics as the unchanged base, in pre-existing
server lifecycle/browser-session files and Chat's existing suppression. No new
lint diagnostics were introduced.

Independent reviews found and drove fixes for source-aware model selection,
provider/node handoff, cloud registration removal, command-script execution
context, parameter flag aliases, exact model-detail source binding, worker-cloud
parameter scope and clipped TUI confirmations. Tests include real browser
session/CSRF authorization for parameter merge-patch and management SSE. Browser
installation/download/update operations use synthetic contracts, never real
provider mutations merely for verification.

Net production change: +2,015 lines (2,811 added, 796 removed), excluding tests,
docs, tooling and generated vendors. The increase adds the management workflows
in the acceptance matrix while removing the duplicate TUI team-policy editor.
