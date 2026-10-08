// Package logs is the shared client library for the zzrouter run-logs
// feature. It owns all business logic — HTTP, SSE parsing, dedup,
// reconnect, filtering — so that the TUI and CLI presentation layers
// stay thin adapters.
//
// Layering:
//
//	TUI (Bubbletea)  CLI (Cobra)
//	     \               /
//	      \             /
//	       v           v
//	  internal/client/logs  <-- this package
//	             |
//	             v
//	  internal/client/utils (HTTP transport, auth)
//	             |
//	             v
//	     GET /zzrouter/v1/runs/:id/logs
//
// The package exposes:
//
//   - RunFilter + ResolveRuns: list-and-filter runs for the picker.
//   - GetRun: prefix-aware single-run lookup.
//   - FetchTail: one-shot tail of the last N lines.
//   - RunSession: live event stream for a run, owning tail+SSE+dedup
//     +reconnect. Presentation layers consume Events() and call
//     Close() on teardown.
//   - Generic SSE Reader in sse.go, intentionally source-agnostic so a
//     future inference-logs session can reuse it.
//
// This package is UI-agnostic. It must not import Bubbletea, Cobra, or
// anything that writes to stdout/stderr. It must be fully unit-testable
// without a TTY.
package logs
