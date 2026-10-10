// Package logging manages per-instance log files with rotation and
// cleanup.
//
// [Manager] owns the filesystem side: path resolution, rotation, cleanup.
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
package logging
