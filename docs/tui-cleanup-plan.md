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
