// Package access is the unified authentication, authorization, and
// enforcement facade for zzrouter.
//
// The root defines [Store], the one boundary for access records, and the
// immutable [Snapshot] it publishes after each commit. Each subpackage owns
// one concern:
//
//   - [control] — the [control.Core] facade that callers use end-to-end
//     (Authenticate + Enforce). Owns post-response reservation settlement
//     and the pending-reservation reaper goroutine.
//   - [keys]    — virtual keys (Argon2id hashing, UUID + lifecycle fields).
//   - [quota]   — RPM/TPM limiters, reservation-based budgets, spend
//     tracking, concurrency limiter ([quota.Enforcer]).
//   - [teams]   — teams and their access policies.
//
// people holds the account state and its transitions without storage;
// filestore implements Store over keys.yaml, teams.yaml and people.json;
// accesstest is the contract every Store passes. Localproof owns the
// installation proof without importing access storage.
//
// # Dependency Direction
//
//	keys   teams   quota   (siblings, no cross-imports)
//	  \      |      /
//	   \     |     /
//	    \    |    /
//	     control  ->  access (root)  <-  filestore
//
// `control` composes the three data-plane subpackages and reads the root's
// Snapshot. None of keys, teams, or quota imports another or the root.
//
// # Lifecycle
//
// `(*control.Core).Start(ctx)` starts the authenticator cache cleaner,
// the pending-reservation reaper (tracked on Core's WaitGroup), and
// the quota enforcer's subsystems. `(*control.Core).Stop(ctx)` is
// idempotent and drains owned goroutines before returning.
//
// # Enforcement Order (from control.Core)
//
// suspended → expired → model access (group-aware intersection) →
// RPM → TPM → budget reservation → concurrency.
//
// Any downstream failure cancels the budget reservation.
//
// # Architecture
//
// See docs/package_architecture.md and docs/audits/access.md.
package access
