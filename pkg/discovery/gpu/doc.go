// Package gpu inventories attached GPUs across Linux, Darwin, and
// Windows for NVIDIA, AMD, and Apple silicon.
//
// Primary entry: [Inventory] (plus per-OS/per-vendor probes). Build
// tags split per-OS code (`hint_linux.go`, `probe_darwin.go`, etc.);
// per-vendor code is in `nvidia_smi.go`, `rocm_smi.go`,
// `amd_inventory.go`. `live_metrics.go` provides the optional runtime
// utilization sampler.
//
// # Scope
//
// Discovery and metric sampling only. Scheduling GPU workloads onto
// runtimes is owned by [pkg/prov_apps]; capacity accounting lives in
// [pkg/cluster/mesh/resource_tracker].
//
// # R2 Status
//
// ~2,200 LOC spanning a 3-OS × 3-vendor matrix. The size is
// structural, not organic — splitting by vendor would fracture the
// shared `inventory.go` orchestrator and duplicate probe scaffolding.
// The flat layout with build tags is the idiomatic choice; do not
// subpackage without a clear factoring that preserves the
// orchestrator.
//
// # Context
//
// GPU probes use `context.WithTimeout(context.Background(), …)` at
// their roots because callers (CLI detection, one-shot boot inventory)
// have no outer ctx to propagate. This is the R9-allowed "no caller
// ctx, must bound external I/O" pattern; see inline comments at probe
// sites.
//
// # Architecture
//
// See docs/package_architecture.md.
package gpu
