# Cluster protocol compatibility

The current cluster protocol and its minimum are defined once, in
[protocol.go](../pkg/version/protocol.go); a node reports them as
`cluster_protocol` and `min_cluster_protocol` in `zzrouter-node version --json`.
Coordinators accept peers only within that minimum/maximum window. Workers
advertise their version. Application semantic versions and cluster protocol
versions are separate.

Engine failure declarations, model features and typed provider install recipes require a compatible
provider configuration on every node. Upgrade incompatible peers
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

Every release publishes `release.json` with `version`, `cluster_protocol` and
`min_cluster_protocol`. The release pipeline runs the built linux/amd64 node's
`version --json` command to read its protocol window, then includes the manifest
in `checksums.txt`, authenticated by the same Sigstore bundle as the archives.
The rehearsal script reads the freshly built node the same way. Protocol values
remain owned by [protocol.go](../pkg/version/protocol.go).

The serial rollout API fetches the exact target's manifest and verifies it through
the update verifier before accepting a job. A missing manifest returns HTTP 409
`release_manifest_missing`; verification failures follow the existing update
failure path. It reads every peer's current protocol window, including peers
outside the selected nodes. An unreadable window on a selected node or the
coordinator returns HTTP 503 `node_unreachable` with `Retry-After`. An unselected
peer that cannot be read (a sleeping worker) does not block the rollout: it is
not updated, the rollout lists it in `unchecked_peers`, and the protocol gate
decides whether it rejoins when it reconnects.

A target protocol outside the running coordinator's accepted window, or a target
minimum above an unselected peer's protocol, returns HTTP 409
`protocol_cutover_required`. The detail names the node and protocol values.
Refusal creates no job, changes no rollout history, and sends no update command.
Even selecting every node cannot bypass this gate: workers update before the
coordinator, so a protocol cutover requires the maintenance procedure above.
A peer that becomes unreachable after acceptance can remain pending until it
reconnects.

Compatible signed updates can use the serial rollout API. It is not an atomic
all-node cutover or a recovery mechanism for incompatible peers. See
[installation and updates](install.md).
