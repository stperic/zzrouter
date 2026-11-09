# Cluster networking

The coordinator initiates every cluster TCP connection to workers. Workers accept authenticated requests and return responses, including updates through held long polls. Worker-to-coordinator and worker-to-worker connections are unnecessary. External model and provider downloads still need their declared upstreams.

Allow coordinator egress and worker ingress on each worker's configured cluster port (9091 by default), with established return traffic. Worker public administration stays on loopback. Existing HTTP inference and job streams use the same coordinator-initiated mTLS transport.

The coordinator holds a typed operation poll for up to 25 seconds. Catalog publication, provider enable choices, departure and certificate renewal share this exchange. Operation IDs and node incarnations make reconnect replay idempotent within a running coordinator generation. A coordinator restart may repeat an unacknowledged operation; these domain operations are idempotent. Separate bounded acknowledgments report actual completion. Catalog success requires refreshed coordinator inventory, rather than delivery alone. Transient disconnects retry with bounded backoff.

## Enrollment

On the worker, run:

```sh
zzrouter-node cluster pair --coordinator-url https://COORDINATOR:9091
```

It opens a code-protected TLS enrollment listener for 15 minutes and prints a complete acceptance command with the worker URL and its public-key fingerprint. Carry that command to the coordinator:

```sh
zzrouter-node cluster accept CODE --worker-url https://WORKER:9091 --worker-fingerprint sha256:WORKER_PUBLIC_KEY_HASH
```

The coordinator pins the worker key, obtains and validates its CSR, delivers the signed identity and confirms it over normal mTLS before admission. The worker never contacts the supplied coordinator URL; it records that URL as enrollment intent and owner identity. Use `--secure --ca-fingerprint sha256:COORDINATOR_CA_HASH` on the worker to require a coordinator CA obtained out of band. A retry after committed certificate delivery confirms the installed identity. Regeneration, cancellation and expiry close the bootstrap listener. Re-enrollment preserves the long-term key, including after certificate expiry.

The admin API uses `POST /zzrouter/v1/cluster/pair` on the worker and `POST /zzrouter/v1/cluster/pairing/accept` on the coordinator. Acceptance requires `code`, `worker_url` and `worker_fingerprint`. There is no coordinator pending-pairing inbox.

## Model replication

The coordinator reads a manifest and file stream from the source and streams checked multipart bytes to the destination. Coordinator-local destinations use the existing pull materializer. Worker destinations receive uploads and never dial a source. Both paths share plan validation, materialization locks, partial-file handling, checksums and receipts.

An upload acknowledgment confirms checked byte receipt. The existing detached sync job remains authoritative for feature setup, catalog publication and terminal success. No-byte stalls and cancellation stop both relay legs without imposing a total duration on progressing large files. A reused accepted job does not start a second upload.

## Upgrade

Protocol 19 requires a coordinated upgrade of coordinator and workers. Protocol 18 cannot join this topology; there is no reverse-connection fallback. Preserve identity keys, the CA and existing pairing state during upgrade. Follow the [protocol maintenance procedure](protocol_versioning.md#operator-procedure).
