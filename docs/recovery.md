# Installation backup and recovery

Use **System → Recovery** to review the running coordinator's scope. The local
`zzrouter-node recovery` commands work on one installation at a time. Back up each
worker separately. They do not stop services, overwrite installations, restore
privileged service files or activate recovered state.

## Before a backup

Start the installation once with this release. The node records its actual
configuration, data and provider roots in `recovery-layout.json`. A later backup
uses these roots and current provider declarations, not the invoking operator's
account-relative defaults. This matters for Linux service accounts and Windows
LocalSystem installations. `start --dir` now selects the same configuration root
for provider configuration, credentials, policy and cluster identity.

If provider roots, declared provider data or model paths change, backup refuses
the stale inventory. Start the node with the intended configuration, inspect the
new scope, then stop it for backup. Relative model paths retain the daemon
account’s resolution. A failed inventory check does not create installation
state. A previously recorded source becoming unavailable also stops the backup, including an unmounted
provider volume with unchanged configuration. Never-created optional state is
listed in the manifest’s `missing_optional` field and counted in CLI output.
Inspect that list, especially after installing a provider since the last startup.

Run maintenance as the installation owner or an administrator who can read its
protected files. Finish other installer/configuration work and stop the node and
managed providers first. Node startup and backup share an exclusive installation
lock. Independently managed providers and external writers must also be quiesced.
Do not remove lock files to bypass an ownership refusal.

Inspect the scope before stopping, or locally afterward:

```sh
zzrouter-node recovery inventory --dir /path/to/installation
```

On macOS, the usual directory is `~/Library/Application Support/zzrouter`.
Linux system installations normally use `/etc/zzrouter`; Windows services use
`C:\ProgramData\zzrouter`. Use the exact path shown by your console. Follow the
existing [service instructions](install.md#supervised-services) to stop and start it.

## Included and separate state

The archive includes configuration and provider definitions/assets/ownership
receipts, applicable configuration-directory dotenv files, the access database
(`access/access.sqlite`: people and grants, app keys, teams and the access
history) with its store marker, OAuth authority, routes/model groups, spend,
pricing overrides, cluster identity and trust including revocations, encrypted
provider connections, node pins/download state and provider install metadata. An
installation that ran an earlier development build may still hold `keys.yaml`,
`teams.yaml`, `access/people.json`, `access/people.json.lock` and `audit.jsonl`.
Nothing reads them any more, but they hold key and password hashes and travel in
every backup: delete them. Declared
provider data such as workflows and uploads is included; declared model stores
are excluded and listed separately.

Model weights/caches, provider executables and environments, service definitions,
the binary installation and privileged install policy require separate recovery.
Keep the matching release available. Also preserve external TLS certificates,
proxy trust, custom environment settings, symlink targets, container volumes and
independently managed provider data. The inventory lists known paths but cannot
discover every dependency of an arbitrary external service or command. Paths
within each recorded root cannot traverse links, including provider parent
directories. Special files are also refused. Installation roots themselves
are the operator-selected boundaries; back up linked external data separately.

Pairing codes, local owner-recovery proof, lock/PID/temp files, caches and pending
update requests, pending update confirmation, executable rollback backups, and
an access database journal or unpublished staged store are excluded. Before it
copies anything, backup finishes SQLite's recovery of a journal a crash left
beside the access database, so the copy holds exactly the committed
transactions. A database it cannot read stops the backup with the store's
`unreadable` reason; one a newer release wrote is settled and backed up. A node creates fresh transient state after startup.
Archives are limited to 20,000 files and 4 GiB; they are recovery state bundles,
not model-library or complete operating-system images.

## Back up and verify

Choose new paths in secure backup storage. Keep the key in a separate directory
and separate backup custody. These files contain secrets: use encrypted storage
and restrictive access. The archive itself is not encrypted or publisher-signed.

```sh
zzrouter-node recovery backup --dir /path/to/installation \
  --output /secure-backups/router.tar.gz \
  --key-output /separate-key-storage/router.key
zzrouter-node recovery check /secure-backups/router.tar.gz \
  --key-file /separate-key-storage/router.key
```

The same flags work in PowerShell; enter the command on one line with quoted
Windows paths. Existing output files are refused. The key export is required when
the installation has encrypted connections. It is never packed in the archive.
A failed export can leave a protected partial output; do not use it unless a
subsequent check succeeds. Keep a previous verified backup until the new one passes.

Checking verifies bounded archive structure, paths, file hashes, spend snapshots,
trust key/certificate pairs and encrypted connections with the matching key.
Until backups capture the access database as a checked image, `recovery check`
validates no access records: the database travels as an ordinary file, and the
restored coordinator checks it when it starts, refusing to start with a reason
if it fails. A restored store marker without its
database refuses with `missing`. It does not authenticate the archive's author or prove remote
reachability, provider compatibility or successful inference. Use only trusted
backups. Checks do not prune expired grants or turn corrupt spend into zero usage.
Other configuration and provider assets are preserved byte for byte; the matching
release validates their operational compatibility when the recovered node starts.

## Restore and activate deliberately

```sh
zzrouter-node recovery restore /secure-backups/router.tar.gz \
  --key-file /separate-key-storage/router.key \
  --to /new/private/restore-staging
```

The destination must not exist. Restore checks the archive before writing, then
verifies the extracted bytes again. An interrupted extraction leaves `INCOMPLETE`;
do not activate it. A successful staging directory contains `manifest.json`,
`config/`, any separate `data/` and `providers/` content, and `protection/` when a
key was supplied. Inspect the manifest's original roots before moving anything.

Keep the target node stopped. Install the matching release and service account,
restore configuration and data to the intended roots, and set their ownership for
that account. Reinstall provider executables and recover separate model/external
data. Restore the protection key to `config/private/credentials.key`, or to the
explicit `security.credentials.key_file` location. Unix keys require owner-only
permissions; Windows keys must be owned and readable under the intended service
identity with the restricted ACL. Do not loosen permissions to bypass validation.

Staging is intentionally separate from activation: configuration and provider data
can live on different filesystems, and replacing them is not one atomic operation.
Do not point a running installation at partially recovered state. Preserve the
existing installation until recovery has been qualified.

Start the node and verify owner sign-in, a team policy and app key, recorded spend,
provider connections, worker trust and one real request. Recover the owner through
the existing local recovery command if necessary. Expired worker certificates can
require pairing again. An older valid backup can restore access that was revoked
later; review people, keys, grants and budget state after recovery. A successful
archive check is not a successful activation test.
