// Package health runs Kubernetes-style readiness and liveness probes
// against provider instances.
//
// [Monitor] periodically probes every registered instance in the
// [instance.Registry] and updates instance readiness state. Probe
// configuration is carried on each instance's ServiceConfig.
//
// # Scope
//
// Probing and readiness reporting only. Startup, shutdown, and crash
// restart policy live on [prov_apps.ProviderAppManager]; process
// lifecycle lives in [process].
//
// # Architecture
//
// See docs/package_architecture.md. Monitor holds one mutex for its
// internal probe state; it does not reach through [instance.Registry]
// locks while holding its own.
package health
