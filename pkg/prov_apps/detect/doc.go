// Package detect answers two specific questions about providers.
//
//  1. [ProviderPython] returns the Python interpreter used to spawn a
//     provider (managed venv first, then Runtime.Execution.Command,
//     then "python3"). The spawn path and any future detection caller
//     MUST go through it so a provider cannot be "installed but not
//     detected" because the two paths resolved different interpreters.
//
//  2. [ProbeOllama] pings an Ollama daemon's /api/version endpoint and
//     returns the version string if one is running. This is the single
//     non-managed provider probe — Ollama is external-by-design and
//     has a stable version endpoint.
//
// For managed installs (MLX, vLLM, llama.cpp) the installed-or-not
// question is answered by [install.IsInstalled] and [install.ReadInstalledVersion],
// which read the marker + version files written at install time. This
// package does not shell out to detect them.
//
// # Scope
//
// Provider python resolution and the one Ollama probe. Lifecycle and
// process supervision live in sibling packages ([process], [health]);
// installation lives in [install].
//
// # Architecture
//
// See docs/package_architecture.md. No package-level state, no
// goroutines, no mutexes.
package detect
