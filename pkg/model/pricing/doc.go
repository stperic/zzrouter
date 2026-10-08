// Package pricing owns the model pricing catalog (per-token costs,
// per-request fees, per-provider overrides).
//
// Primary type: [Store] — constructed via [NewStore]; loads pricing
// from disk, refreshes on a background cadence, and serves
// [ModelPricing] lookups.
//
// # Scope
//
// Pricing data only. Spend tracking and quota enforcement live in
// `pkg/access/quota`; this package provides the per-request cost
// function they consume.
//
// # Lifecycle
//
// Coordinator-only. The server's coordinator Start/Stop group calls
// Pricing.Start / Pricing.Stop directly. Workers hold a nil [Store]
// (coordinator-gated feature) and must nil-check.
//
// # Architecture
//
// See docs/package_architecture.md. Two mutexes: cache metadata and
// refresh state. Independent — the refresh goroutine never holds the
// read-serving mutex across I/O.
package pricing
