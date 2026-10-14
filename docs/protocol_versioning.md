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

Before each provider-sync round sends bytes to a worker, the coordinator obtains
fresh protocol evidence through the existing mTLS internal version query and
applies its local protocol window. Cached UP status, pairing success and a probe
from an earlier round do not authorize a push. Unreadable, missing, zero or
out-of-window protocol evidence prevents the POST and leaves that worker missed
in the existing partial/pending sync report. Compatible peers still receive full
provider configs, schemas and assets.

A worker can restart on another binary between probe and POST. Receiver-side
strict validation remains the backstop: incompatible bytes are refused before
config, schema or asset writes. This gate does not project newer configurations
into an older schema, and an older coordinator without it can still attempt a
push that the worker refuses.

## Contributor policy

Change `ClusterProtocolVersion` in [protocol.go](../pkg/version/protocol.go)
when the wire contract changes. Raise both it and `MinClusterProtocolVersion`
when old peers would reject, misread or mis-handle the new bytes. Optional fields
that old peers safely ignore may keep the previous minimum for one release.
A bug fix without a wire change does not require a bump.

The ownership receipt and fresh provider-sync evidence gate do not change the
wire contract or require a protocol bump. Future managed provider fields that
older schemas reject still require the normal protocol/minimum change. The
rollback reader floor, v0.2.0, is separate from the protocol window: targets before
it, including public v0.1.0, require the
[manual provider-config recovery procedure](install.md#manual-recovery-for-targets-before-v020).
See [the configuration rollback boundary](install.md#configuration-rollback-compatibility-floor)
for retained operator edits, unavailable providers and the receipt requirement.

Protocol 7 raises the minimum from 6 to 7. Every asynchronous download or peer
sync is admitted with an immutable observable job before physical work starts.
Jobs retain transfer bytes through feature publication and terminal events, with
fractional progress, speed and cancellation metadata applied before emission.
Coordinators retain these outcomes independently of worker download cleanup and
job expiry. Old workers can accept unobservable work, so all peers must upgrade
together using the maintenance procedure below.

Protocol 6 raised the minimum from 5 to 6. Every worker resolves reserved
run parameters `keep-alive` and `evict-idle-on-memory-pressure` through the
provider tier tree and keeps them out of engine argv. Older workers ignore
tree idle settings or pass eviction flags to engines. Upgrade all peers
together using the maintenance procedure below.

Protocol 5 raised the minimum from 4 to 5. Internal model deletion carries an
exact physical name, provider, registry and source id. Every serving node must
guard that target against frozen run reservations and configured variant references,
and serialize deletion/publication with cold starts, queue admission and daemon
adoption. Old workers can ignore these fields or delete held weights. All peers
must upgrade together using the maintenance procedure below.

Protocol 4 raised the minimum from 3 to 4. Internal model show preserves
the selected provider, registry/source id, compact projection and native
uncataloged-read selector. A protocol-3 worker ignores those selectors, which
can select another source/backend or return oversized metadata. Upgrade every
peer together using the maintenance procedure below; this change has no rolling
compatibility window. Public full-detail callers retain their response shape.

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
