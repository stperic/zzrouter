// Package hardware inventories CPU, memory, disk, and platform
// fingerprints for the local host.
//
// Stateless one-shot probes. Output is consumed by
// [pkg/discovery.Discovery] and — at cluster join — by
// `pkg/cluster/mesh` for capacity advertisement.
//
// # Architecture
//
// See docs/package_architecture.md. No locks, no goroutines.
package hardware
