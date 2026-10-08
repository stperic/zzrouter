// Package search implements multi-source model search, filtering, and
// ranking on top of the [modelregistry] catalog.
//
// The package is stateless from the caller's perspective — each search
// invocation builds its own query plan, fans out to registered
// sources, and ranks the union. Internal caches (by source type) are
// guarded by independent mutexes; none is held across a caller call.
//
// # Scope
//
// Query, filter, rank, enrich. Catalog ownership (what models exist)
// lives in [modelregistry.Registry]; per-source connectors live in
// [modelregistry/source]. This package does not write to the catalog.
//
// # Lock Ordering
//
// Three independent internal mutexes — no nested acquisition. If a
// new code path requires holding two, declare the order here first.
//
// # Architecture
//
// See docs/package_architecture.md and docs/audits/modelregistry.md.
package search
