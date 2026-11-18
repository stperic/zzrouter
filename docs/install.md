# Installation and updates

Release archives contain the client (`zzrouter`), node (`zzrouter-node`),
process launcher (`zzrouter-launcher`), platform installer and license notices.
The executables use these normal names inside each archive, with `.exe` on
Windows. Keep all three together: the node embeds the SHA256 of its matching
launcher. Available targets are Linux amd64/arm64, macOS amd64/arm64 and Windows
amd64.

Follow the [README](../README.md) for Unix download, Sigstore verification,
checksum verification and installation. A checksum from an unauthenticated
source does not establish publisher identity.

## First connection

The installer starts a coordinator on loopback for one-computer use. It does
not ask you to invent or copy an administrator API key. On an interactive desktop
it opens the console after verifying the running installation; otherwise run:

```sh
zzrouter console
```

Enter the temporary code printed by the trusted local CLI, then create your
owner username and password. The code is short-lived and single-use; the URL
does not contain it. Follow the console checklist to connect a provider, prepare
a model, test Chat and connect an application. The coordinator computer can also
run local providers.

Use **People** to invite another person to a team. Use **Keys & teams** to
create a named application key and set the team's providers, routes, limits
and cloud spending budget. People sign in with their accounts; applications
use their own keys. Existing API-key sign-in remains under Advanced access.

For a headless machine, run `zzrouter console` in a trusted shell on that
machine. Reach its console through HTTPS or an SSH-forwarded loopback port.
For a default loopback installation, forward port 9090 with
`ssh -L 9090:127.0.0.1:9090 user@router-host`, then open
`http://localhost:9090/console/` on your computer. Use the actual port if yours
differs. `zzrouter console --print` only prints a URL and does not issue a code.

If the owner's password is lost, run `zzrouter access recover-owner` on the
installed host, with its configuration directory and service-account authority.
Follow the printed recovery instructions. Local recovery preserves app keys,
providers, models and computer trust; it invalidates the owner's old sessions.

## macOS and Linux

After verifying and extracting the archive, run its bundled `install.sh`.
It finds the three binaries alongside itself even when invoked from another
directory. A missing binary stops installation before any running node is stopped.

| Mode | Command | Startup behavior |
| --- | --- | --- |
| macOS personal install | `bash install.sh` | Per-user LaunchAgent, starts when you log in |
| Linux supervised install | `sudo bash install.sh` | systemd service, starts at boot |
| Linux personal install | `bash install.sh` | Detached process, no restart supervisor |
| Explicit lightweight mode | `bash install.sh --no-service` | Detached process |

Run the macOS installer as yourself, not with `sudo`. Its **Start when I log in**
behavior does not start the node before login. Stopping or uninstalling unloads
the LaunchAgent so KeepAlive cannot immediately restart it. Use `--yes` to skip
prompts and browser opening, and `--role worker` when installing an additional
computer for the first time.

The installer keeps an existing `node.yaml`, including its mode, bind address
and ports. Flags that describe a new installation do not overwrite that file.
`--force-config` explicitly replaces it after making a backup; ordinary upgrades
do not need this flag. A preserved HTTPS installation is refused before stopping
the node: use its authenticated updater, which already has the configured trust.

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
powershell -NoProfile -ExecutionPolicy Bypass -File .\zzrouter-release\install.ps1
```

Without elevation, this installs for the current user and starts a detached
node. Run from an Administrator shell for the Windows service, which starts at
boot. `-PerUser` explicitly selects user mode; `-NoOpenConsole` skips browser
opening. The installer adds a **Router console** Start menu shortcut. It opens
a persistent PowerShell window with the temporary sign-in code and the selected
configuration directory. Native setup offers this same shortcut after installation.
Existing configuration and credentials are preserved, with the same HTTPS upgrade
preflight as the Unix installer.

An existing Windows service with a custom daemon account is checked before any
stop or file replacement. If the credential permissions cannot safely retain
that account, installation refuses and explains the account review needed. It
does not replace the service definition or silently switch to LocalSystem.

The native macOS app/DMG and Windows installer have build recipes in
[scripts/packaging](../scripts/packaging/README.md). These recipes do not establish
that a published tag includes signed desktop installers. Use only artifacts
actually attached to the verified release. Signing, notarization, and clean-OS
install/upgrade/reboot qualification require the corresponding release machines
and credentials.

## Supervised services

The bundled installers and the matching source checkout's `scripts/install.sh`
or `scripts/install.ps1` use the same path. Linux root installs and elevated
Windows installs default to a system service. macOS uses the current user's
LaunchAgent. Read installer options before changing an existing installation.

Credential creation and migration belong to the node's configuration library,
called through `zzrouter-node config credentials`. Installers and service setup
reuse that operation; they do not generate or print another administrator key.
The canonical credential file is `.env` in the selected configuration directory.
An existing service's authoritative credential is retained during supported
legacy migration. Ambiguous conflicts fail with source paths instead of silently
rotating credentials. Explicit environment overrides remain explicit overrides.
User installations and Windows services respect an absolute
`ZZROUTER_CONFIG_DIR` override and use that same directory for the service and
CLI. Linux system services require `/etc/zzrouter`; a custom directory is
refused before any stop or file changes. Linux custom service drop-ins that change
`Environment`, `EnvironmentFile`, `ExecStart` or `User` require explicit
reconciliation or the existing managed updater. The installer refuses those
overrides before stopping the node; the managed stop-only drop-in is retained.

Linux managed services keep binaries under root-owned `/opt/zzrouter/versions`
and activate the node, client and launcher through `/opt/zzrouter/bin` symlinks.
The public `/usr/local/bin` links use these same managed copies. A dedicated service
user owns runtime state. Root update path/timer units perform privileged updates;
keep the binary tree and service units outside the service user's write access.
Windows services use machine configuration under `%PROGRAMDATA%\zzrouter`.
An update restarts a Windows service by exiting with an error code, which
Windows counts as a non-crash failure: its restart actions apply only when the
service's non-crash failure flag is set. The installer sets it on new services.
On an older service, `update status` reports a restart is required and names
the fix; run it once as Administrator: `sc.exe failureflag zzrouter 1`.

## Connect people and applications

The coordinator is useful on its own. Finish owner sign-in and try a local model
or cloud connection before adding workers. On first startup, protected provider
credential storage is prepared under the node account automatically; explicit
`security.credentials.key_file` configurations retain their existing key custody.

Teams define provider/route access and shared budgets for both people and app keys.
Invite people through **People**; issue a named key only for an API application.
**Clients → Connect client** separates account-based MCP browser authorization from
API-key setup. Both paths use the same backend policy and attribution.

Before sharing an invitation or client endpoint, check **System → General**.
The default localhost issuer is local to one computer. For another device, use a
reachable HTTPS origin in `auth.oauth.issuer_url`, restart the coordinator and test
that origin from that device. Allow OAuth discovery/authorization through a reverse
proxy. A configured address is not proof of network reachability. Cluster pairing
uses the cluster listener independently of the browser address.

Review committed access changes in **System → Access history** and recovery scope
in **System → Recovery**. Use the [offline recovery guide](recovery.md) to back up
each coordinator and worker. Keep protection-key backups separately.

## Add another computer

Additional workers are optional. Finish your coordinator setup first; add another
computer whenever you need it.

1. In the coordinator console, open **Nodes > Add compute node**.
2. Choose the new computer’s operating system. Download, verify and extract the
   matching release using the linked release instructions.
3. On the new computer, run the displayed **Install as a worker** command. It
   includes the right cluster port. Windows offers a service that starts at boot
   or a personal installation for the current session. Native worker installers
   ask for the same port before installation.
4. Run **Connect to this router** on that computer. For Windows, open a new
   PowerShell window and select the installation type you used. Keep the worker
   running and copy the temporary pairing JSON.
5. Return to the console, select **I have pairing details**, paste or upload the
   JSON, compare its name and fingerprint with the worker, and approve it.
6. Open the node, choose **Set up a provider**, then a model. Chat and model tests
   keep that computer selected.

Existing workers can skip installation. Commands assume the documented default
configuration folders; custom configuration uses the existing CLI with the
installation’s `ZZROUTER_CONFIG_DIR`. Both computers must reach the cluster port
on their LAN or VPN. Management access can remain local-only. An expired pairing
window needs new details; an uncertain enrollment needs inventory inspection.

For terminal approval, save the pairing details as a private JSON file and run
on the coordinator:

```sh
zzrouter-node cluster accept --file pairing.json
zzrouter-node cluster accept --file pairing.json --confirm
```

The first command displays the computer name, endpoint, fingerprint and expiry
without enrolling it. Confirm only after checking these details against the
worker. The document is bounded data, not a script; it carries the existing
single-use pairing code. Expiry and pinned coordinator-led enrollment still
apply. The original code, `--worker-url` and `--worker-fingerprint` arguments
remain supported.

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

`zzrouter-node update history` and the update-history API read the privileged
updater's record on managed Linux installs. Root retains write ownership; the
service account only reads it. An existing mode-0640 history file becomes
service-readable when a privileged apply next records an outcome. To read that
older record immediately, run the history command as root. History records apply
outcomes; rollback continues to be reported by update status and the updater journal.

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
`scripts/install.sh`. The installer activates the complete bundle through the
existing managed updater before starting an installed Linux service. It verifies
the responding node's version and configuration before reporting success.
To repair public links separately, run `sudo zzrouter-node update enable` to repair
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

The first release containing the format-1 provider ownership receipt reader is
**v0.2.0**. Public v0.1.0 predates it. Private builds without the reader also
predate this recovery contract. A protocol number or application version comparison does not establish
that a binary contains the reader.

Automatic provider-config repair, for explicit rollback and failed-update
auto-rollback, requires a target at v0.2.0 or later and intervening template
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

### Manual recovery for targets before v0.2.0

Use a maintenance window when rolling back to v0.1.0 or another binary older than v0.2.0,
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

### Graceful node stop on existing systemd installs

New installations use `KillMode=mixed` so systemd signals only the node first;
the node signals each engine once and waits for graceful exit before escalation.
The root updater atomically installs `/etc/systemd/system/zzrouter-node.service.d/stop.conf`
and reloads systemd before restarting. First installation and `update enable`
write it through the same function. The drop-in is the sole definition of these
settings; the base unit carries neither. A write or reload failure is logged and
reported in `operation.warning`; the update or rollback continues. Recover
immediately with `sudo zzrouter-node update enable --restart=false`, or let the next
apply or rollback retry; an update `check` does not retry the drop-in. The drop-in
changes only stop settings. The recovery command also rewrites the base unit and relocates
binaries into the managed layout; `--restart=false` suppresses its default restart.

The updater applying the first update to this release is the old binary, so that
first update does not install the drop-in. It arrives on the next apply, or
immediately with `sudo zzrouter-node update enable --restart=false`. This uses the
existing privileged installer, with managed layout setup before service
installation.
Uninstall removes the managed stop drop-in and its directory when empty,
preserving operator drop-ins.

A rollback to a release older than the drop-in keeps `KillMode=mixed`. That
release stops runs one at a time within 45 seconds. Runs it has not reached
when it exits are SIGKILLed by systemd.
