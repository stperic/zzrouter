// Package resolver resolves a requested model name to a concrete
// [Resolved] target (node + provider + model).
//
// Two implementations:
//
//   - [Default] — direct name lookup via a [RemoteLookup] closure
//   - [Group] — resolves through a [group.GroupStore] first, falling
//     back to an inner [Resolver]
//
// Both are stateless once constructed; composition yields the
// "try group, then direct" chain used by request dispatch.
//
// # Scope
//
// Name → target resolution only. Health-aware selection, fallback
// chains, and circuit breaking live in sibling packages
// (`pkg/fallback`, `pkg/cluster/mesh`).
//
// # Architecture
//
// See docs/package_architecture.md. No mutexes, no goroutines.
package resolver
