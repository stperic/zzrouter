// Package process handles provider subprocess execution, PID tracking,
// orphan detection, and graceful termination.
//
// Primary types:
//
//   - [CommandBuilder] builds argv from a service config
//   - [LaunchResult] wraps a started process with its PID and metadata
//   - [FileMountProcessor] resolves and mounts file arguments
//   - orphan scanner detects provider processes leaked across
//     zzrouter restarts
//
// # Scope
//
// Execution, PID/cgroup bookkeeping, and termination only. Health
// probing lives in [health]; log capture lives in [logging]; install
// lives in [install].
//
// # Architecture
//
// See docs/package_architecture.md.
//
// This package is at 1,758 LOC / 11 files — just under the Rule 2
// subpackage threshold. Monitor growth: if execution and orphan
// detection diverge further, split into
// process/{launch,orphan,terminate}.
//
// # Lock Ordering
//
// One mutex, internal to the orphan scanner. Not reachable from
// callers holding [instance.Instance] or [instance.Registry] locks.
package process
