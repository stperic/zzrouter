# Shared installation integration

Installation and MCP conventions live in the private `common` Go module. zzRouter pins its immutable `v0.3.1` release and no longer needs a sibling checkout or local replacement. Consumer hosted qualification precedes merging the integration.

The owner named the shared repository `common` so additional tools and libraries can live alongside its installation packages. The dependency is `github.com/stperic/common`, which includes the [shared MCP conventions](plan_shared-mcp.md). The owner selected Apache-2.0 and private hosting; installation APIs and product behavior are unchanged.

## Private module access

An authorized GitHub account can configure Git authentication with `gh auth setup-git`. Set `GOPRIVATE=github.com/stperic/common` when building or testing, so Go fetches this dependency directly without sending its path to the public proxy or checksum service:

```sh
export GOPRIVATE=github.com/stperic/common
go mod download
make build
```

CI uses the repository secret `COMMON_READ_KEY`, a read-only deploy key scoped to `stperic/common`. Go module/build caches are disabled in this public repository to avoid publishing private module source. Fork pull requests do not receive that credential and cannot run dependency-based checks; a maintainer must reproduce the change on an authorized branch. Ordinary GitHub release downloads for zzRouter remain public.

## Ownership and qualification

All download and retry callers import the shared module. `pkg/security` and `pkg/update/Verifier` retain their public contracts through thin adapters. The shared verifier keeps exact workflow/tag identity, bundle v0.3, transparency and observed signing time requirements. Product release lookup, allowed hosts, platform member naming and release manifests remain here.

Archive extraction scans the entire archive, bounds total expanded input, rejects duplicate matched binaries and publishes staged output only after success. The managed layout selects its ordered links and retained companions; `common/install` performs preflight and reverse restoration. The existing temporary-symlink behavior is retained. Service users, root updater requests, launcher hashes, health confirmation, rollback history and pruning are unchanged.

The release workflow invokes `zzrelease` for dependency notices, deterministic packaging, checksum generation and signing inside its existing job. The shared notice command replaces the former shell collector and queries the actual GOOS/GOARCH, release tags and disabled cgo setting for each archive. Build flags, launcher hashing, SBOMs, pinned cosign and the publisher certificate identity remain owned by this repository.

Validation on macOS arm64: 616 affected-package race tests passed across 12 packages, with 3 platform/live-service skips. `go build ./...`, `go vet ./...` and the repository's Go 1.26.9 lint invocation are required before commit. Shared-module tests and the native zztrace installation trial are recorded in the sibling zztrace report `reports/acceptance/2026-10-18-shared-installation/README.md`. This is local qualification, not a live updater or signed hosted-release run.
