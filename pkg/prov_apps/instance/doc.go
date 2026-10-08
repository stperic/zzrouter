// Package instance owns the in-memory registry of running provider
// instances.
//
// [Registry] is the authoritative map of instance ID → [Instance]. Each
// [Instance] tracks status transitions, active requests, keep-alive
// deadline, and log buffer for one running provider process.
//
// # Scope
//
// State tracking only — creating, looking up, removing instances; no
// process supervision, no probing, no lifecycle side effects. Those
// live on [prov_apps.ProviderAppManager] and the [process] / [health]
// subpackages.
//
// # Architecture
//
// See docs/package_architecture.md.
//
// # Lock Ordering
//
// Two mutexes in play:
//
//	Registry.mu  →  Instance.mu
//
// Callers that iterate the registry under Registry.mu may call
// Instance methods that take Instance.mu. Never acquire Registry.mu
// while already holding an Instance.mu. This matches the root ordering
// declared in pkg/prov_apps/doc.go:
// ProviderAppManager.mu → Registry.mu → Instance.mu.
package instance
