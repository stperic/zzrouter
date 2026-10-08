// Package fsroot owns filesystem-layout primitives for provider installs:
// the provider root directory, per-provider paths (bin/venv/version/.managed),
// the backup/restore/cleanup rollback helpers, the per-provider install
// lockfile, symlink-safe verification, version-string validation, process-
// scoped install config (proxy env, hardened-venv toggle), and the
// shell-quoting helpers used to compose install-step commands.
//
// Depends on stdlib only. The parent install package re-exports the
// surface consumed by external callers (internal/server, pkg/prov_apps)
// so existing import paths keep working during decomposition; new code
// should import fsroot directly.
package fsroot
