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
and activate a version through `/opt/zzrouter/bin` symlinks. A dedicated service
user owns runtime state. Root update path/timer units perform privileged updates;
keep the binary tree and service units outside the service user's write access.
Windows services use machine configuration under `%PROGRAMDATA%\zzrouter`.

## Updates and rollback

```sh
zzrouter-node update check
zzrouter-node update apply
zzrouter-node update rollback
```

Updates require `checksums.txt.sigstore.json` authenticated for
`https://github.com/stperic/zzrouter/.github/workflows/release.yaml@refs/tags/v<version>`
and GitHub Actions' OIDC issuer. Metadata from another feed does not change
this compiled trust identity. Existing installations with a different publisher
need an operator-delivered trusted build before using this feed.

A managed Linux node submits an update request to its root updater. Activation
waits for health and rolls back on failure. Self-updating nodes confirm across
restart; unsupervised installations may require a manual restart.
Verify the running version as well as health after any update.

Cluster rollout APIs update compatible workers serially and the coordinator
last. Protocol cutovers need operator maintenance, matching builds and rollback
copies for every node. See [protocol compatibility](protocol_versioning.md).
