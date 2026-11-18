# Coordinator state and recovery specification

Status: proposed. The API routes and recovery changes below are implementation requirements.

Base: the `feat/onboarding-access` worktree. It holds the people store, `pkg/recovery`, the [recovery contract](recovery.md) and the [onboarding and access spec](onboarding-access-spec.md) that this document builds on. Names of existing code below were checked against that tree on 2026-11-18.

Decision, 2026-11-18: access records and the audit history move into an embedded SQLite database. See "Why SQLite".

## Goals

- Keep coordinator access records consistent across writes, crashes and upgrades.
- Let operators inspect storage use, create a verified online backup and reset access state through a documented API.
- Let an operator move the coordinator to another machine using a portable backup and a planned cutover. Support an existing worker as the destination.
- Keep installation and maintenance self-contained on the platforms zzRouter already ships.
- Leave a storage foundation that later features and contributors can build on without a second migration of operators' data.

## Storage and ownership

One embedded SQLite database lives in the coordinator's configuration directory at `access/access.sqlite`, beside today's `access/people.json`. An unclustered node serving the same management role uses the same store. The node process already holds the installation lock (`config.LockInstallation`) for its lifetime; open the database under it, with restrictive directory and file permissions on Unix and Windows. Role activation must finish opening, validating and migrating the store before serving coordinator requests. Demotion must stop admitting writes and close the database. Worker mode leaves the database unopened and uses the existing cluster APIs and provider sync: on a worker the access core reads an empty snapshot, so only static installation keys authenticate there. A worker's own `keys.yaml` and `teams.yaml`, which it loads today, are left untouched and are imported only if that node is later activated as a coordinator. Nothing here changes the cluster protocol.

| Owner | Data |
| --- | --- |
| Access database | People and password hashes, the permanent owner initialization marker, teams and membership, app-key records and hashes, onboarding progress, access invitations and one-time grants, the audit history, schema metadata and maintenance receipts. Replaces `access/people.json`, `keys.yaml`, `teams.yaml` and `audit.jsonl`. |
| Installation and coordinator files | `node.yaml`, provider definitions and assets, routes (`model_groups.yaml`), spend (`spend.json`), `pricing_overrides.yaml`, OAuth authority state (`oauth.json`), encrypted provider connections (`credentials.enc`), cluster trust and the store marker. Preserve existing owners and API contracts. The handoff section says which of these travel. |
| Node-local state | Hardware and runtime paths, installed providers, model caches and weights, run state, and local node identity. Resolve these at the destination. |

Pricing enablement stays a typed `node.yaml` setting. The downloaded rates remain a rebuildable cache.

### Why SQLite

Access records alone do not need a database: they are small, one process writes them and reads come from memory. The decision rests on what comes next. Request and usage history is an in-memory ring of 1,000 requests today, and per-client usage over time needs a queryable store that an embedded database fits. Once the project carries SQLite for that, one persistence technology and one migration of operators' data are better than a snapshot format now and a database later. Three things also come with it for access records: a mutation and its audit event commit together, the audit history becomes filterable, and no size cap forces a rewrite of the whole state on each change.

Rejected alternatives:

- **One JSON snapshot file**, the shape `people.Store` has today. It adds no dependency and is enough for the records at this size. It is the right choice only if no database ever joins the project, and it leaves audit as an unfiltered append-only file.
- **bbolt**, a pure-Go key-value store. It is transactional with a smaller dependency, but it has no SQL for the history queries that motivate the choice, no foreign keys, and operators cannot inspect it with standard tools.
- **Three files, as today.** A change that spans two of them is not atomic, which is why the services carry rollbacks and a startup orphan sweep.
- **A database server.** It breaks the self-contained installation.

History gets its own database file in its own spec and never shares this one. Backups and resets of access records must stay small, and inserts at request rate must not contend with access commits. Both stores use one small package that opens, migrates, backs up and checks a SQLite file and knows nothing about access. The recovery archive's container format moves into a package of its own below both `pkg/recovery` and the access store, because the store writes and reads database archives and recovery checks images through the store.

The risk is the driver, not SQLite. `modernc.org/sqlite` is a machine translation of the C source and is CGO-free. Checked on 2026-11-18: v1.62.0 bundles SQLite 3.54.1, covers all five shipped targets (linux and darwin on amd64 and arm64, windows on amd64) and exposes the online backup API. Its own documentation sets three rules this design must follow: pin `modernc.org/libc` to the exact version in the driver's `go.mod`, bound the connection pool, and never share a raw connection between goroutines. Pin the driver and include it in the existing dependency and release checks. Measure the `zzrouter-node` binary size before and after and report it.

### Transactions and versioning

- Store people and app-key team references with foreign keys. Keep existing immutable IDs, normalized uniqueness and protected owner rules. Audit events retain actor and subject IDs as historical values so deleting a record preserves its history.
- Commit each access mutation and its audit event together. Examples include creating an app key and its personal team, accepting an invitation and creating its person, and issuing the owner setup grant with its workspace team. Return new credentials after commit. The transactions replace the compensations listed under Preparatory refactoring. No transaction is open while the OAuth authority is called.
- SQLite is the durable writer, not the read path. Reads come from the snapshot published after each commit, so authentication and policy checks never run SQL.
- Use one connection and the rollback journal, not WAL. Authentication and policy never query. The few queries that exist (audit pages) are bounded and finish before any response byte is written, so a slow client cannot hold the connection and block access writes. One connection also satisfies the driver's pool and raw-connection rules, and a clean commit leaves no sidecar file. Keep transactions short and the database on a local filesystem.
- Apply these settings through the connection string, so a reopened connection keeps them: `foreign_keys=ON`, `journal_mode=DELETE`, `synchronous=EXTRA`, and `fullfsync` on macOS. `EXTRA` is required: in this journal mode a commit is the removal of the journal file, and without it a power loss can undo a commit whose credential was already returned. Opening the store writes nothing beyond SQLite's own recovery of a journal left by a crash.
- There is one audit history, and the store is its only writer: an operation carries its event, and the store has one more operation that records an event for a change whose state lives elsewhere. No separate audit sink remains. Reading needs its own small interface with a keyset cursor: `handleAccessAudit` type-asserts `*audit.FileSink` today and uses its file-offset cursor, which the console treats as opaque. An event for a database mutation is written in that mutation's transaction. Events whose state lives in a file (spend and usage resets) and maintenance events (backup created, backup exported) are appended to the same table after their own commit, best effort as today. Pages are keyset pages built in memory. Filtering by actor, action, subject and time becomes one indexed query; it ships as its own change (see Delivery order).
- Keep OAuth signing and token storage in the existing authority. Resolve credentials against the current access store so removed identities and advanced credential revisions invalidate delegated access. Preserve token hashing and password verification semantics. Static installation keys retain their existing local configuration and credential lifecycle.
- Maintain five values that any release can read from any schema version. The schema version lives in the file header (`PRAGMA user_version`), because it must be readable even when a newer schema uses syntax the installed SQLite cannot parse: the store reads the header first and applies its settings afterwards. The other four live in a metadata table whose layout never changes:
  - an immutable **store ID**;
  - the **schema version** (in the header, as above);
  - a **revision** that advances when people, teams, keys or invitations change. It is what a backup must match before a reset. Console-entry codes (issue, exchange, consume), onboarding milestones and audit rows for maintenance and file-backed events do not advance it, so a console sign-in, a served request or a new backup leaves a reset preview eligible;
  - a **sequence** that advances on every commit that changes stored content. A schema migration changes no content and does not advance it. Every "nothing was written since" check below compares the sequence, never the revision, so no audit row is dropped silently;
  - a **generation**, equal to the store ID at creation and replaced by a random value whenever a staged restore replaces the database's content. A re-import does not rotate the generation: it builds a new store with a new store ID (see "Legacy files after the import"). The new value is written into the staged image before that image is published, never afterwards. Revision and sequence are comparable only within one generation, because a restore moves them backwards. Status, backup manifests, confirmation tokens and receipts carry the generation.
- A staged restore invalidates earlier sessions and delegated tokens through the version comparisons that exist today, with no change to a stored format (`control.CredentialReference` has no generation field, and references are persisted in `oauth.json` and in grant issuers):
  - *People.* Staging gives every person a new credential revision, the mechanism `RevokeSessions` uses. A person's version is that revision, and code outside the access core compares it (grant issuers, the compare-and-swap in password change and session revocation), so nothing may be mixed into it.
  - *App keys.* The generation is mixed into `virtualVersion` whenever it differs from the store ID. First-generation versions are therefore computed exactly as today, and references issued before the upgrade keep resolving.
  - *Static installation keys* are not in the database, and sessions bound to them are untouched.

### Upgrade and recovery behavior

A store that cannot open is fatal at startup, as an unreadable `teams.yaml` is today. The node exits with a closed reason and the next action; it has no degraded mode. Every existing mechanism then works unchanged: the updater gets no answer from `/health` and rolls back, with a probe that needs no change in any released binary; the CLI's running checks stay true; no route needs a recovery-mode answer. Diagnosis and repair are offline, where `zzrouter-node recovery` already works: `recovery inventory` gains a `store` section with the state, the reason, the next action and the archives that can be restored. Each next action starts with stopping the service: a supervisor that restarts the node every few seconds would otherwise race the offline commands for the installation lock. The same single rule covers a promotion at runtime: a store that cannot open stops the node. A promotion cannot be refused instead, because by the time the role changes the new mode is already committed. Both promotion paths (a configuration reload, and a worker that is removed from its cluster, which becomes an unclustered coordinator) write `node.yaml` before any listener runs, the listeners run independently, and `role.Manager.Set` publishes the role before its subscribers and gives them no veto. The node therefore exits, and the restart reaches the startup refusal with its reason. A coordinator without a store never exists.

| Reason (names proposed) | Meaning | Next action |
| --- | --- | --- |
| `missing` | The store marker says a database existed, and it is gone. | Activate a checked backup, or remove the marker to start over; legacy files still in place would then be imported. |
| `schema_newer` | A newer release wrote the database and the self-restore rule below did not apply. | Start that release, or activate a backup the installed release can read. |
| `integrity_failed` | The database opens but fails its checks. | Activate the newest checked backup. |
| `unreadable` | The database cannot be opened. | Activate the newest checked backup. |
| `import_invalid` | A legacy file failed validation during an import. | Repair the named file, then start again. On a re-import the database is intact, so removing the file keeps the database instead. |

**First upgrade.** Validate the legacy files and their references, then import them into a staged database in one transaction. The sources are `keys.yaml`, `teams.yaml`, `access/people.json` and `audit.jsonl`. Preserve existing IDs, password and key hashes, the initialization marker and consumed grant states. Carry env-backed keys unchanged: a `key_env` record names an environment variable and has no hash. Count unreadable audit lines and report them; they do not fail the import. A personal team left without its key, which a failed key creation can leave behind today, is removed by the import with an audit row instead of failing it: this is the one startup sweep (`ReconcilePersonalTeams`) run for the last time. Verify database integrity, foreign keys and import counts before publishing the database with one atomic rename; an interrupted import leaves no database and the next start imports again.

The import checks the invariants of stored state, not the input rules of the API: data that a previous release legitimately wrote must import, or the update would roll back on every attempt. Within that, it is stricter than today's load on purpose. An unreadable `keys.yaml` is only a warning today and the node runs with no virtual keys. Now any legacy file that fails validation stops the start with `import_invalid`, the update rolls back, and nothing is silently dropped.

**Legacy files after the import.** The new release never moves or changes them. The previous release reads them, and `pkg/update` restarts that release when the new one never answers `/health`. Every rule below keys on the database's own metadata. None reads update state, which a node on a service install cannot see, and none depends on a directory existing.

1. *No database.* If the store marker exists, the database was lost: stop with `missing`. Otherwise, if legacy files exist, import them and record the import's sequence and, for each of the four files, a digest of its decoded content or the fact that it was absent. Otherwise create an empty store.
2. *Database still at its import sequence.* Nothing has been written since the import. If every file recorded as present is still present and any file's content differs from the record (a file that was absent and now exists counts), the previous release ran in between: import again, as a new store. If a file recorded as present is absent, the operator has begun removing them: use the database.
3. *Database past its import sequence, or with no import record* (fresh, restored or handed off). Legacy files are never read as input again. With an import record, content that differs from the recorded digests sets the status warning `legacy_files_changed`: access changes made on an older release after this database moved on are not in it, and the operator repeats them. With no import record the files are only listed as a retained copy. This check writes nothing.

The **store marker** is what tells a lost database from a first upgrade. Backups cannot: they exist only if someone created one and they expire. It is a small file beside `node.yaml` that holds the store ID and no secret. It is written when a database is first published or activated, and a start that finds a database ensures it exists. Recovery bundles carry it.

A re-import builds a new store: a new store ID, a generation equal to it, and a rewritten store marker. A pre-migration archive made from the earlier import can therefore never match the new content, and no backup of the earlier import can exist, because creating one advances the sequence. Rotating only the generation would be wrong: the generation is mixed into app-key versions whenever it differs from the store ID, so every app-key reference issued on the older release during the rollback window would stop resolving.

Digests are over decoded content, not bytes, because the previous release rewrites `keys.yaml` and `teams.yaml` at every shutdown without changing a record.

What this means for an operator, stated in the upgrade notes:

- An automatic rollback returns the previous release to exactly its pre-upgrade state, and changes made on that release are imported at the next upgrade.
- A manual rollback after the database has been written serves the pre-upgrade records. Changes since the upgrade are missing and access revoked since is live again while the older release runs. `docs/recovery.md` gives the same warning for restoring an older backup.
- The legacy files hold credential hashes for as long as they stay. Status lists them, and the operator removes them once returning to a pre-database release is no longer wanted.

**Schema migrations.** Before one, write a verified database archive (the format of an online backup, below) at a fixed path and keep it outside the backup rotation until the next migration succeeds. This archive writes no audit row, and its manifest records the sequence. Apply all pending migrations in one transaction; they do not advance the sequence. A release that meets a newer schema restores that archive itself only when the store ID and generation match, the store's sequence equals the archive's, and the release can read the archive's schema. Nothing has been written since the migration in that case, so the content is the same state: the generation is kept and an update rollback keeps working. The restore is staged beside the database and published with one atomic rename, so a crash leaves either file whole. In every other case the start stops with `schema_newer`. `update rollback` does not check the schema; an operator who rolls back past a migration after writes gets that reason and its next action.

The first SQLite release freezes what an older binary must be able to rely on later: the metadata table, the store marker's name and format, the archive's path, and its container and manifest version for the `database` profile (`recovery.validateManifest` accepts only version 1 today). That release tests the rule against a synthetic next schema, because a later release cannot add any of this to an older binary.

Remove the legacy `/state/snapshot` and `/state/restore` routes and their handler. This is step 0 of the preparatory refactoring. They are test-harness facilities for four files, two of which no longer exist, and no e2e suite uses them. Keep the `state.snapshot` and `state.restore` audit action names, because imported history contains them and released actions are never repurposed.

### Extending the store

These rules keep the store cheap to change for the next feature and the next contributor.

- **One place per concern.** A new record type or field is made in `pkg/access`: a migration with its fixture, the validator, the store operation, the published snapshot and the contract suite. In `internal/server` only the handler that exposes it changes, and the changelog names the schema change. Two architecture tests hold the boundary: only the plumbing package imports the driver, and only the access store opens `access/access.sqlite`. `pkg/recovery` opens archive images through the plumbing package.
- **The database is internal.** The API is the only supported way to change access records, and the schema is not a public interface. `POST /zzrouter/v1/keys/reload` re-reads a hand-edited `keys.yaml` today; retire it with a `410` that says keys are managed through the API. The retired-route handler is specific to provider parameters today (its message names the parameters PATCH, and the route catalog recognizes it by that method's name), so generalize it first. Hand-editing was also the only way to declare an env-backed key, since create and rotate clear `key_env`. Imported env-backed keys keep working; this spec adds no way to create new ones, and a `key_env` field on the keys API would be its own change.
- **A schema change is routine, not an event.** Migrations are numbered SQL files embedded in the binary, applied in order, forward only. A released migration is never edited; a committed checksum list fails the build if one changes. Each released schema version has a small fixture database, a test fails if a released version has none, and one test migrates every fixture to the current version and validates it. The changelog names every schema change, because it decides what an update rollback can do.
- **Rules are data-independent.** The validator takes candidate records and returns closed problem codes. It reads no file, clock or network, so it runs in the API, the import, the recovery check and tests alike. SQL constraints are a second line of defense that never produces a different verdict.
- **Generic plumbing is separate from access.** Opening with the required settings, migrating, backing up and checking a SQLite file live in one small package with no access knowledge, so the history store reuses it instead of copying it.
- **History is a different database.** Three datasets grow with use:
  - *Audit* records administrative changes only: keys, teams, people, grants. Inference traffic never writes to it, so it grows with administration, not with load. It moves into this database.
  - *Request history* is an in-memory ring of the last 1,000 requests and is lost on restart.
  - *Spend* is running totals per scope in `spend.json`, flushed every 60 seconds, with no time series.

  Per-client usage over time and request search are the next spec. They get their own database file on the same plumbing.

Describe the first three rules in `CONTRIBUTING.md` and update the storage table in `docs/configuration.md`.

### Copies that hold credentials

Every copy of access records outside the live database has a stated lifetime. Status lists the ones present by kind, creation time and size, without paths.

| Copy | Created by | Removed by |
| --- | --- | --- |
| Online backups | `POST /backups` | Rotation. |
| Pre-migration archive | A schema migration | The next successful migration. |
| Legacy files, in place | The releases before the database | The operator, once a pre-database release is no longer wanted. |
| Activation rollback directory | `recovery activate` | The operator, after the destination is qualified. |

Reset removes none of these copies. They return to use only through a local restore or, for the legacy files, a manual rollback to a pre-database release.

## Preparatory refactoring

The database must arrive as a storage swap behind one boundary, not as a fourth persistence pattern. Today the access records sit in three stores with two commit shapes, and the server's services hold the logic that keeps them consistent. Land the steps below first, on the existing files, each as its own change with the full race suite green and its net production-line change reported. Each preserves success-path behavior. Three steps deliberately change behavior, and their change descriptions say so: step 1 (a failed save no longer leaves memory ahead of disk), step 2 (nothing is rewritten at shutdown) and step 12 (workers stop loading access files). Findings are from the `feat/onboarding-access` worktree.

| # | Today | Target before the database |
| --- | --- | --- |
| 0 | `/state/snapshot`, `/state/restore` and `POST /keys/reload` are the only callers of the stores' `Reload`, and the state routes also call `Save` and write the YAML files directly. No e2e suite calls the harness state helpers; only their own test does. | Delete the two state routes with their handler, the harness state helpers and `StateTar` diagnostics, their discovery and reserved-name entries and their OpenAPI paths. Retire `/keys/reload` with the generalized `410` handler. This comes first, because step 1 removes `Reload`. |
| 1 | `keys.FileKeyStore` and `teams.FileTeamStore` change memory, then the service calls `Save()` (14 calls in four files). A failed save leaves memory ahead of disk ("key created but failed to persist"). `people.Store.mutate` already copies the state, applies the change, persists, then publishes. | One `access.Store` interface whose methods are whole operations. Persistence happens inside the operation. `Save` and `Reload` leave the interfaces. `KeysService.ReloadKeys` went with its route in step 0. |
| 2 | A shutdown flush rewrites both files "for in-memory-only fields", naming `VirtualKey.LastSeenAt`. Nothing writes that field. | Delete the flush. Decide the field in its own change: wire it deliberately or remove it from the type, the key response, the OpenAPI schema and the client code that prints it. |
| 3 | `Server`, `KeysService` and `TeamsService` hold the concrete `*FileKeyStore` and `*FileTeamStore`. Only the `control` package and `budgetSnapshotter` use the `KeyStore` and `TeamStore` interfaces. | Every caller depends on the interface. No type outside the storage package names a file store. |
| 4 | Operations that span records are service choreography: key plus personal team (create team, create key, roll the team back, sweep orphans at startup), the workspace team created when the owner setup grant is issued (`initialOwnerTeam`), team deletion (checks keys, people and invitations by hand), `TeamsService.membership` (joins three stores). | Each is one named operation on the store: create key with its team, issue the owner setup grant with its workspace team, delete team with its dependents check, membership projection. The existing choreography moves behind the boundary unchanged, so the swap deletes it in one place. |
| 5 | `TeamsService.mu` is the cross-store serialization boundary. `KeysService` and `mutatePeople` take it, and a three-level lock order is documented in the onboarding spec. `KeysService` also has a mutex of its own, taken first and absent from that order. | The store's single writer gate. Services hold no mutex for store consistency. The one surviving rule: the OAuth authority is never called while a write is open. |
| 6 | Four read shapes: copies under an `RWMutex` (keys), the stored pointers themselves handed to callers (teams), copies under a mutex (people), and an atomic flag for `HasGatedTeam` on the anonymous hot path. | One immutable snapshot published after each commit, under the copy-on-write rule `AppsConfigStore` already follows: a state once handed out never changes. `control.Core` reads the snapshot. The request path never runs SQL. |
| 7 | Two invalidation paths for one cache: `KeyStore.OnChange` feeds the authenticator, and `KeysService` also calls `InvalidateAuthCache()` at four sites. Deletions have a third path: `OnKeyDeleted` and `OnTeamDeleted`. | One commit notification from the store that carries the IDs it removed. One subscriber invalidates the cache and purges the spend rows of removed keys and teams, which `OnKeyDeleted` and `OnTeamDeleted` do today from four call sites. The explicit calls are deleted. |
| 8 | Two audit patterns: `people.Store` emits inside `mutate` with the caller's identity context; the key and team services emit after `Save()` with `context.Background()`. | The operation carries its audit event to the store with the change. Services lose `SetAuditSink`. The transaction then includes the event without touching a caller. |
| 9 | `people.Store` holds runtime state beside the records: login throttling and the Argon2 admission semaphore. | Throttling and password admission move beside the store, not in it. |
| 10 | The `zzrouter` client imports `pkg/access/teams` (policy types) and `pkg/access/people` (the local proof file reader). The proof file lives in `people` although it will authorize maintenance as well. | Domain types in packages without storage. The local proof in its own package. The storage implementation where only `zzrouter-node` imports it, held by an import-guard test like the one under `pkg/modelregistry/source`. |
| 11 | Access rules are split: services (a personal team takes no second key, reserved ID prefixes, role values), stores (immutable kind, preserved `CreatedBy`), `recovery.validateSnapshots` (team references across files), and soon SQL constraints. | One rule set in `pkg/access`, run by API writes, the legacy import and the recovery check, in the way `pkg/config` owns provider-tree rules. |
| 12 | The stores follow the process, not the role. People and OAuth open when the node is built and close at process stop; a node promoted at runtime has neither. A failed promotion is logged and the role stays coordinator. Workers load `keys.yaml` and `teams.yaml` and run `ReconcilePersonalTeams`, which can write `teams.yaml`. | The access store opens when the node becomes a coordinator, at boot or by promotion, and closes on demotion. A store that cannot open stops the node, at boot as an unreadable `teams.yaml` does today, and on promotion as well. A worker opens nothing. This is an intended behavior change for workers and is stated in the change. |

Order: 0, 10, then 1 to 3 together with 8, 7, 4 and 5 together, 6, 12, 9, with 11 folded into 4. Step 8 travels with 1 to 3 because both rewrite every key and team write. In step 1 an unparseable `keys.yaml`, only a warning today, also becomes fatal at open: left as a warning, the next key write would replace the file with the one new key. Until the swap, the implementation of `access.Store` is an adapter over the three existing stores; do not write a new unified file store.

After these steps the database change is: a SQLite implementation of `access.Store`, the import, and the maintenance operations. Once the server runs on the SQLite store, a following change reduces the file stores to decoders, which the import and the check of older recovery bundles still need, and deletes the adapter, the choreography from step 4 and `ReconcilePersonalTeams`. No file store remains as a second write path. One contract test suite runs against the adapter and the SQLite store while both exist, which is what proves the import preserves access semantics; when the adapter goes, an import-equivalence test takes over from the dual run.

Stop and report if a step cannot preserve success-path behavior. The thin services that remain should make the whole arc net-negative in production lines, not counting the driver.

## Delivery order

Each stage ships on its own and leaves the node complete.

1. **Preparatory refactoring.** No storage change.
2. **Storage swap.** The SQLite store, the import and the legacy-file rules, the startup reasons with the `store` section of `recovery inventory`, the status route, online backup, and everything `zzrouter-node recovery` needs to stay correct once a database exists: image capture, exclusions, the one validator, the `database` profile and staged restore with its rotation. Also the pre-migration archive with the self-restore rule. This is the first SQLite release, and it carries everything listed as frozen above.
3. **Database activation.** Restoring a backup into the same installation. Reset depends on it: a required backup is only worth requiring once it can be restored.
4. **Reset.**
5. **Handoff.**
6. **Audit filters**, a small change of their own.

## Maintenance API

Paths below are relative to `/zzrouter/v1/system/state`. Use the existing success envelope and Problem Details. The administrator routes use the existing admin gate and browser CSRF controls. The local-proof routes are mounted outside the admin group, as the owner-recovery route is. Worker requests return `421` through the existing `workerManagementMisdirected` answer. Keep all operations installation-scoped. Publish the endpoints in OpenAPI and link them from **System → Recovery**. The existing recovery inventory (`GET /zzrouter/v1/system/recovery`) continues to describe the full installation.

| Method and path | Result | Authorization |
| --- | --- | --- |
| `GET /` | `200` with store status, version, size and last reset receipt | Coordinator admin |
| `GET /backups` | `200` with the retained backup metadata | Coordinator admin |
| `POST /backups` | `201` with the new backup's metadata | Coordinator admin, `Idempotency-Key` |
| `GET /backups/{id}` | `200` with backup metadata, checksum and expiry | Coordinator admin |
| `GET /backups/{id}/content` | `200` with the verified archive as a streamed attachment | Local installation proof |
| `POST /reset/preview` | `200` with exact impact and a confirmation token | Local installation proof |
| `POST /reset` | `200` with the committed receipt | Local installation proof, confirmation token, `Idempotency-Key` |

Backup and reset are synchronous. This database holds access records and administrative audit only, so both are short bounded operations and neither needs a job. Remote console users can inspect status and create backups. The Recovery page displays the local retrieval and reset commands.

Local proof reuses the owner-recovery transport (`people.LocalRecovery`, `handleLocalOwnerRecovery`) and adds no second proof scheme:

- The daemon's proof file is the authority: one proof per daemon lifetime, readable only by the installation's account. No admin key or session substitutes for it. Today the proof is created only inside `initOAuth`, so a coordinator with OAuth disabled has none. Create it whenever the access store is open. Its rotation happens under `people.json.lock` today, a file that goes away; rotate it under the open store instead.
- The peer address must be assigned to this host (`people.LocalRecoveryAddress`). This is not a loopback-only rule: a node bound to one interface is reached on that address, and loopback alone is never authority.
- Reject any request that carries an `Origin` header or browser credentials.
- On these routes the proof travels in a dedicated request header (proposed: `X-Zzrouter-Local-Proof`), never in a URL or the body. MCP dispatch forwards a fixed set of headers (the caller's key headers and a few transport ones), so this header cannot arrive through a tool. The proof changes with every daemon lifetime, so a proof in the body would make the same request hash differently after a restart and defeat replay. The owner-recovery route keeps its body field.
- A proof middleware runs ahead of `s.idempotent` and sets a local-operator principal. The idempotency middleware skips requests without a principal today, so without this the routes would have no replay at all, and with it an unproven caller can never read a recorded reply. After admission retain only the authorized operation and the installation identity.
- The commands are online local operations, so they live in the `zzrouter` client beside `access recover-owner` and reuse its proof-reading client. Proposed names: `zzrouter access state export` and `zzrouter access state reset`. `zzrouter-node recovery` stays offline and gains no command that talks to a running node.
- Mark the three local-proof routes in the route catalog so `zz_api` cannot reach them. Exclude archive bytes, credentials, proofs and confirmation tokens from logs and captured API examples.

### Status and size

Return `store_id`, `generation`, `schema_version`, `revision`, `sequence`, `status`, `observed_at`, `size`, `warnings`, the retained copies and the last reset receipt when one exists. Status is `ready` or `setup_required`; a store in any other state stopped the node at startup. `warnings` is a closed list, today only `legacy_files_changed`, each with its next action. Report all sizes as integer bytes:

| Field in `size` | Meaning |
| --- | --- |
| `database_bytes` | Length of the main SQLite file. |
| `journal_bytes` | Length of a journal sidecar. `0` outside a transaction. |
| `free_page_bytes` | Reusable space in the main database, computed from SQLite freelist pages and page size. |
| `backup_bytes` | Total bytes in retained archives, reported separately from the live database size. |

Status reads are bounded and run no query: the freelist size is cached at each commit. File sizes are observations that may change during writes. Use `null` for an unavailable measurement. Full integrity scans and compaction belong to maintenance. Include checked time and outcome when a completed integrity check exists. [SQLite PRAGMA reference](https://www.sqlite.org/pragma.html)

### Online database backup

`POST /backups` accepts an empty object. It creates a snapshot of the database at one committed revision with a SQLite-supported consistent copy (`VACUUM INTO` or the backup API) while the coordinator serves traffic. It verifies the private temporary image with integrity and foreign-key checks, then writes a recovery archive in the existing container format with profile `database`: the image plus a manifest with store ID, generation, schema version, revision, sequence, creation time, image size and SHA-256. One reader, `recovery.Check`, validates every profile. The `backup created` audit row is committed before the archive is listed, so a listed backup always means the sequence has moved past the import and no re-import can follow it. A failed or interrupted backup lists nothing, and a restart removes leftovers. The same idempotency key and body return the original result inside the existing replay window while the process lives; the replay record is in memory, so a retry after a restart creates a second backup. A different body with the same key is refused by the existing middleware (`422 idempotency_key_conflict`). [SQLite backup API](https://www.sqlite.org/backup.html)

The archive contains credential hashes and access policy. Stream its contents only with local proof. Keep archives in a protected server-managed directory under `access/`, with fixed internal filenames and `Cache-Control: no-store`. No filesystem paths, remote destinations or arbitrary archive entries are client inputs. Keep every archive for at least the idempotency replay window, so a replayed create never returns an ID that is gone. Past that window keep the three newest, and none beyond seven days. A documented byte cap bounds the directory; creation beyond it is refused. Operators export verified copies with the local export command to their own protected backup storage.

An online database backup can restore the access database. A coordinator move uses the portable recovery bundle described below, including the related files that the access database depends on.

### Reset access state

Reset removes non-owner people, app keys, other teams, invitations, one-time grants and onboarding progress. Preserve the initialized marker, the protected owner and owner team, and the audit history. Keep the owner team's current policy and spend scope. Advance the owner's credential revision. An installation without an initialized owner remains eligible for normal setup.

1. Create or select a checked online backup. `POST /reset/preview` takes `backup_id` and returns counts by affected record type, the preserved owner scope, and a short-lived opaque confirmation token that binds the store ID, generation, revision and backup ID. Require the backup to match that generation and revision. A mismatched or expired backup requires a fresh backup.
2. `POST /reset` takes `backup_id` and `confirmation_token`. Look up the receipt first, by the idempotency key and body hash the middleware already computes, and answer a completed request from it before any token or revision check. Otherwise it is one transaction under the writer gate: recheck installation ownership, the backup, generation, revision and confirmation; apply the reset; write the audit event; store a receipt with the operation ID, that key and body hash, old and new revisions and backup ID. A changed revision yields a conflict and requires a fresh preview. The local command runs backup, preview and reset in one flow, so this is rare.
3. The commit notification invalidates the authentication cache, and sessions and delegated tokens of removed identities fail their live resolution against the new snapshot. Requests admitted before the commit finish under the identity they were admitted with, and their spend settles to the scopes they were admitted under; nothing is charged to another team.

A crash before the commit changes nothing. After it, the receipt is in the database. The idempotency middleware replays the original `200` while the process lives; after a restart the receipt answers the same key and body with the same `200` instead of running again.

Reset removes records the way the API's own deletes do and adds no second rule. Deleting a key or a team purges its spend rows today (`OnKeyDeleted`, `OnTeamDeleted`); a reset's commit notification carries the removed IDs to the same subscriber. A settlement still in flight can write a row back afterwards, exactly as after a delete today. No route names a team or a key, so removing them cannot widen route policy. Keep related files and static installation credentials under their existing maintenance APIs. SQLite reuses freed pages, so a successful reset may leave file length unchanged. [SQLite space reclamation](https://www.sqlite.org/lang_vacuum.html)

### Error and recovery contract

| Condition | HTTP and proposed code |
| --- | --- |
| Missing authentication or insufficient privilege | `401` or `403`, existing auth problems |
| Worker received coordinator maintenance request | `421`, existing coordinator-role problem |
| Unknown or expired backup | `404 state_backup_not_found` |
| Reset preview or backup revision changed | `409 state_revision_changed` |
| Invalid, stale or expired confirmation | `409 state_reset_confirmation_invalid` |
| The disk or the backup byte cap cannot hold a backup | `507 state_backup_capacity_exceeded` |

Responses identify the next read or operator action without including filesystem contents or raw database errors. Add the codes to `docs/agent_error_codes.md`.

## Coordinator handoff

Use the existing `zzrouter-node recovery` backup, check and staged restore workflow. Its archive gains a profile, so one format and one checker serve three scopes:

| Profile | Contents | Activation |
| --- | --- | --- |
| `installation` | Today's full bundle, with the database captured as described below. | Staged only, then manual, as `docs/recovery.md` describes. |
| `database` | The database image alone. Online backups and pre-migration archives use it. | New command, into the same installation. |
| `handoff` | The database image plus the portable coordinator files. | New command, onto another installation. |

The offline backup captures the stopped store through the same consistent copy and never copies the main file alone, so a transaction left in a journal by a crash is resolved first. Finishing SQLite's own journal recovery is the only change the offline tooling may make to the store. The live database file, any journal, the online backup directory and, once a database exists, the legacy access files are excluded from the bundle's raw file sources. The image travels under its own entry name, distinct from the live path, because restore refuses any manifest entry that matches an exclusion. `recovery.validateSnapshots` calls the one access validator and keeps the legacy decoders for bundles made before the database.

Staging prepares the image, so nothing has to happen after publication. After `recovery restore` has verified the extracted bytes against the archive, it prepares the staged image for every profile: a new generation, a new credential revision for every person, pending sign-in and enrollment grants expired, and the import record cleared. It then records the prepared image's digest in the staging manifest. A manual `installation` restore and both activations therefore never publish an image as it was archived, and sessions and delegated tokens issued to people and app keys before the restore stop resolving, with no offline edit of the OAuth authority state.

Activation is new: `pkg/recovery` stages and has never activated. Add `zzrouter-node recovery activate` (proposed name) for the `database` and `handoff` profiles only. It refuses a staging directory marked `INCOMPLETE`, requires the destination stopped and holds its installation lock, verifies the staging directory against its staging manifest, and verifies that the installed release reads the schema and that the image is compatible with the installation's routes, spend scopes and authentication authority.

- The `database` profile replaces one file with one atomic rename.
- The `handoff` profile replaces several files, which is not atomic. The command writes an activation marker before the first replacement and removes it after the last. A node refuses to start while the marker exists. Running the command again either completes the activation or restores everything from the rollback directory, and both are safe to repeat.
- It keeps what it replaces in a rollback directory.
- The image it publishes has no import record, so legacy access files on the destination are never imported over it. Status lists them as a retained copy.

The handoff profile exports the checked database and portable coordinator settings, routes, spend, pricing overrides, managed provider definitions and assets, and encrypted provider connections. Record the source release, schema, store ID and generation, file digests, declared external dependencies and target mapping requirements. Use the existing separate key custody for credential protection material. Treat host-bound configuration and credentials as explicit inputs at the destination. An env-backed key is host-bound: it works on the destination only when its variable is defined there, and the bundle lists those variables as mapping requirements. Carry authentication configuration and protected static keys through their existing secure recovery mechanism when preserving those clients is required. Preserve human account IDs, password verification, app-key IDs and key hashes so existing account and app-key credentials can be used.

It does not carry the cluster CA, node identity, worker enrollments and revocations, OAuth authority state, pins, download state or anything else node-local. Workers therefore pair again, and browser and delegated OAuth clients sign in or authorize again on the new coordinator.

### Planned move

1. Verify the destination release, role eligibility, capacity, service account, HTTPS or loopback policy, provider paths, and protected credential dependencies. If it is an existing worker, record its local configuration, enrollments and model roots for rollback. Stage and validate a rehearsal restore in a private directory while the original coordinator serves traffic.
2. Withdraw the source from client and worker ingress, then stop it through its supervisor; the ordinary node stop drains requests and flushes spend. Disable supervisor restart. Take the final handoff archive from the stopped source with `recovery backup`, which holds the installation lock. Verify it before making the target authoritative. Access changes ahead of the fence must appear in the final archive.
3. Stage the final bundle on the stopped destination and verify checksums, schema compatibility, team references, consistency of route and spend references, protection keys and paths. Map portable settings onto the target's own node name, listen address, storage paths and service identity. Report provider enablement entries (`nodes.<node>`) and route destinations pinned to a source node and require explicit mapping or operator resolution before activation. On an existing worker the provider tree is a cache of the source coordinator's tree, so the bundle's tree replaces it; a provider file the worker owns (cloud, registries) or an encrypted connection of its own that collides with the bundle needs an explicit choice. Keep the existing worker installation available for rollback. Use the supported local role and pairing flow to establish coordinator trust on the destination. The first supported handoff re-pairs workers against the new coordinator and verifies its fingerprint through the existing enrollment flow.
4. Start the destination coordinator and verify ready status, owner sign-in, an existing app key, team policy, recorded spend, provider credentials, audit history and one routed request. Repoint clients or move the stable public address, then verify worker reconnects, provider sync and the old source's continued isolation. The former coordinator may join as a worker through normal enrollment. First move everything that holds its old access records to archive storage: the `access/` directory, the store marker, and `keys.yaml`, `teams.yaml` and `audit.jsonl`, which live in the configuration root. A worker that is later removed from its cluster becomes an unclustered coordinator, and with nothing left behind it starts at owner setup. Leaving any of these in place would make it serve the superseded records, import the old files, or refuse to start.

The operator owns the cross-machine fence. A rehearsal or backup never activates another writer. A destination activation requires explicit confirmation that the source service is stopped and isolated. If activation fails before the destination accepts authoritative writes, stop and isolate it before restarting the source. After the destination accepts writes, moving back uses a new handoff from the destination so later changes travel with it. For an unavailable source, restore the last verified bundle only after fencing that source. Report the backup timestamp and review access revocations and spend since that point before reopening traffic.

## Acceptance criteria

- The preparatory refactoring lands first and changes only the behaviors its steps name. Afterwards no service calls a persistence method, holds a store mutex or names a file store, and one contract suite passes against the adapter and the SQLite store.
- Every released schema version has a fixture that migrates to the current version and validates. Architecture tests confine the driver to the plumbing package and the live database file to the access store, and the `zzrouter` client and the launcher do not link the driver.
- Fresh coordinator setup and legacy import preserve access semantics, including the audit history. A worker neither opens the database nor exposes administrative maintenance operations. A promotion whose store cannot open stops the node with the same reason as at boot, and `/health` behaves exactly as before.
- Killing the process during import, key and personal-team creation, or reset leaves the complete new state or the previous one, never a mix. A retried reset produces one mutation and one audit entry, before and after a restart.
- Update rollback, tested with the previous release's own updater and probe: a release whose import or migration fails exits at startup with a closed reason and is rolled back. A release that imports and is rolled back before any write returns to the previous release with its files untouched, and changes made on that release are imported at the next upgrade as a new store, and app-key references issued on that release keep resolving. Once the database has been written, legacy files are never imported again: changed ones raise `legacy_files_changed`, and removing some of them never empties the database. A release that migrates the schema and is rolled back before any content write starts ready on the previous release; the first SQLite release proves this against a synthetic next schema.
- A database that is missing stops the start with `missing`, with or without backups, because the store marker exists; it is never replaced by an import or an empty store. A former coordinator archived as the planned move describes starts at owner setup when it later leaves the cluster. `recovery inventory` reports the reason and the next action for every startup refusal.
- A backup taken during concurrent access writes is one known revision and passes the check. Corruption, low disk space and interrupted writes never list a backup. Status reports the database, journal and retained backup sizes. An offline bundle taken after a hard kill contains every committed transaction.
- After a database restore or a manual installation restore, a backup or confirmation token from the earlier generation cannot authorize a reset, and earlier sessions and delegated tokens of people and app keys do not resolve. References issued before the first upgrade keep resolving until then. Password change and session revocation still work after a restore. A crash at any point of an activation leaves a node that either starts on the complete new state or refuses to start.
- An audit row written by a backup or an export is never dropped silently: any such write advances the sequence, which rules out both a self-restore and a re-import.
- Public status and backup metadata contain no secrets or installation paths. Remote sessions, `zz_api` and stale confirmations cannot retrieve backup bytes or commit a reset. Reset invalidates affected access without losing the protected owner's recovery path. Status lists every retained copy that holds credentials.
- Restore to a distinct supported host, including an enrolled worker chosen as the destination. Activation refuses without the explicit confirmation that the source is stopped and isolated. Demonstrate a clean rollback before destination writes and a new handoff afterward.
- Check Linux, macOS and Windows packaging, filesystem permissions and offline recovery. Rehearse with a real two-node cluster before declaring handoff supported.
- The two state routes are gone and `/keys/reload` answers `410`. Imported env-backed keys still authenticate. An invalid legacy file stops the start with `import_invalid`, never an empty key set. A slow reader of an audit page does not delay an access write.
- Update `docs/recovery.md`, `docs/security.md`, `docs/configuration.md` and the storage-files list in the project guide in the same change.
