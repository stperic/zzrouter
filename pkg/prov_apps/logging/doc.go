// Package logging manages per-instance log files with rotation and
// cleanup.
//
// [Manager] owns the filesystem side (path resolution, rotation,
// cleanup); [Writer] wraps an io.Writer for a single running instance.
//
// # Scope
//
// Instance log file management only. The structured logger used by the
// rest of zzrouter lives in `pkg/observability/logger` — do not
// conflate the two. This package writes provider stdout/stderr to
// files; it does not emit application logs.
//
// # Architecture
//
// See docs/package_architecture.md.
//
// # Lock Ordering
//
// Manager holds one mutex for its path index; each Writer holds one
// mutex for its rotation state. A caller never holds both: Manager
// hands a Writer to the caller and does not reach back through it.
package logging
