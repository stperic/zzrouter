// Package install handles provider installation, upgrade, verification,
// and rollback.
//
// Per-runtime installers live in [builtins/ollama], [builtins/llamacpp],
// and [builtins/pythonvenv] (vLLM/MLX share the pythonvenv installer
// with different configs). The builtins root package wires them into a
// Dispatcher via builtins.NewDispatcher. Shared machinery lives in
// subpackages: [fsroot] (paths, config, lockfile, rollback, verify,
// Platform), [variant] (OS/GPU-aware variant selection), [download]
// (HTTP fetch + allowlist), [preflight] (Python/GPU/disk/write checks).
// This root package owns only the orchestration: Plan/Step runtime,
// Dispatcher, BaseInstaller, PlanProvider, the sentinel re-exports,
// and the LoadAppsConfig seam consumed by concrete installers.
//
// # Scope
//
// Install/upgrade/uninstall only. Detection (is it already present?)
// lives in [detect]; runtime supervision lives in [process] and
// [health].
//
// # Architecture
//
// See docs/package_architecture.md.
//
// Decomposition per docs/plan_prov_apps_install_split.md is complete:
// [fsroot], [variant], [download], [preflight], and [builtins] (with
// ollama/llamacpp/pythonvenv leaves) are all carved out. Do not add new
// top-level responsibilities here — add them in the appropriate
// subpackage.
//
// # Lock Ordering
//
// Locks in this package are internal to individual installer types
// (lockfile, progress). No lock in this package reaches through to
// locks in [prov_apps] or [instance].
package install
