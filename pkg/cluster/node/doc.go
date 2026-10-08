// Package node implements the local-node side of cluster membership.
//
// Primary type: [Node] — the running node's identity, current [Mode]
// (single / coordinator / worker), pairing state, and role-claim
// lifecycle. Constructed via [New]; lifecycle via
// `(*Node).Start(ctx) error` and `(*Node).Stop(ctx) error`.
//
// # Scope
//
// Local node runtime: pairing handshake with coordinators, mTLS
// identity bootstrap, role claim/renewal, pairing store (TTL-backed).
// Peer mesh state and dispatch live in [pkg/cluster/mesh]; role state
// machine transitions live in [pkg/cluster/role].
//
// # Lifecycle and Goroutines
//
// `Start` spawns pairing-window watchers, role renewal loops, and
// pending-pairing expirers. All owned goroutines derive from the
// `startCtx` captured by `Start`; `Stop` cancels that context and
// waits on the owning WaitGroup with `stopCtx` as a deadline.
//
// A pre-existing self-deadlock precondition is documented at
// `lifecycle.go:500` — read it before adding new lock-holding code
// paths through role transitions.
//
// # Lock Ordering
//
// **Traced — see docs/audits/cluster_lock_graph.md.** Six struct-field
// mutexes live on [Node] (mu, pairingMu), denyList.mu (embedded on
// Node), [PairingStore].mu, pairingRateLimiter.mu, and per-IP
// tokenBucket.mu.
//
// Three independent lock families (no cross-family nesting observed):
//
//  1. Node lifecycle: [Node].mu and [Node].pairingMu. Acquired
//     sequentially (mu → release → pairingMu), never nested. Node.mu
//     guards lifecycle state (started, stopped, httpServer, listener);
//     Node.pairingMu guards the active pairing window.
//  2. Pairing rate limiter: pairingRateLimiter.mu → tokenBucket.mu.
//     One confirmed nesting at pairing_ratelimit.go:145, where
//     gcLocked() sweeps per-IP buckets and takes tokenBucket.mu for
//     a single-field read under the outer limiter lock. Benign by
//     construction (inner bucket never reaches back).
//  3. denyList.mu (embedded) and [PairingStore].mu: each independent,
//     neither nested with any other mutex in this package.
//
// Lock discipline (do not regress):
//
//   - Do not acquire [Node].mu while holding [PairingStore].mu, and
//     vice versa.
//   - Do not hold any node mutex across a role-manager callback.
//     notifyModeChange is called after n.mu.Unlock (see
//     lifecycle.go:480-482).
//   - Functions with a `-Locked` suffix (startRenewalTickerLocked,
//     cancelWindowLocked, gcLocked) assume the caller already holds
//     the relevant mutex and MUST NOT re-acquire it. See
//     lifecycle.go:500 for the self-deadlock that pattern was
//     introduced to prevent.
//   - Add a lock-order comment at any new nested Lock() call and
//     update docs/audits/cluster_lock_graph.md.
//
// # Architecture
//
// See docs/package_architecture.md and docs/audits/cluster.md.
package clusternode
