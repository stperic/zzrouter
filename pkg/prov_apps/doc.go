// Package prov_apps provides unified provider lifecycle management for zzRouter.
//
// It consolidates provider detection, protocol handling, instance management,
// process execution, health monitoring, and installation into a single package
// with well-defined subpackages.
//
// # Subpackages
//
//   - instance: Instance state tracking and registry
//   - port: Per-app port pool allocation
//   - process: Process execution, PID tracking, and orphan detection
//   - health: Health monitoring with Kubernetes-style probes
//   - logging: Instance log management
//   - protocol: API protocol handlers — Ollama, OpenAI
//   - detect: Provider detection and version discovery
//   - install: Install/upgrade/uninstall with guided mode
//
// # Lock Ordering
//
// To prevent deadlocks, locks must be acquired in this order:
//
//	ProviderAppManager.mu → instance.Registry.mu → instance.Instance.mu
//
// Never acquire a higher-level lock while holding a lower-level one.
package prov_apps
