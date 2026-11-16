# Console cleanup

Implement every finding from the 2026-11-15 cleanliness review, preserving the
accepted console capabilities. Work in `feat/console-complete` from `53c1b39`;
the primary checkout contains unrelated concurrent work. No public push or PR.

## Ownership and existing mechanisms

1. Remove the unused TUI Form/Text/Radio framework and unused Section renderer.
   Keep checklist/overlay consumers and move their label helper to the checklist.
2. `details.mjs` remains the editor lifecycle owner. Add a small shared form
   submission lifecycle for existing runtime/provider/pricing/update editors:
   pending, settled, and blocked states; preserve transport-classified refusals,
   uncertain outcomes, secret clearing, and read-after-write inspection. The
   existing panel draft guard owns cancellation through the common editor footer.
   Do not add a form schema framework or duplicate authorization decisions.
3. `workload-details.mjs` owns exact-job observations and cancellation. Provider
   operations reuse that inspector with accepted-node context and a return-to-
   plan action. Provider readiness still requires provider observations. Downloads
   and updates keep their distinct resource contracts and observation owners.
4. Break the two view import cycles. Models owns its navigation; Discovery does
   not import its parent. Move existing provider action plumbing into one module
   consumed by runtime and install views, with job inspection delegated above.
5. The install domain produces a script export or refusal before any plan loses
   its execution context to JSON. Expose this on the existing plan API, retain it
   through coordinator forwarding and the client DTO, and have both UIs consume
   it without reconstructing script rules. Only context-free POSIX command plans
   can export; managed context, hooks, platform, preflight, rollback, environment,
   optional/time-limited/typed steps must stay with the managed installer. Do not
   serialize hidden environment values. Missing export evidence is unavailable.
   The response addition requires protocol 24 with minimum 23: older peers may
   omit the artifact; managed install operations remain unchanged. Document the
   compatible coordinator-first update and no client-side fallback generation.
6. Reuse `OutputData` for CLI structured output, first honoring Cobra's output
   writer and reporting write failures. Expose admitted run endpoints from config
   and remove the server's repeated endpoint list. Separate large controllers'
   concrete rendering/editing responsibilities where those changes touch them;
   do not introduce generic CRUD scaffolding or compress code to lower counts.

## Failure paths and verification

Regression checks cover correctable rejection recovery, blocked uncertain writes,
cancelled draft preservation, exact job/node retention, terminal and missing-job
observations, plan return actions, and script-export refusal for unexported
execution context. Preserve script quoting, per-step shell isolation and first
failure behavior. Exercise both local and routed synthetic plan APIs, the TUI
copy action, and the browser consuming the server artifact verbatim.

Run JS tests, vendor type/build checks and browser journeys; affected Go race
tests, vet/lint, server/client builds, and independent whole-diff reviews. Compare
existing lint failures with the verified unchanged base. Add behavioral tests,
not assertions on documentation or implementation spelling. Record actual net
production lines, including justified increases. Update documentation/changelog.
Locally commit and verify the local installation using existing authorization;
preserve shared source/index state and installed launcher integrity.

## Progress

- [x] Dead widgets removed.
- [x] Shared editor lifecycle and regressions.
- [x] Shared job inspection and acyclic feature modules.
- [x] Planner-owned script export through API and both clients.
- [x] CLI output, endpoint ownership, and controller cleanup.
- [x] Validation, independent review and measured change.
- [x] Local commit, installation and verification.

## Review and verification

Independent Go/TUI and browser reviews found no outstanding issues after fixes.
The browser review caught global activity synchronization overriding a disabled
return-to-plan action. The action now retains independent terminal-job evidence,
and regression coverage exercises running, terminal, pending and missing states.
The TUI copy confirmation now calls the artifact a command script and explains
that managed verification and activation remain required.

All 258 JS tests and 12 Chrome journeys passed. The browser journeys exercise
desktop and phone layouts and synthetic API operations; they do not install
providers or download models on real nodes. Type checking and vendor bundling
passed. Focused Go race tests cover hidden execution context, isolated shell
steps, first failure, stable plan IDs, local/routed plan responses, old-worker
omission, client consumption, TUI refusal handling, CLI writers and endpoints.
All 41 authored console modules have an acyclic import graph.

Full `go vet ./...` passed. Full lint has the same 15 diagnostics as the clean
`e11e3c9` baseline, with no added or removed findings. The earlier scoped lint
count was eight; the full scan also includes existing domain and e2e suppression
diagnostics. Validation did not suppress or change those unrelated findings.
The final provider browser journey also passed after adding running/terminal
return-to-plan assertions against the real console.

Net production reduction: 273 lines (405 added, 678 removed). Using the original
feature's counting scope, its net growth falls from 2,015 to 1,742 lines. Counts
exclude tests, docs, build tooling and generated bundles, and include the
one-line authored event-parser entry point. New domain code owns script-export
rules previously split between clients; additional tests cover failure paths.

The full `go test -race ./...` suite passed, including server, provider domain,
client/TUI, protocol and in-process contract packages. Final client/TUI tests
were rerun after the review wording correction.


## Local release verification

Code commit `aabd3f2` is installed locally as `0.2.3+11151629`. Client and release
server builds passed; the server embeds the unchanged installed launcher digest.
The coordinator restarted with its existing configuration and reports healthy,
protocol 24 and minimum 23. All 73 served console assets match the committed
source byte for byte, with expected content types and response protections.
`zzrouter console --print` resolves `http://localhost:9090/console/#overview`.

Installed binary hashes were verified, rollback copies retained, and the shared
primary checkout's source/index state remained unchanged. No public push or
remote deployment occurred. Browser interactions were verified in isolated
Chrome fixtures; signed-in live installation, download and clipboard operations
were not exercised on real providers merely for validation.
