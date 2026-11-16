# TUI cleanup

Base: `86eb233`, `feat/console-complete`. Work is confined to this worktree.

## Ownership and contracts

- Keep Bubble Tea's Root, List, Detail, Confirm, ViewContext and NavigationGuard. Views own their requests and drafts; commands capture immutable request inputs and replies carry the owner/resource identity. Root continues to own navigation.
- Keep parameter resolution, validation, inheritance, routes and pricing policy in the management API. The parameter editor reads one configured scope plus schema and optional effective evidence. It patches changed leaves only, including null for clearing an override. Do not populate editable overrides from resolved values.
- Use the client's existing context-aware HTTP helpers and Merge-Patch transport. Replace parameter-editor-only compatibility helpers when no callers remain. Keep the model-cell API used by CLI commands.
- Shared chrome owns table width allocation, viewport row budgets and footer fitting. Views declare column importance; narrow terminals keep the resource identity and status. The existing detail view exposes omitted columns.
- Shared form/popup rendering owns frame dimensions and placement. Reuse existing confirmation/navigation mechanisms for pending saves, draft discard and destructive actions. No additional UI framework.

## Implementation

1. Replace the parameter editor's resolved-value save path with scoped configured values, changed-leaf patches and guarded writes. Remove unsupported Validate/Ignore actions. Keep drafts on failed writes; distinguish a successful write from a failed follow-up read. Guard late read/write replies and scope changes. Compare edited leaves with a fresh configured GET before PATCH to detect concurrent changes.
2. Bring pricing and route writes under the existing pending-operation/navigation contract, with bounded contexts and identity-bearing replies. Check model usage and node-detail read identities, including same-resource refresh generations.
3. Correct shared row budgets and table sizing, introduce optional column priorities, and fit footer hints without losing back/help access. Apply to Nodes, Providers, Models and route destinations. Inspect normal, narrow and resized layouts and mouse row coordinates.
4. Protect parameter/key/pricing/route drafts on Back, breadcrumb, menu, replace and push navigation and confirm provider uninstalls. Standardize form navigation/save/cancel hints where touched. Preserve API error details and the last successful provider inventory on refresh failure.
5. Improve provider browsing with local/cloud and node/text filters, explicit disabled visibility, and status in the list. Keep disabled-provider management available. Show route destination nodes.
6. Keep focused fields and save/cancel hints visible in tall forms using the existing Detail viewport. Consolidate duplicated popup framing/sizing and separate cohesive rendering/data responsibilities in oversized touched files. Use readable secondary text colors while preserving the user's chosen terminal brand and theme selection.

## Failure paths and validation

- Test changed settings without freezing inheritance, empty string vs absent, clearing an override, concurrent changes to unrelated leaves, write failure, read-after-write failure, duplicate save and tab/navigation during pending writes.
- Test stale model/node responses after selection, view replacement and refresh; obsolete reads must not overwrite current data.
- Test dirty cancel and confirmation outcomes without executing live mutations.
- Assert visible row and width budgets, retained identity/status columns, resize and selection geometry; inspect a built TUI interactively at 80x24 and 40x10.
- Run focused race tests, repeated concurrency regressions, affected-package vet/lint, and a client build. Run the broader TUI/client/UI suite after integration.
- Obtain independent TUI/Go review of the architecture and final diff; fix findings within the authorized scope. Report net production-line change and remaining live checks.

No server API changes, live provider mutations, service restarts, merge or deployment are needed for this refactor.

## Completed validation

- Affected TUI, client API and UI packages pass `go test -race`.
- New ownership, draft, scope and small-terminal regressions pass 20 consecutive
  runs with the race detector.
- Affected-package `go vet`, repository `go build ./...`, client binary build,
  `git diff --check`, and CI-pinned golangci-lint 2.11.4 with Go 1.26.11 pass.
- Independent final TUI/Go review reports no open findings. Review corrections
  cover immutable reload scope, bounded confirmation footers and retained drafts
  when a route disappears during a background refresh.
- Live read-only PTY checks cover node list/details, enabled provider inventory,
  provider search, shortcut help, scoped parameter reads, and resizing from
  80x24 to 40x10. Provider detail metadata now shares its whole-pane viewport,
  with a regression and live 40x10 check protecting the edit action and footer.
- Production Go is 444 lines smaller, including the new responsibility-based
  file splits. The local client binary is `build/tui-refactor/zzrouter`.

No live configuration was changed. Parameter writes are covered by synthetic
HTTP fixtures; no mutation was sent to the running cluster. Concurrent-edit
checks use a fresh GET and cannot eliminate the interval before PATCH without
an API revision precondition. The refactor review was completed before any
merge, commit, deployment or restart. The subsequent delivery step integrates
the reviewed branch, updates only the installed client and checks live workflows
without restarting the coordinator.

## Delivery: 2026-11-16

- Committed the reviewed refactor as `7fd2e63` and integrated the reviewed
  console history through `6f75483` on local `main`. No public push was made.
- Installed client `0.2.3+11161312`, built from `6f75483`. Its SHA256 matches
  the tested release candidate. Retained a verified copy of the previous
  client in the integration worktree's `build/tui-ship/zzrouter-before`.
- Updated only the client. The coordinator and launcher binaries, coordinator
  PID and process start time are unchanged.
- The integration passes `go mod verify`, `go build ./...`, `go vet ./...`,
  `go test -race -timeout 15m ./...`, and CI-pinned golangci-lint 2.11.4.
  Console validation passes all 258 unit tests and TypeScript checking, and
  rebuilding its assets produces no changes. Twelve browser cases passed on
  the initial run; the theme case failed a graph-pan equality assertion once
  and passed its focused recheck without source changes.
- Live release-candidate checks cover local/cloud provider filters and search,
  worker-scoped parameter reads and edits, route details and edits, key policy
  details and edits, and pricing entry. Changed drafts were discarded, and
  reopening parameters confirmed the original inherited value. No live save,
  provider mutation or key creation was performed; write paths are covered by
  the automated HTTP fixtures. Key form and discard controls remained usable
  at 40x10, although long field values are clipped at that width.
- The installed client was launched again to confirm provider browsing and
  the TUI's Open web console action. Browser launch returned successfully, the
  discovered URL contained no credentials, and `/console/` returned HTTP 200.
  The browser-control host was unavailable, so the resulting live browser page
  was not visually inspected.
- The final preservation check matches the shared checkout's original branch,
  base commit, index checksum and all 710 status entries. Local `main` had been
  a symbolic alias of the shared work branch. The references are now separate;
  the existing shared checkouts retain their original work branch and base,
  and their indexes were preserved. The clean main worktree holds the integration.
