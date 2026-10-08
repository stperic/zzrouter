// Package port manages per-provider TCP port allocation.
//
// [Pool] hands out ports from a configured [Range] for a single
// provider, remembers allocations, and releases them on instance
// shutdown. Each provider runtime has its own Pool (vLLM, llama.cpp,
// ...); they share no state.
//
// # Scope
//
// Port bookkeeping only. Actual listen/bind happens in the spawned
// provider process; this package just reserves numbers and tracks
// whether a port is currently listening (for conflict detection on
// restart).
//
// # Architecture
//
// See docs/package_architecture.md.
//
// # Lock Ordering
//
// Pool holds two mutexes: one for allocation state, one for the
// listen-check cache. Allocation lock is acquired first; the
// listen-check lock is never held across allocation calls.
package port
