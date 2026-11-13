# Console routes and team assignments

Implement the approved route workspace in the existing console worktree. Read
ignored agent guidance from the main checkout; preserve earlier console work.

## Ownership and contracts

- Model groups remain the sole route store and router. Use the existing
  `/zzrouter/v1/model-groups` CRUD, schema, state and preview API. The UI calls
  them Routes. No new route store, ledger or browser selection engine.
- Teams own an explicit `route_access` policy: `direct` retains existing direct
  model/group access; `selected` permits only its exact assigned route names;
  `none` denies inference. Omission retains existing behavior. Selected requires
  a nonempty unique list. PATCH replaces the policy; malformed or null policies
  are refused. Keys inherit the team policy without a new key field.
- Request authorization compares the original route spelling, without model
  identity normalization. Resolved admission also requires the same GroupName,
  so node/provider pins, deleted/expired routes and direct-name collisions cannot
  bypass route-only access. Candidate admission applies existing provider and
  model ceilings. Discovery lists route aliases without exposing direct models.
  Opaque surfaces cannot satisfy route-only access and refuse it. Anonymous
  admission and browser-session revalidation observe the new restriction.
- Team service, persistence, browser identity, audit, SDK DTOs, access schemas
  and OpenAPI all carry the policy. No silent migration of existing teams.
  Unknown assignments are rejected by team writes; a subsequently missing route
  remains denied. The route API refuses deletion when assignments are observed. Concurrent
  assignment, configuration edits and expiry can leave a dangling name; inference
  still fails closed. Recreating the same name restores its grants.
- Preview accepts `team_id`, reads current team policy, reports forbidden
  destinations and budget refusal, and uses shared live strategy/filter and
  quota primitives. Remove the hand-copied preview strategy. Preview never sends
  inference, starts a model, reserves budget, consumes rate limits or records
  routing observations. Its response explicitly describes checks not performed;
  it is a current-state projection, not a guarantee of the next request.

## Console

Routes gets a searchable list, add action and existing right-side panel. The
editor owns name, description, strategy and node/provider/model destinations.
Priority controls work with mouse and keyboard. Preserve all unsupported advanced
route/replica fields when editing. Show automatic/claimed ownership, expiry,
affected teams and cloud destinations. Keep unknown health and price explicit.

Team forms offer Direct models & routes, Assigned routes only and No inference.
Assigned-route choices are searchable with target summaries; saved missing
choices remain visible. Provider/model ceilings and budgets remain on Teams.
Key details preview inherited routes. Route details link to assigned teams and
offer Preview as team using the API. Reuse page lifetimes, detail panels and
pending-write/conflict safeguards, including unknown-outcome recovery.

## Validation

Cover persistence and PATCH semantics, route-name/pin/collision bypasses,
destination ceilings, anonymous/opaque/discovery behavior, policy revalidation,
team preview exclusions and unchanged quota/tracker state. Cover CRUD and team
inheritance in console tests, retained fields/drafts, stale async results and
pending writes. Run affected race suites, console tests, vet, build, lint against
the established baseline, independent architecture and code reviews, and browser
acceptance with actual assets and synthetic records. No live policy changes.

Record results and production-line delta against `/tmp/zzrouter-routes-before`
in the existing console handoff. No commit, merge, push or deployment is implied.
