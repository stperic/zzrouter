# Native release packaging

These recipes wrap the same release bundle and installer used by the archives.
The macOS app delegates installation and console authorization to the shipped
CLI. The Windows setup delegates installation to `install.ps1`. Neither wrapper
implements accounts, credentials or access policy.

The recipes are optional release steps. Adding them does not mean a published
release has signed native packages. The normal release workflow currently
publishes verified archives; native artifacts need the signing environment and
platform qualification below before publication.

## Build and stage once

The Makefile and release CI both call `scripts/stage-release.sh` to produce
normal binary names, matching installers and license notices. Stage only the
three binaries from the same build. Native packaging takes that staged directory;
it does not download or mix release components.

The launcher integrity hash is embedded in the node. Native signing changes
binary bytes, so the release build calls `sign-binary.sh` in this order:

1. Build and optionally sign the launcher.
2. Compute its SHA256 and embed it while building the node.
3. Build and sign the node and client.
4. Stage, package and sign the outer native installer.

Do not sign or modify a staged launcher afterward. Rebuild its node if the
launcher changes. Without a platform signing identity the hook is a no-op;
signed native packaging refuses unsigned inputs rather than silently fixing them
after the hash was embedded.

## macOS app and DMG

Run on macOS with Xcode command-line tools. The wrapper targets macOS 13 or
later. Select `arm64` or `amd64` to match the staged bundle. For a local,
explicitly unsigned qualification build:

```sh
bash scripts/packaging/macos.sh arm64 dist/darwin-arm64 /tmp/router-dmg-test 0.2.3 --unsigned
```

The output directory must not exist. The result is named `*-unsigned.dmg`.
The app can be copied to Applications and then opened. It installs the three
binaries under `~/.local/bin`, uses the existing per-user LaunchAgent and shows
the CLI's temporary console code. Existing configuration stays in its normal
location, outside the app bundle. Moving the app to Trash does not uninstall the
node; use the bundled `install.sh --uninstall` for that action.

For distribution, configure `ZZROUTER_MACOS_SIGN_IDENTITY` as a Developer ID
Application identity before the release build. Provision an existing notarytool
keychain profile in the release account and set `ZZROUTER_MACOS_NOTARY_PROFILE`.
Then omit `--unsigned`. Packaging verifies the existing binary signatures, signs
the app and DMG, submits for notarization, staples the result and runs Gatekeeper
assessment. Credentials stay in the signing environment, not in this repository.

## Windows installer

Run on Windows with Inno Setup's `ISCC.exe` and the Windows SDK signing tools.
For local unsigned qualification:

```powershell
.\scripts\packaging\windows.ps1 -BundleDir .\dist\windows-amd64 `
  -OutputDir C:\Temp\router-setup-test -Version 0.2.3 -Unsigned
```

The output directory must not exist. The result is named `*-unsigned.exe`.
The setup defaults to installation for the current user, with an explicit
elevated option for the existing Windows service. Uninstallation removes the
installed executables and shortcut while preserving configuration and data.
Setup offers **Open Router console** after installation. This opens the same
Start menu shortcut, whose PowerShell window stays open so the temporary code
can be copied. The shortcut retains the installation's selected configuration
directory; it does not depend on the parent installer's process environment.

For signed binaries, set `ZZROUTER_WINDOWS_SIGN_THUMBPRINT` to a certificate
already available to the release account before the release build. The hook
uses `signtool.exe` with SHA256 and a timestamp. For the outer installer, pass
`-SignToolCommand` using Inno Setup's documented `$f` placeholder. For example,
with an appropriately configured signing certificate:

```powershell
.\scripts\packaging\windows.ps1 -BundleDir .\dist\windows-amd64 `
  -OutputDir C:\Temp\router-setup-signed -Version 0.2.3 `
  -SignToolCommand 'signtool.exe sign /a /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 $f'
```

The recipe verifies the staged binaries' Authenticode signatures first. Inno
Setup signs both setup and uninstall through its configured tool. See the
[Inno Setup signing contract](https://jrsoftware.org/inno-setup/documentation/topic_setup_signtool.htm)
and [compiler options](https://jrsoftware.org/inno-setup/documentation/topic_compilercmdline.htm).

## Release qualification

Run `pwsh -NoProfile -File scripts/packaging/tests/windows-console-shortcut.ps1`
for the source fixture covering the persistent window, literal path quoting and
native setup handoff. It does not create a real shortcut or start installation.

Source syntax, staging fixtures and cross-compilation cannot establish native
installation behavior. Before publishing a native artifact, verify it on clean
supported machines: first install, owner setup, normal sign-in, reopening the
console, login/reboot persistence, upgrade preserving configuration and
credentials, failed-upgrade diagnostics, pairing, owner recovery and uninstall.
Check both Windows privilege modes and both macOS architectures. Verify the
installed node accepts its matching signed launcher and rejects a changed one.

These wrappers use the existing installer and its upgrade behavior. A partial
installation failure is reported; they do not add transactional rollback for
every filesystem or OS-service mutation. Do not describe them as qualified
until the corresponding signed artifact has passed those checks.
