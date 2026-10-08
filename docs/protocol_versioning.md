# Cluster protocol compatibility

The current cluster protocol is **7**, with minimum **7**. Coordinators accept
peers only within the compiled minimum/maximum window. Workers advertise their
version. Application semantic versions and cluster protocol versions are separate.

Typed provider install recipes require protocol 7. Model features also require
a compatible provider configuration on every node. Upgrade incompatible peers
in one maintenance window before rejoining them.

## Contributor policy

Change `ClusterProtocolVersion` in [protocol.go](../pkg/version/protocol.go)
when the wire contract changes. Raise both it and `MinClusterProtocolVersion`
when old peers would reject, misread or mis-handle the new bytes. Optional fields
that old peers safely ignore may keep the previous minimum for one release.
A bug fix without a wire change does not require a bump.

Document both old and new values, the contract change and upgrade procedure in
the public changelog. A compatibility window requires explicit support for both
versions in the relevant code and tests.

## Operator procedure

Before a protocol cutover, stop inference, disable automatic updates, ensure
every node is reachable, and retain binary/configuration rollback copies.
Install matching node/launcher bundles on every node. Verify each running
version, protocol, health and a request pinned to that node. If cutover fails,
restore a compatible cluster before resuming inference.

Compatible signed updates can use the serial rollout API. It is not an atomic
all-node cutover or a recovery mechanism for incompatible peers. See
[installation and updates](install.md).
