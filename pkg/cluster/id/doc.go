// Package clusterid owns cluster identity material and the mTLS CA.
//
// Primary types:
//
//   - [Identity] — this node's stable cluster identity (node ID, cert,
//     private key material)
//   - [CA] — cluster certificate authority: issues peer certs, rotates
//     the signing key, verifies incoming certs
//
// # Scope
//
// Pure identity + CA cryptography. No networking, no state sharing with
// other cluster subpackages. Stdlib-only (by policy — see
// `clusterid.go`); do not import `pkg/utils` or other sibling packages
// from here.
//
// # Architecture
//
// See docs/package_architecture.md. Leaf package: no sibling imports.
// A single mutex on `CA` serializes signing-key rotation; no ordering
// concern because `CA.mu` is never held across an external call.
//
// Note: the directory is `id/` but the package name is `clusterid` —
// grandfathered to avoid stutter at call sites (`clusterid.Identity`
// vs `id.Identity`).
package clusterid
