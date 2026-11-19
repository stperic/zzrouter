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

### The access store section

`recovery inventory` prints `{"inventory": ..., "store": ...}`. The `store`
section reports the access database as the check a backup runs finds it. It is
worked out on each run and saved nowhere, neither in `recovery-layout.json` nor
in any manifest. The store is inspected even when the installation section
cannot be read: a store refused at the first start stops the node before it
records `recovery-layout.json`, so the command then prints `"inventory": null`
with the store section and exits with an error. The section prints no secret,
and no path except in an `unknown` state's `evidence` and in `archives_error`,
which are error texts.

| `state` | Meaning | What to do |
| --- | --- | --- |
| `ready` | The database passes the check a backup runs and an owner exists. | Nothing. |
| `setup_required` | The database passes the check a backup runs; nobody has claimed the owner account yet. | Start the node and finish owner setup. |
| `absent` | Neither a database nor a store marker: a worker, or a node that never ran as a coordinator. A coordinator start creates an empty store, at owner setup. | Nothing on a worker. |
| `in_use` | The installation lock is held, by a running node or another recovery command, so the inventory opened neither the database nor its journal. | On a coordinator, read the store's state from `GET /zzrouter/v1/system/state`; a worker has no store. Or stop the service, or let the other command finish, and run the inventory again. |
| `refused` | The database fails the check a backup runs, for `reason`: `missing` (the store marker exists and the database is gone), `schema_newer` (a newer release wrote it), `integrity_failed` (it opens but fails its checks) or `unreadable` (it cannot be opened). A start refuses for the same reasons, except that it runs SQLite's quick check and can still open a store whose only damage is in an index. `evidence` is the reason with the store's phrase for a cause it tells apart, such as `integrity_failed (its schema objects differ from this release's)` for an index, trigger or `ANALYZE` table another tool added. `next_action` is the text the node logs when it refuses to start. | Follow `next_action`; each one starts with stopping the service. |
| `unknown` | The inventory could not judge the store: for example, the account cannot open or write the installation lock (a held lock is `in_use` instead), or the system's temporary directory is not writable or full. `evidence` is the error. The command exits with an error. | Resolve the cause in `evidence` and run the inventory again. |

When it can read the store's identity, the section carries `store_id`,
`generation`, `schema_version`, `revision` and `sequence`, also for
`schema_newer`, since every release keeps that table's layout. With the node
stopped, the inventory takes the installation lock (only when its file exists,
so it never creates one owned by your account), copies the database into a
private directory in the system's temporary directory and checks the copy with
SQLite's full integrity check, so the live database is never migrated. It
writes to the installation in one case: when a crash left a journal beside a
stopped database, copying it first recovers that journal into the live file,
exactly as the next start would.

`archives` lists the [online backups](#online-backups-of-the-access-database)
in the configuration's `access/backups` directory, newest first and then by
`id`: each archive's `id` (its file is `<id>.tar.gz`, the ID a UUID),
`created_at`, `size_bytes`, the store identity its manifest names, and
`readable`. It reads that directory exactly as a starting coordinator does.
`readable` says only what the manifest states: a `database` bundle at the
schema version this release writes. It is not a check, since the inventory
reads nothing but the manifest; `recovery check` checks an archive, so run it
before you restore one. A file named like an archive that is not one is listed
as `readable: false` and aged by its modification time. Other files (the
temporary files of an interrupted backup, files under other names), links,
directories and other special files are not listed, and the inventory never
follows a link and deletes nothing. A coordinator lists only the database
archives among them, and when it starts deletes only the temporary files an
interrupted backup left there. While the lock is held (`in_use`) the
list is empty, because a running node owns that directory and its status lists
them. Archives that cannot be listed, for example because a link stands on the
way to `access/backups`, leave the store's state as it is: `archives` is empty,
`archives_error` holds the error, and the command exits with an error.

## Included and separate state

The archive includes configuration and provider definitions/assets/ownership
receipts, applicable configuration-directory dotenv files, an image of the access
database (people and grants, app keys, teams and the access history) with its
store marker, OAuth authority, routes/model groups, spend, pricing overrides,
cluster identity and trust including revocations, encrypted provider connections,
node pins/download state and provider install metadata. An installation that ran
an earlier development build may still hold `keys.yaml`, `teams.yaml`,
`access/people.json`, `access/people.json.lock` and `audit.jsonl`. Nothing reads
them any more, but they hold key and password hashes, and the first two and the
last travel in every backup: delete them. Declared
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

Pairing codes, local owner-recovery proof, lock/PID/temp files, caches and
pending update requests, pending update confirmation and executable rollback
backups are excluded. A node creates fresh transient state after startup. The
configuration's `access/` directory is never copied as files: the live database,
any journal, an unpublished staged store and online backups stay out of every
bundle, and restore refuses a bundle that carries an entry there. Instead, backup
takes a consistent copy of the stopped access database (SQLite's `VACUUM INTO`)
into a private directory beside the archive, checks it as a coordinator start
would, with SQLite's full integrity check, and packs it as `store/image.sqlite`.
It first finishes SQLite's recovery of a journal a crash left beside the
database, so the image holds exactly the committed transactions; that recovery is
the only change backup makes to the database. The manifest's `store` object names
the image's store ID, generation, schema version, revision and sequence.

A database that will not open stops the backup with the store's reason
(`unreadable`, `integrity_failed`, or `missing` for a store marker without its
database) and its next action, and writes nothing; copy a damaged database by
hand if you need it. A database a newer release wrote is copied byte for byte:
this release cannot check or restore it, `recovery backup` says so, and the
release that wrote it reads the archive. A worker, or a node that never ran as a
coordinator, has no access store, and its bundle carries no image and no `store`.

Archives are limited to 20,000 files and 4 GiB; they are recovery state bundles,
not model-library or complete operating-system images.

## Bundle profiles

Every archive names a `profile` in its manifest. The three profiles share one
manifest format, one check and one staged restore. A profile this release does not
know is refused.

| Profile | What it carries | What it is for |
| --- | --- | --- |
| `installation` | The scope above, with an `inventory` of the installation's recorded roots. | Recovering this installation. `recovery backup` writes it by default. |
| `database` | The access database image alone: no configuration, no key, no inventory. | Restoring the access database into the installation it came from. `recovery backup` does not write it. |
| `handoff` (experimental) | The access database image, the provider tree's definitions and assets, routes, spend, pricing overrides and encrypted provider connections, with the old coordinator's `source` and a `separate` list. Nothing that belongs to the old machine. | Moving the coordinator to another machine. `recovery backup --profile handoff` writes it. See [Coordinator handoff](#coordinator-handoff-experimental). |

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
the installation has encrypted connections. It is never packed in the archive. A
failed backup leaves neither the archive nor the exported key. If the check that
`recovery backup` runs afterwards fails, it says so and the archive stays: do not
use it. If that check is interrupted, the archive and key are already written but
unchecked: the command says so and fails; run `zzrouter-node recovery check` on
them before relying on them. Keep a previous verified backup until the new one
passes. Each command names the bundle's profile and its access store's ID,
generation, schema version, revision and sequence.

`recovery backup` keeps its temporaries in private directories beside the archive
and the key, named after them with a `.tmp` suffix (for example
`router.tar.gz-1234.tmp`), and moves the key and then the archive into place
last. `recovery check` extracts the access database image into a private
directory in the system's temporary directory (`TMPDIR`; `TMP` on Windows), so
the archive can sit on read-only media; that directory needs room for the whole
access database, so point `TMPDIR` at a disk with space when the default is small
or held in memory. `recovery restore` keeps its temporaries inside the staging
directory. An interrupt (Ctrl-C) or a service stop cancels a command, which
removes its temporaries and exits with an error. A process killed outright can
leave a `.tmp` directory or file behind: delete it. The commands refuse a `.tmp`
file as an archive or a key.

Checking verifies bounded archive structure, paths, file hashes, spend snapshots,
trust key/certificate pairs and encrypted connections with the matching key. It
streams the access database image to a private temporary file, never into memory,
and runs the checks a coordinator start runs on it, with SQLite's full integrity
check: foreign keys, every access record against the access rules, the schema
version this release writes, and schema objects (tables, indexes, triggers,
views) exactly as this release creates them, so an archive cannot carry an extra
trigger or view into a store. The image's own store ID, generation, schema
version, revision and sequence must equal the manifest's `store` object. In an
`installation` bundle the store marker must name the image's store ID, and a
marker without an image is refused, since the restored node would stop with
`missing`. A refused image is reported as the archive's ("the archive's access
database image is refused" and the reason) with advice about the archive: do not
use it, take another backup or use another archive; for `schema_newer`, only the
release that wrote it can check or restore it. Checking does not authenticate the
archive's author or prove remote reachability, provider compatibility or
successful inference. Use only trusted backups. Checks do not prune expired
grants or turn corrupt spend into zero usage. Other configuration and provider
assets are preserved byte for byte; the matching release validates their
operational compatibility when the recovered node starts.

## Online backups of the access database

A running coordinator backs up its access database on request:
`POST /zzrouter/v1/system/state/backups` with an empty JSON object, as an
administrator (the API or `zz_api`) or, from the installed host, with the local
proof in the `X-Zzrouter-Local-Proof` header. It copies the database at one
committed revision with SQLite's `VACUUM INTO` while the node serves, checks the
copy as a start would, with SQLite's full integrity check, writes it as a
`database` bundle (the image and a manifest naming its store ID, generation,
schema version, revision, sequence and creation time), records one
`store.backup_created` event in the access history and only then lists it.
`GET /zzrouter/v1/system/state/backups` lists the retained backups and
`GET /zzrouter/v1/system/state/backups/{id}` reads one, with the SHA-256 of its
archive, `protected_until` and `expires_at`; `GET /zzrouter/v1/system/state`
counts them in `backup_bytes` and `retained_copies`. No answer names a path.

An online backup is the access database alone: people and password hashes,
app keys and their hashes, teams, invitations and the access history. It is not
an installation backup. It holds no configuration, routes, spend, provider
connections, OAuth authority or cluster trust, so moving a coordinator to
another machine needs the installation bundle that `recovery backup` writes. It
is also not a copy elsewhere: the archives live in the configuration's
`access/backups` directory, owner-only, beside the database, and retention
deletes them. They never travel in an installation bundle.

To take a copy off the running coordinator, run on its host, as the
installation account:

```sh
zzrouter access state export --output /secure-backups/access.tar.gz
zzrouter-node recovery check /secure-backups/access.tar.gz
```

Without `--backup-id ID`, the export takes a new backup first; with it, it
copies that retained backup. The command reads the installation's local proof,
so only an operator on that host can export, and `--dir` selects another
installation. The node checks the archive on the file it sends from, records
one `store.backup_exported` event in the access history before the first byte,
and sends the whole archive. The command refuses an output that exists or lies
inside the installation, writes `FILE.partial` owner-only, checks the SHA-256
the node announced, and only then renames it; a mismatch or an interrupt
(Ctrl-C) removes the partial file. If the node refuses the archive as damaged,
take a new backup and export that one; the live database is not involved. The copy holds the same credential
hashes as the backup, outside retention: it is yours to protect and to delete.

A failed or interrupted backup lists nothing and leaves no file. A copy that
fails its check answers `500 state_image_check_failed`: the live database may be
damaged, so stop the service and run `recovery inventory`. The same
`Idempotency-Key` and body replay the original `201` while the coordinator
process lives; a retry after a restart creates a second backup. The
`store.backup_created` event is committed before the archive is published, so a
listed backup always has its event; if the process dies after the event, or
publishing the archive then fails, the history holds an event for a backup that
was never listed, and nothing of it remains on disk once the next start has run.

Every backup is kept for at least the 24-hour replay window
(`protected_until`), so a replayed create never names a backup that is gone.
Past it, the three newest backups are kept, and none from seven days on. Pruning
runs at start, after each backup and hourly; on Windows a file another program
holds open is removed at a later pruning. Retained backups may hold at most
1 GiB, counting only what retention keeps: a create is refused with
`507 state_backup_capacity_exceeded` before anything is copied when they already
fill it, and after the archive is written when it does not fit beside them. Wait
for retention to remove older backups (each one's `expires_at`), or take an
offline `recovery backup` with the node stopped. A full disk answers the same
code with `capacity: disk`.

A backup holds access writes (key, team and people changes, console sign-ins)
only while it copies the database. On an Apple M5 Max laptop SSD that took
about 90 ms for a 29 MiB database (100,000 access-history events) and 0.7 s for
a 297 MiB one (1,000,000 events), and the whole backup 0.6 s and 6.8 s.
Inference never writes to the access history, so it grows with administration,
not with traffic.

To restore an online backup today, export it, stop the node and stage the copy
with `recovery restore`, then place the prepared image as
the next section describes for a staged `database` bundle. A command that
activates it is a later change.

## Reset access state

A reset returns the access records to the owner alone, on the running
coordinator, without touching configuration, providers, routes or the static
installation keys. Reset when the records are the problem but the installation
is sound: leaked or unknown app keys, people and teams nobody can account for,
or a coordinator handed to someone new. Restore a backup instead when you want
an earlier state of the records back.

Run on the coordinator's host, as the installation account:

```sh
zzrouter access state reset
```

The command takes an online backup (or names the retained one given with
`--backup-id ID`), previews the reset against it, prints what goes and what
stays, asks for confirmation (`--yes` skips the question) and resets. A reset
removes every app key, every team but the owner's, every person but the owner,
every grant (invitations and setup, sign-in and recovery codes) and the
onboarding progress, in one transaction with one `store.reset` event in the access history and a stored
receipt. What survives:

- the owner, with their ID, name and password, under a new credential revision:
  their console sessions and delegated grants stop resolving, so they sign in
  again with their password, or with `zzrouter access recover-owner` if it is
  lost;
- the owner's team, with its policy and spend;
- the access history, the online backups and everything outside the access
  database, including the static installation keys.

Removed app keys stop authenticating at once, and the spend of removed keys and
teams is purged. Requests admitted before the reset finish under the identity
they were admitted with. With no owner claimed, everything goes and owner setup
stays open. The node answers `/zzrouter/v1/system/state/reset/preview` and
`/zzrouter/v1/system/state/reset` to the local proof only, so no API key,
session or MCP tool can reset.

The backup must hold the records' current revision, and the preview's
confirmation lasts five minutes, never past the backup's `protected_until`, and
no longer than the coordinator process. If anything changes the records after
the backup (`409 state_revision_changed`) or the confirmation lapses
(`409 state_reset_confirmation_invalid`), nothing is reset: run the command
again without `--backup-id`. The reset needs an `Idempotency-Key`; a retry with
the same key and body answers with the stored receipt, after a restart too.

To undo a reset, restore the backup its receipt names, as for any online
backup: export it, stop the node, stage the copy with `recovery restore` and
place the prepared image as the next section describes for a staged `database`
bundle. Export it before retention removes it.

## Restore and activate deliberately

```sh
zzrouter-node recovery restore /secure-backups/router.tar.gz \
  --key-file /separate-key-storage/router.key \
  --to /new/private/restore-staging
```

The destination must not exist. Restore creates it, marked `INCOMPLETE`, checks
the archive with the image inside it, and removes it again if the check fails. It
then extracts the archive and verifies the extracted bytes against the manifest
it checked, refusing an archive replaced in between. Then it prepares the staged
access database image to replace a store's content, in one transaction: a new
generation (the store ID is kept), a new credential revision for every person,
every pending sign-in and enrollment grant expired, and one
`store.restore_prepared` row in its history. It checks the prepared image again
and records it in the staging manifest's `staged` object: `image_sha256`, the new
`generation` and `prepared_at`. The image's entry in `files` keeps the archived
digest. Restore refuses an archive that already carries `staged`. An interrupted
extraction or a failed preparation leaves `INCOMPLETE`; do not activate it. A
successful staging directory contains `manifest.json`, `config/`, any separate
`data/` and `providers/` content, `store/image.sqlite` when the bundle has an
access store, and `protection/` when a key was supplied. Inspect the manifest's
original roots before moving anything.

No command activates a staged directory yet; activation is a later change. What
an operator can do today, by hand, with the node stopped:

- A staged `installation` bundle holds `manifest.json`, `config/`, any `data/`
  and `providers/`, `store/image.sqlite` when the installation had an access
  store, and `protection/credentials.key` when a key was supplied. Restore all
  of it as described below.
- A staged `database` bundle holds only `manifest.json` and
  `store/image.sqlite`. Place the image as the access database, exactly as
  described below, into the installation it came from: the store ID in that
  installation's `access-store.json` must equal `store.store_id` in the staging
  `manifest.json`. Nothing else in the installation changes.
- A staged `handoff` bundle is not activated in this release, by command or by
  hand: see [Coordinator handoff](#coordinator-handoff-experimental).

Once the prepared image is in place, browser sessions, delegated OAuth tokens and
app-key references issued before the restore do not resolve, without any edit of
the OAuth authority state. People sign in again with their passwords; app keys
keep working. Static installation keys are not in the database and are unaffected.

Keep the target node stopped. Install the matching release and service account,
restore configuration and data to the intended roots, and set their ownership for
that account. Reinstall provider executables and recover separate model/external
data. Restore the protection key to `config/private/credentials.key`, or to the
explicit `security.credentials.key_file` location. Unix keys require owner-only
permissions; Windows keys must be owned and readable under the intended service
identity with the restricted ACL. Do not loosen permissions to bypass validation.

Restore the access database from the prepared image only. Check that the SHA-256
of the staged `store/image.sqlite` (`sha256sum`, `shasum -a 256` or PowerShell
`Get-FileHash`) equals `staged.image_sha256` in the staging `manifest.json`. Move
the target configuration's existing `access/` directory aside: it holds the
database being replaced and can hold its journal, which SQLite would replay over
the new one. Create `access/` in the target configuration directory, owner-only
for the service account, and place the staged image there as
`access/access.sqlite`, owner-only. From an `installation` bundle, the store
marker `config/access-store.json` goes to `access-store.json` in the target
configuration directory with the rest of `config/`; it names the store ID the
prepared image keeps. Never place an image whose digest differs from
`staged.image_sha256`, an image restore did not prepare, or a `-journal` file
beside it.

Staging is intentionally separate from activation: configuration and provider data
can live on different filesystems, and replacing them is not one atomic operation.
Do not point a running installation at partially recovered state. Preserve the
existing installation until recovery has been qualified.

Start the node and verify owner sign-in, a team policy and app key, recorded spend,
provider connections, worker trust, the `store.restore_prepared` entry in
**System → Access history** and one real request. Recover the owner through
the existing local recovery command if necessary. Expired worker certificates can
require pairing again. An older valid backup can restore access that was revoked
later; review people, keys, grants and budget state after recovery. A successful
archive check is not a successful activation test.

## Coordinator handoff (experimental)

A `handoff` bundle holds what a new coordinator needs from the old one, and
nothing that belongs to the old machine. This release writes, checks and stages
it. Activating a staged handoff on the new coordinator arrives in a later release;
until then the staging directory shows what would move, and nothing in this
release, command or manual step, activates it. Treat the profile as experimental
until that release has been rehearsed on a real two-node cluster.

A handoff carries exactly these entries, and `recovery check` and `recovery
restore` refuse a handoff that carries any other, naming the entry. They also
refuse one with no provider definition, which has lost its provider tree, and one
whose `source`, `separate` list or entry names are not short plain text. The
list alone decides what a handoff carries: a provider or an asset named like a
temporary, lock or PID file travels like any other:

| Entry | What it is |
| --- | --- |
| `store/image.sqlite` | The access database image: people and their passwords, app keys, teams and their policies, and the access history. Staging prepares it like every image. |
| `config/providers/settings.yaml` | Global provider settings. |
| `config/providers/<kind>/<name>/config.yaml` and `schema.yaml` | Each provider's definition and its parameter schema, for the kinds `on-demand`, `external`, `cloud` and `registries`. |
| `config/providers/<kind>/<name>/assets/<asset>` | Operator assets, such as chat templates. |
| `config/providers/.managed-ownership.json` | The ownership receipt: which keys of each shipped provider definition the release manages. |
| `config/model_groups.yaml` | Routes. |
| `config/spend.json` | Recorded spend. A clean stop flushes it; from a source that crashed, it holds spend only up to its last save, at most 60 seconds old. |
| `config/pricing_overrides.yaml` | Pricing overrides. |
| `config/credentials.enc` | Encrypted provider connections. As for an installation bundle, the key is exported separately with `--key-output` and never packed, and the manifest's `key_sha256` names it. |

An entry travels only because this list names it: a file a later release adds to
the configuration directory does not travel until the list names it too. A
handoff never carries `node.yaml`, the dotenv files (`.env`, `.env.local`,
`.env.<environment>`), OAuth authority state (`oauth.json`), the cluster CA and
its deny list (`ca/`), node identity (`identity/`), cluster and pairing state
(`cluster/`, `pairing.txt`), private keys (`private/`), the recovery layout, the
store marker, online backups, the rollback directory, update state, data
directories, the pricing cache, provider installs or models. Its manifest has no
`inventory`. It names `source`, the old coordinator's release, node name and
platform, and `separate`, what the operator moves or recreates by hand:

```json
{"version": 1, "profile": "handoff", "created": "2026-11-19T10:54:46Z",
 "store": {"store_id": "4b244e15-…", "generation": "4b244e15-…", "schema_version": 1, "revision": 3, "sequence": 5},
 "source": {"release": "0.3.0", "node": "coord-1", "platform": "linux"},
 "key_sha256": "4b94c8b3…",
 "separate": ["The destination's own node.yaml: node name, listen address, TLS, storage paths, authentication and cluster role.", "…"],
 "files": [{"name": "config/credentials.enc", "size": 204, "sha256": "3ceac6a1…"}, "…"]}
```

The flow, up to staging:

1. Install the matching release on the destination.
2. Stop the source coordinator through its supervisor and keep its supervisor from
   restarting it. The ordinary stop drains requests and flushes spend.
3. Take the handoff from the stopped source:

   ```sh
   zzrouter-node recovery backup --profile handoff --dir /path/to/installation \
     --output /secure-backups/handoff.tar.gz \
     --key-output /separate-key-storage/handoff.key
   ```

   The same rules as an installation backup apply: the installation lock, new
   output paths, the key in a separate directory, no output after a failure or an
   interrupt. A worker, or a node that never ran as a coordinator, has no access
   database and so no handoff: the command stops with `no access store` and writes
   nothing. A link in place of the provider tree, a kind, a provider, its assets
   or a carried file, and, on a case-insensitive filesystem, a name that is a
   carried entry spelled in another case, also stop it, naming the path: the
   node would read them, so a handoff refuses them rather than leave them behind.
   On a case-sensitive filesystem such a spelling is a separate file the node
   ignores, and it is not carried. Links elsewhere, such as a provider install's
   environment, are not carried either. The command prints what the bundle
   carries and the `separate` list.
4. Check it, and stage it on the destination:

   ```sh
   zzrouter-node recovery check /secure-backups/handoff.tar.gz \
     --key-file /separate-key-storage/handoff.key
   zzrouter-node recovery restore /secure-backups/handoff.tar.gz \
     --key-file /separate-key-storage/handoff.key \
     --to /new/private/handoff-staging
   ```

   Staging works as for every profile. The staging directory holds
   `manifest.json`, `config/` with the entries above, the prepared
   `store/image.sqlite` and `protection/credentials.key`. Once a later release
   activates it, sessions, delegated tokens and app-key references the old
   coordinator issued do not resolve; passwords and app keys keep working.

What the operator moves or recreates by hand, as `separate` lists:

- The destination's own `node.yaml`: node name, listen address, TLS, storage
  paths, authentication settings and cluster role. A handoff carries none of the
  source's.
- Environment variables the dotenv files or the service environment set: the
  static installation keys (`ZZROUTER_ADMIN_API_KEY`, `ZZROUTER_API_KEY`),
  provider credentials (such as `OPENAI_API_KEY` or `HF_TOKEN`) and every variable
  a provider definition reads (`connection.fields[].env`, or `${VARIABLE}` in a
  value). Set them on the destination: a connection that reads its key from the
  environment fails without it. A handoff carries the connections saved through
  zzRouter in `credentials.enc`, never the environment.
- External services the provider definitions point at, such as an Ollama server
  or an endpoint on the old machine's network: make sure the destination reaches
  them.
- External TLS or proxy trust material that `node.yaml` names: copy it over a
  secure channel and name it in the destination's `node.yaml`.
- Provider runtimes and model weights: install and download them on the
  destination.
- Workers: pair each one again with the new coordinator. The cluster CA, node
  identity and worker enrollments stay with the old one. Browsers and delegated
  OAuth clients sign in or authorize again.

A handoff never rewrites node names. Provider enablement entries (`nodes.<node>`)
and route replicas pinned to the old coordinator's node keep that name, and a
replica with no node, which meant the old coordinator, means the new one; review
them through the running API after activation.
