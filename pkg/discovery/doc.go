// Package discovery detects the local host's hardware, runtimes, and
// network peers.
//
// The root file `discovery.go` is a thin facade that composes the
// subpackages; most logic lives in them:
//
//   - [gpu]      — GPU inventory (NVIDIA/AMD/Apple) across
//     Linux/Darwin/Windows via per-OS + per-vendor probes.
//     Large by necessity (3-OS × 3-vendor matrix).
//   - [hardware] — CPU, memory, disk, platform fingerprinting.
//   - [network]  — mDNS peer discovery and multicast-service
//     registration. Owns a background discovery loop with
//     explicit Start/Stop lifecycle.
//   - [tools]    — runtime-tool detection (installed provider
//     runtimes, CLI helpers) with a definitions registry
//     at `tools/definitions`.
//
// # Scope
//
// One-shot host inspection plus a long-running mDNS listener.
// Supervision of detected runtimes lives in [pkg/prov_apps]; cluster
// membership lives in [pkg/cluster].
//
// # Architecture
//
// See docs/package_architecture.md and docs/audits/discovery.md.
package discovery
