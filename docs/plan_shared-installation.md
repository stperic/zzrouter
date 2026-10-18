# Shared installation primitives

Owner-selected scope, 2026-10-18: extract a sibling Go module, `common`, and use it from zzRouter and zztrace. zzfuse keeps its Compose and database lifecycle; it can adopt the same release packaging later.

## Ownership

- `common/download` owns the existing resumable transfer and stall watchdog. Move its tests and the small public retry package with all router consumers; convert every router caller and remove the old package.
- `common/artifact` owns bounded offline Sigstore verification, checksum verification and bounded tar/zip extraction. The caller fixes the publisher workflow, issuer and release tag. Router release lookup, network allow-lists, platform naming and release manifest remain here.
- `common/install` owns atomic symlink replacement and restoration after an ordinary activation error. Router selects its ordered links and retains its legacy companions, history, pruning and privileged updater. A multi-link update is not crash atomic.
- Shared release tooling packages caller-built binaries and generates checksums. Products keep build commands, cgo settings, signing identity and service/container definitions. Initial integration uses a local sibling module; consumer integration stays on this local branch until publication and immutable remote pins make CI self-contained.
- zztrace gets a small installer for its two MVP CLI binaries, immutable version directories, version probes and explicit rollback. Local archives require an explicit SHA256 from the operator; downloaded releases require the fixed publisher's signed checksums. No database migration or daemon installation is implied.

## Contracts and failure paths

Keep the router's existing API results, release tag identity, launcher hash pairing, install order and service privilege split. Verify before running extracted binaries. Extraction writes only to caller-selected staging files, bounds expanded data and refuses duplicate matches. Failed transfers and failed extraction leave the installed files untouched. Validate all link destinations before activation and return restoration errors. Callers serialize installation into an owner-controlled prefix; do not add a privileged request language.

## Verification

Move existing transfer tests, retain router integration tests, and add focused shared tests for authenticated metadata, bounded archives and activation failure. Exercise zztrace installation, upgrade, failed probe and rollback in a disposable local prefix with real cgo binaries. Run race tests, vet and build for affected Go packages, router lint and `npm run verify` for zztrace. Independent cold review precedes implementation and commits. No live installation, VM or LXC qualification is part of this change.

## Owner-selected naming

Rename the checkout and module to `common` to allow other shared tools and libraries. Update imports, consumer replacement paths and release commands together. Preserve package contracts and the original trial revisions; validate module resolution and build both consumers. This is a naming change with zero production Go lines added.

## Private release integration

On 2026-10-18 the owner selected Apache-2.0, private common hosting and release completion. Pin the published immutable version and remove the sibling replacement. CI and release jobs authenticate with a read-only common deploy key through a pinned SSH agent action; preserve strict GitHub host-key verification and disable public Go caches. Fork pull requests receive no key and must be reproduced on an authorized branch for dependency checks.

Move the existing third-party notice collector into common's release package and delete this repository's script. The Go command queries the real dependency graph for each archive's GOOS/GOARCH, release tags and disabled cgo setting, handles module replacement paths as JSON and fails on missing licenses or empty graphs. Keep publisher identity, signing, launcher pairing and all lifecycle rules here. Review the shared implementation and consumer wiring independently, then pass local checks and hosted CI before merging.
