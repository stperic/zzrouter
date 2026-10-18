# Shared installation integration

The `refactor/shared-installation` branch extracts the common implementation into the sibling `common` Go module. That module is local and unpublished. Do not merge this branch into a published product until its local replacement is removed and an immutable published module version passes clean hosted CI.

The owner named the shared repository `common` so additional tools and libraries can live alongside its installation packages. The dependency is `github.com/stperic/common` at local tag `v0.2.0`, which also includes the [shared MCP conventions](plan_shared-mcp.md). Installation package APIs and product behavior are unchanged.

All download and retry callers import the shared module. `pkg/security` and `pkg/update/Verifier` retain their public contracts through thin adapters. The shared verifier keeps exact workflow/tag identity, bundle v0.3, transparency and observed signing time requirements. Product release lookup, allowed hosts, platform member naming and release manifests remain here.

Archive extraction scans the entire archive, bounds total expanded input, rejects duplicate matched binaries and publishes staged output only after success. The managed layout selects its ordered links and retained companions; `common/install` performs preflight and reverse restoration. The existing temporary-symlink behavior is retained. Service users, root updater requests, launcher hashes, health confirmation, rollback history and pruning are unchanged.

The release workflow invokes `zzrelease` for deterministic packaging, checksum generation and signing inside its existing job. Build flags, launcher hashing, SBOMs, pinned cosign and the publisher certificate identity remain owned by this repository.

Validation on macOS arm64: 616 affected-package race tests passed across 12 packages, with 3 platform/live-service skips. `go build ./...`, `go vet ./...` and the repository's Go 1.26.9 lint invocation are required before commit. Shared-module tests and the native zztrace installation trial are recorded in the sibling zztrace report `reports/acceptance/2026-10-18-shared-installation/README.md`. This is local qualification, not a live updater or signed hosted-release run.
