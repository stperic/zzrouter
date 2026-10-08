// Package network implements mDNS peer discovery and multicast-service
// registration.
//
// Primary surfaces:
//
//   - [NodeDiscovery] — background mDNS listener started via
//     `StartBackgroundDiscovery()` and stopped via
//     `StopBackgroundDiscovery()`. The owned goroutine drains on stop
//     via a cancel-context + done-channel pair (see mdns.go).
//   - Registration side — [Start]/[Stop] publish this node's service
//     record on the multicast group.
//
// # Lifecycle (R4 note)
//
// The non-canonical lifecycle names
// (`StartBackgroundDiscovery`/`StopBackgroundDiscovery`) are retained
// because the type also exposes one-shot `DiscoverNodes` /
// `DiscoverNodesWithContext` calls that are distinct from the
// background loop. Renaming to `Start`/`Stop` would conflate the two
// modes. The contract shape (spawn on Start, drain on Stop) matches
// R4.
//
// # init()
//
// `discovery.go:17` runs `init()` to cache multicast interfaces before
// gopsutil's netlink usage corrupts `net.Interfaces()`. This is a
// load-bearing workaround, not general package-level state. R13
// waiver.
//
// # Architecture
//
// See docs/package_architecture.md.
package network
