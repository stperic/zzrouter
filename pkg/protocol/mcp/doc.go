// Package mcp implements the Model Context Protocol transport gateway
// used by zzrouter to bridge to MCP-speaking tools.
//
// Primary type: [StdioBridge] — wraps a child process over stdin/stdout
// with request/response framing and a single write mutex. The bridge's
// one goroutine (at stdio.go:107) is request-scoped: it watches for
// ctx cancellation and closes stdin to unblock a stuck write, then
// exits via a `done` channel when the request completes. No
// long-lived owned goroutines.
//
// # Scope
//
// Transport + framing for MCP. Business logic (tool routing, capability
// negotiation) lives in internal/server; this package only shapes the
// bytes on the wire.
//
// # Architecture
//
// See docs/package_architecture.md. Single mutex on StdioBridge
// serializes writes; `dead` is an atomic flag used to short-circuit
// subsequent calls after an I/O error.
package mcp
