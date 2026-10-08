# Cluster CA rotation

Rotate the cluster CA after suspected key compromise, loss of the CA key, or
an operator policy decision. Rotation invalidates worker trust and requires
re-pairing every worker. It does not rotate static or virtual client keys.

Before rotation, stop inference, record every paired worker, stop the
coordinator and make a protected configuration backup. Obtain the configuration
directory from your service settings or the [configuration guide](configuration.md).

On the stopped coordinator:

```sh
zzrouter-node cluster rotate-key --confirm
zzrouter-node start
zzrouter-node cluster ca-fingerprint
```

The command requires confirmation, refuses a running node, requires coordinator
CA material and takes a rotation lock. It backs up CA/identity files and
`node.yaml`, clears configured endpoints and prints the re-pair worklist.
Starting the coordinator generates fresh CA and identity material.

Verify the new fingerprint through an independent trusted channel. On each
worker, use its supported reset-and-pair operation against the coordinator's
cluster listener:

```sh
zzrouter-node cluster pair --reset --secure --no-mdns \
  --coordinator-url https://coordinator.example:9091 \
  --ca-fingerprint 'sha256:<verified-fingerprint>'
```

Accept the worker's pairing code on the coordinator:

```sh
zzrouter-node cluster accept <pairing-code>
zzrouter-node cluster list
```

Confirm every required worker is healthy and serves a pinned request before
resuming inference. Securely remove obsolete CA-key backup material after
verification. Backups preserve recovery options but also preserve compromised keys.

## Recovery and exit codes

On failure, inspect the named error and protected `.bak-<timestamp>` siblings.
Restore a consistent configuration/identity set from a known-good backup while
the node is stopped, or finish establishing the new cluster trust. Never mix
old and new CA/identity material. Rollback after compromise requires separately
addressing the original exposure.

| Exit code | Meaning |
| --- | --- |
| 0 | Rotation completed |
| 2 | Confirmation missing |
| 3 | Node is running |
| 4 | Coordinator CA material absent |
| 5 | Concurrent rotation or stale lock |

See the [security model](security.md) for the client and cluster authority boundaries.
