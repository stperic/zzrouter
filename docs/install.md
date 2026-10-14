# Installation and updates

Release archives contain the client (`zzrouter`), node (`zzrouter-node`) and
process launcher (`zzrouter-launcher`). Install all three in one directory.
The node embeds the SHA256 of its matching launcher. Available targets are
Linux amd64/arm64, macOS amd64/arm64 and Windows amd64.

Follow the [README](../README.md) for Unix download, Sigstore verification,
checksum verification and installation. A checksum from an unauthenticated
source does not establish publisher identity.

## Windows

Windows 10 version 1809 or Windows Server 2019 and later are supported.
With cosign available, set the published tag before running PowerShell:

```powershell
$ErrorActionPreference = 'Stop'
$Version = 'vX.Y.Z'
$Base = "https://github.com/stperic/zzrouter/releases/download/$Version"
foreach ($File in @('zzrouter-windows-amd64.zip', 'checksums.txt', 'checksums.txt.sigstore.json')) {
  Invoke-WebRequest "$Base/$File" -OutFile $File
}
cosign verify-blob --new-bundle-format --bundle checksums.txt.sigstore.json `
  --certificate-identity "https://github.com/stperic/zzrouter/.github/workflows/release.yaml@refs/tags/$Version" `
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
if ($LASTEXITCODE -ne 0) { throw 'Publisher verification failed' }
$Entry = @(Get-Content checksums.txt | Where-Object { $_ -match '^[0-9a-f]{64}  zzrouter-windows-amd64.zip$' })
if ($Entry.Count -ne 1) { throw 'Missing or duplicate archive checksum' }
$Expected = ($Entry[0] -split '\s+')[0]
if ((Get-FileHash zzrouter-windows-amd64.zip -Algorithm SHA256).Hash -ne $Expected) { throw 'Checksum mismatch' }
Expand-Archive zzrouter-windows-amd64.zip -DestinationPath .\zzrouter-release
foreach ($Bin in @('zzrouter', 'zzrouter-node', 'zzrouter-launcher')) {
  Rename-Item ".\zzrouter-release\$Bin-windows-amd64.exe" "$Bin.exe"
}
```

Move the directory to your chosen installation location and put it on PATH.

## Supervised services

From the matching source checkout with all three built binaries, use
`scripts/install.sh` (Linux/macOS) or `scripts/install.ps1` (Windows).
Linux root installs and elevated Windows installs default to a service.
Unprivileged installs run detached; use them for evaluation and single-user use.
Read installer options before changing an existing installation.

Linux managed services keep binaries under root-owned `/opt/zzrouter/versions`
and activate the node, client and launcher through `/opt/zzrouter/bin` symlinks.
The public `/usr/local/bin` links use these same managed copies. A dedicated service
user owns runtime state. Root update path/timer units perform privileged updates;
keep the binary tree and service units outside the service user's write access.
Windows services use machine configuration under `%PROGRAMDATA%\zzrouter`.

## Updates and rollback

```sh
zzrouter-node update check
zzrouter-node update apply
zzrouter-node update rollback
```

On managed Linux installs, apply and rollback run without root privileges
submit the same privileged-updater request as the API. Success acknowledges the
request. Filesystem permissions control submission; permission errors explain
how to run as the service account or root. Root runs the same privileged executor
synchronously, without writing a request, and refuses while the updater service
or a published run is busy.
The privileged executor installs, restarts and verifies health. Follow it with
`zzrouter-node update status --json` or
`journalctl -u zzrouter-update.service -f`. `check` only reads release metadata;
`update run` remains the privileged executor. Other installs keep in-process
apply and rollback.

Updates require `checksums.txt.sigstore.json` authenticated for
`https://github.com/stperic/zzrouter/.github/workflows/release.yaml@refs/tags/v<version>`
and GitHub Actions' OIDC issuer. Metadata from another feed does not change
this compiled trust identity. Existing installations with a different publisher
need an operator-delivered trusted build before using this feed.

A managed Linux node submits an update request to its root updater. Activation
waits for health and rolls back on failure. Self-updating nodes confirm across
restart; unsupervised installations may require a manual restart.
Verify the running version as well as health after any update.

Managed updates require a client that runs and reports the release version.
Rollback restores the matching client when its version directory has one. Older
directories can lack a client; rollback then keeps the existing working client
link, and prune retains its source directory until a complete update replaces it.
These legacy rollbacks can temporarily run a newer client than the node.

To adopt managed client updates on an older service installation, deliver a
trusted build with an unused version and all three binaries through
`scripts/install.sh`, then run `sudo zzrouter-node update enable`. An existing
service's start command skips first-run binary relocation. Alternatively,
after a complete managed update, run `sudo zzrouter-node update enable` to repair
stale `/usr/local/bin` files or links. Running enable before the managed tree has
a client preserves the external client and reports a warning; run it again after
the complete update. If public-link repair fails after relocation, correct the
reported obstruction and retry with
`sudo /opt/zzrouter/bin/zzrouter-node update enable`.
User installs on Linux/macOS and Windows in-place installs
still require reinstalling all three to refresh the operator client.

Cluster rollout APIs update compatible workers serially and the coordinator
last. Protocol cutovers need operator maintenance, matching builds and rollback
copies for every node. See [protocol compatibility](protocol_versioning.md).

### Configuration rollback compatibility floor

Call the first release containing the format-1 provider ownership receipt reader
**R**. It is currently Unreleased; no release number has been assigned. Public
v0.1.0 predates R. Private builds without the reader also predate this recovery
contract. A protocol number or application version comparison does not establish
that a binary contains the reader.

Automatic provider-config repair, for explicit rollback and failed-update
auto-rollback, requires a target containing R's reader and intervening template
writers that retain the receipt contract. It cannot repair an already released
older binary. Retain a private target-era configuration backup before upgrades.

Before loading providers, the node account publishes a cumulative ownership
receipt at `providers/.managed-ownership.json`. It contains shipped provider
identities and managed paths, with no configuration values. Keep it through
successive rollbacks. A missing receipt adopts current ownership; it cannot
identify unknown keys written before adoption. Corrupt, unsupported or oversized
evidence refuses startup instead of discarding history. Preparation failures
count toward self-update boot attempts and cannot confirm an update as healthy.

For providers shipped by the target, startup removes historical managed keys
the target does not own, then applies its own managed runtime and install
recipes, schemas and shipped pins. Supported operator fields keep their latest
values, including enabled state, parameter tiers, models, nodes and environment.
Unseeded pins retain operator ownership. Values overwritten while managed cannot
be recovered. Unknown operator keys and unsupported values still fail strict
loading; there is no automatic merge of settings an older target cannot express.

A recorded shipped provider absent from the target is unavailable there. Its
`config.yaml` moves to sibling `config.yaml.rollback`, with the path logged.
Schemas, assets and provider data remain. A later release shipping that provider
restores the archive before seeding if the live config is absent. A collision
refuses startup without overwriting either file. Custom providers outside the
receipt are not quarantined. Disabling a provider does not bypass strict decoding.

Receipt and config publication use atomic writes. Archive moves retry after
process interruption; they do not have an explicit parent-directory fsync
guarantee across power loss. Repair runs after the previous process drains, so
persisted API edits survive where the target supports them. Concurrent manual
editing or multiple node processes sharing one provider tree is outside this
guarantee.

The receipt is read only under the nonroot node identity. Root bootstrap and
the privileged updater never read it; UID-0 startup applies only current-template
reconciliation and cannot perform historical cleanup. Binary trees remain
root-owned. The same node-side recovery supports macOS, Windows and unmanaged
in-place layouts without a `versions/` tree.

This recovery covers managed provider configuration. It does not downgrade
engine payloads, restore `providers/settings.yaml`, or alter `node.yaml`, keys,
teams, installation policy, models or runtime data. Settings remain strictly
decoded and operator-owned. `node.yaml` currently accepts unknown keys, so it does
not have this particular unknown-field startup failure.

### Manual recovery for targets before R

Use a maintenance window when rolling back to v0.1.0 or another pre-R binary,
or when ownership evidence is unavailable or corrupt. Repeated binary rollback
alone does not repair an incompatible provider tree. The privileged executor
does not automatically roll back again after an explicit rollback fails health.

1. Stop inference and configuration mutations, disable automatic updates, and
   isolate incompatible peers. On managed Linux, pause `zzrouter-update.timer`
   and `zzrouter-update.path`. Wait for any active updater to finish before
   proceeding.
2. Record the active release, actual service account and configuration directory,
   including any `ZZROUTER_CONFIG_DIR` override. Stop the node through its service
   manager and verify it stays stopped. Use the service's configuration context,
   rather than an administrator's default profile.
3. Back up that account's entire provider tree privately, including the receipt,
   archives and assets. Provider configs can contain credentials. Keep this newer
   tree separately so supported operator edits can be recovered manually.
4. Restore a known-good provider tree from when the target was active, retaining
   the node account's ownership and private permissions. If none exists, use the
   verified target binary's own configuration initializer in an isolated directory
   under that account, then reapply only settings the target supports. Replace
   only the live provider tree, preserving its current `settings.yaml`. Keep the
   existing `node.yaml`, keys, teams, installation policy, models and provider data.
   Settings incompatibility requires its own manual migration. Do not copy a newer
   receipt into the target-era tree and assume an old binary can use it.
5. Use the existing binary rollback path with matching node and launcher binaries.
   On managed Linux, run the verified managed CLI as root in the recorded config
   context. For the default layout, use the command below, substituting the actual
   configuration path if different:

   ```sh
   sudo env ZZROUTER_CONFIG_DIR=/etc/zzrouter \
     /opt/zzrouter/bin/zzrouter-node update rollback
   ```

   Root executes synchronously while path/timer units stay paused; an unprivileged
   submission would only leave a waiting request. In-place installs use paired backups and require restart when reported. Leave the binary
   tree root-owned. If the failed rollback already activated the desired target,
   restart that target after config recovery instead of popping the stack again.
6. Verify the running target version, protocol, health and an inference or provider
   request pinned to that node. Rejoin only a compatible cluster, then restore
   the intended update policy. Reapply additional operator edits from the retained
   newer tree only after checking target support.

There is no automatic three-way merge without a target-era baseline. Fresh
templates also do not undo provider runtime upgrades, so check the target's
runtime and dependency requirements before resuming workloads.
