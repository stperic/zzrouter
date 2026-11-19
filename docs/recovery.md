# Installation backup and recovery

Use **System → Recovery** to review the running coordinator's scope. The local
`zzrouter-node recovery` commands work on one installation at a time. Back up each
worker separately. They do not stop services, restore privileged service files or
replace configuration; only `recovery activate` changes an installation, and only
its access store, from a staged database bundle.

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

`archives` lists the online backups in the configuration's `access/backups`
directory, newest first and then by `id`: each archive's `id` (its file is
`<id>.tar.gz`), `created_at`, `size_bytes`, the store identity its manifest
names, and `readable`. `readable` says only what the manifest states: a
`database` bundle at the schema version this release writes. It is not a check,
since the inventory reads nothing but the manifest; `recovery check` checks an
archive, so run it before you restore one. A regular file that is not such an
archive is listed as `readable: false` and aged by its modification time;
`.tmp` leftovers, links, directories and other special files are not listed,
and the inventory never follows a link. While the lock is held (`in_use`) the
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

Every archive has a `profile`. `recovery backup` writes `installation` bundles:
the scope above. A `database` bundle holds the access database image alone, with
no configuration and no key; `recovery check` and `recovery restore` handle it
the same way. A profile this release does not know is refused.

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

A staged `database` bundle holds only `manifest.json` and `store/image.sqlite`;
`recovery activate` publishes it. A staged `installation` bundle holds
`manifest.json`, `config/`, any `data/` and `providers/`, `store/image.sqlite`
when the installation had an access store, and `protection/credentials.key` when
a key was supplied; it is restored by hand, as the last part of this section
describes.

Once the prepared image is in place, browser sessions, delegated OAuth tokens and
app-key references issued before the restore do not resolve, without any edit of
the OAuth authority state. People, the owner included, sign in again with their
passwords; app keys keep working. Static installation keys are not in the
database and are unaffected.

### Activate a database bundle

Stop the node and its supervisor's restarts, then activate the staging directory
into the installation the backup came from:

```sh
zzrouter-node recovery activate /new/private/restore-staging --dir /path/to/installation --dry-run
zzrouter-node recovery activate /new/private/restore-staging --dir /path/to/installation
```

Activation holds the installation lock and first runs every check, in order,
reading only: the lock is free (a running node or another recovery command holds
it); the staging directory is not marked `INCOMPLETE`; its `manifest.json` is a
staging manifest of a `database` bundle with its `staged` object (any other
profile is refused); the directory holds exactly `manifest.json` and
`store/image.sqlite`; the image's SHA-256 equals `staged.image_sha256`; the image
passes the checks a coordinator start runs, with SQLite's full integrity check,
and its generation equals `staged.generation`; and the installation admits its
store. An installation's store marker, or without one its database, admits only
its own store ID, so a backup of another installation's store is refused
(`foreign_store`) wherever a marker or a database exists; only a fresh
installation, with neither, takes another store. A refusal names its reason
(`in_use`, `staging_invalid`, `foreign_store`, `already_activated`,
`activation_incomplete`, or the image's own reason such as `schema_newer`) and
the next action, and changes nothing anywhere. `--dry-run` runs the same checks, prints what would be
replaced, and changes nothing either.

Then activation writes the activation marker `recovery-activation.json` beside
`node.yaml`, records the activation in `rollback/<generation>/rollback.json` in the
configuration directory, and replaces the access store: it copies the image
beside the database as `access/access.sqlite.staged` and checks the copy, lets
SQLite settle a journal a crash left beside the database, copies the database and
the store marker into `rollback/<generation>/store/` together with any journal
SQLite could not settle (an unreadable database is kept as it is), removes that
journal so it cannot be rolled back into the new database, renames the copy over
the database in one step and writes the store marker. Each step reaches the disk
before the next. Last, it removes the activation marker and the staging directory,
which holds credential hashes. It prints what it replaced, the new generation and
the rollback ID (the generation), and the next step: start the node.

A crash or an interrupt (Ctrl-C) between the activation marker and its removal
leaves the marker, and the node then refuses to start with
`activation_incomplete`, naming the staging directory and the two commands that
end it. Run the same `recovery activate` command again to finish the activation:
each step it already did is skipped or repeated with the same result. Or add
`--rollback` to undo it: the database, any journal and the store marker that were
replaced return byte for byte (what did not exist before is removed), then the
rollback directory and the activation marker are removed, and the staging
directory is kept. Both are safe to repeat after another crash. `--rollback` works
only while the activation marker exists, and refuses with `no_activation`
otherwise: once an activation finished, nothing undoes it automatically. A crash before the activation marker was written changed
nothing. One after its removal leaves a finished activation and, possibly, the
staging directory: a rerun refuses with `already_activated`; delete the directory.

The rollback directory `rollback/<generation>/` in the configuration directory
(for example `/etc/zzrouter/rollback/<generation>/` on a Linux system
installation), owner-only, keeps what an activation replaced until you remove it:
`rollback.json` and, in `store/`, the previous `access.sqlite` and
`access-store.json`, with `kept.json` recording which of them existed. It holds
password and app-key hashes, so keep it only until the restored node is
qualified, then delete the directory. To go back to the previous store after a
finished activation, stop the node and copy those files back over
`access/access.sqlite` and `access-store.json` (delete one `kept.json` records as
absent), with no `access/access.sqlite-journal` left beside the database.
Rollback directories are never part of a recovery bundle;
`GET /zzrouter/v1/system/state` lists each as a retained copy of kind
`activation_rollback`, with its ID, creation time and size.

### Restore an installation bundle by hand

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
`access/access.sqlite`, owner-only. The store marker `config/access-store.json`
goes to `access-store.json` in the target configuration directory with the rest
of `config/`; it names the store ID the prepared image keeps. Never place an image
whose digest differs from `staged.image_sha256`, an image restore did not prepare,
or a `-journal` file beside it.

Staging is intentionally separate from activation: configuration and provider data
can live on different filesystems, and replacing them is not one atomic operation.
Do not point a running installation at partially recovered state. Preserve the
existing installation until recovery has been qualified.

### After the restore

At its first start, the node forgets the recorded spend (`spend.json`) of keys and
teams the restored store does not hold, so an ID created again later starts at
zero. Start the node and verify owner sign-in, a team policy and app key, recorded
spend, provider connections, worker trust, the `store.restore_prepared` entry in
**System → Access history** and one real request. Recover the owner through the
existing local recovery command if necessary. Expired worker certificates can
require pairing again. An older valid backup can restore access that was revoked
later; review people, keys, grants and budget state after recovery. A successful
archive check is not a successful activation test.
