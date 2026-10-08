// Package modelregistry is the orchestrator for zzrouter's model
// catalog.
//
// Primary type: [Registry] — tracks local + remote models across
// sources (Ollama, HuggingFace, filesystem), drives downloads, tracks
// deployments, and serves the catalog to query/search clients.
//
// # Subpackages
//
//   - [metadata] — shared DTOs for catalog entries (leaf, no state)
//   - [search]   — multi-source search + filter + ranking
//   - [source]   — per-source connectors (filesystem, huggingface,
//     ollama) behind a common interface; each connector is unit-testable
//     without the orchestrator thanks to the AST import guard at
//     `source/internal_import_guard_test.go` (see R6)
//
// # Lifecycle
//
// [NewRegistry] is non-spawning — construction allocates connectors
// and trackers only. There is no `Start(ctx)` because the registry
// launches goroutines only in response to method calls
// (PullOllamaModelAsync, scan triggers); those goroutines are tracked
// on an internal WaitGroup. `(*Registry).Stop()` cancels an internal
// stop signal and waits on the WaitGroup — idempotent.
//
// This asymmetry is deliberate and matches the doc's R4 philosophy:
// construction has no side effects; Stop drains async work. A no-op
// Start would add ceremony without value.
//
// # Lock Ordering
//
// Two root-level mutexes are held by [Registry]:
//
//   - `scanMu` — guards the filesystem scan cache
//   - `stopMu` — guards shutdown state
//
// **These are independent — no code path acquires both.** Callers that
// touch both do so serially, never nested. Subsidiary types
// ([DeploymentTracker], [DownloadTracker]) own internal mutexes that
// never reach back into the registry.
//
// # Observability
//
// The orchestrator itself emits no OTel spans or metrics. Instrumentation
// lives in source connectors (HuggingFace HTTP calls) and at the
// HTTP server boundary that calls into the registry — see
// `pkg/observability`.
//
// # R2 (size) Status
//
// Root is ~7k LOC / 28 files — over the Rule 2 threshold. A real split
// (candidates: download cluster, query cluster) is tracked against
// `docs/audits/modelregistry.md` as a P2 refactor. Do not add new
// top-level responsibilities here without revisiting the split plan.
//
// # Architecture
//
// See docs/package_architecture.md and docs/audits/modelregistry.md.
package modelregistry
