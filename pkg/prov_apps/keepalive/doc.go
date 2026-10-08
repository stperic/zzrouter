// Package keepalive parses keep-alive directives from provider request
// bodies (e.g. Ollama's `keep_alive` field).
//
// [Parse] is the single entry point; it returns nil when no directive
// is present, so callers can use the default TTL.
//
// # Scope
//
// Parsing only. Keep-alive enforcement — extending or shortening an
// instance's idle deadline — is performed by
// [prov_apps.ProviderAppManager] against [instance.Instance].
//
// # Architecture
//
// See docs/package_architecture.md. Stateless, no goroutines, no locks.
package keepalive
