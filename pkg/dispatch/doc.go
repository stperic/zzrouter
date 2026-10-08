// Package dispatch hosts the per-surface machinery that turns an
// inbound request into an outbound call against the chosen
// provider/node.
//
// Subpackage-only. Each subpackage has a single concern:
//
//   - [adapter]    — [ProtocolAdapter] contract every provider kind
//     (Ollama, OpenAI, cloud relays) implements so the
//     dispatcher can treat them uniformly.
//   - [affinity]   — session-affinity cache ([affinity.Affinity]) that
//     pins a request to the same target within a TTL
//     window.
//   - [chain]      — deployment-chain proxy: resolves the target,
//     plumbs ctx/headers, copies the body, handles
//     streaming, reports usage.
//   - [normalizer] — per-provider response-body + stream normalizers
//     applied during copy so clients see a stable
//     shape.
//   - [wire]       — byte-level helpers (header shaping, framing,
//     usage extraction) invoked by chain during copy.
//
// # Dependency Direction
//
// `chain` composes `adapter`, `normalizer`, `wire`, and (when
// enabled) `affinity`. The leaf packages do not import each other or
// the root.
//
// # Architecture
//
// See docs/package_architecture.md and docs/audits/dispatch.md.
package dispatch
