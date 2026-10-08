// Package sync implements model-catalog synchronization between a
// worker node and its coordinator.
//
// Primary type: [Client] — constructed via [NewClient]. Periodic sync
// pulls the authoritative catalog from the coordinator and reports
// local deltas back. [Progress] + [ProgressCallback] surface sync
// state to the caller.
//
// # Naming
//
// The package is declared `package sync` but the directory is
// `pkg/model/sync`. Import sites must alias to avoid shadowing
// stdlib `sync`:
//
//	import modelsync "github.com/stperic/zzrouter/pkg/model/sync"
//
// Every caller in this repo already uses the `modelsync` alias; do
// not add an unaliased import. If the aliasing convention drifts,
// rename the package to `modelsync` rather than let shadowing land.
//
// # Scope
//
// Worker↔coordinator catalog sync only. The catalog itself lives in
// [pkg/modelregistry]; cluster membership lives in [pkg/cluster].
//
// # Architecture
//
// See docs/package_architecture.md. No internal mutexes; concurrency
// is serialized by the single sync goroutine owned by the caller.
package sync
