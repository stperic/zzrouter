// Package protocol implements per-provider API adapters.
//
// [Provider] is the common interface each adapter implements. Today:
//
//   - [OllamaProvider] — Ollama native API (/api/*)
//   - [OpenAIProvider] — OpenAI-compatible API (/v1/*) used by vLLM,
//     llama.cpp, MLX, and cloud APIs
//
// A provider holds no endpoint or credential; every call is given the
// [Target] its caller resolved.
//
// Registration is static; new protocols are added by implementing
// [Provider] and wiring them in the registry.
//
// # Scope
//
// Protocol translation and model enumeration only. Lifecycle of the
// upstream runtime is owned by [prov_apps.ProviderAppManager];
// HTTP-level routing lives in internal/server.
//
// # Architecture
//
// See docs/package_architecture.md.
//
// # Lock Ordering
//
// Each provider type holds its own mutex for adapter-local state
// (model list cache, connection reuse). No provider reaches through
// another provider's lock, and none reaches back into
// [prov_apps.ProviderAppManager].
package protocol
