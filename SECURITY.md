# Security Policy

## Supported Versions

Security fixes target the latest public 0.x release. Upgrade cluster peers
according to the [protocol compatibility policy](docs/protocol_versioning.md).

## Reporting a Vulnerability

### How to Report

Send reports privately to **security@medoya.com**. Do not put vulnerabilities
or credentials in public issues. Include affected versions, source locations,
reproduction steps and impact; a minimal proof of concept helps.

### What to Expect

We aim to acknowledge reports promptly, communicate progress and coordinate
public disclosure. Credit is given with the reporter's consent. Response and
fix timelines depend on the report; no fixed turnaround is guaranteed.

## Security Best Practices

1. Use TLS for client connections outside loopback and restrict network access.
2. Use generated high-entropy keys. Give inference clients separate virtual keys.
3. Protect configuration and CA private keys; verify pairing fingerprints independently.
4. Keep software current and verify release signatures and checksums.
5. Monitor authentication and inference logs; restrict access to captured prompts.
6. Use a nonprivileged service identity and human-controlled install policy.
7. Debug logging does not bypass authentication. Keep inference authentication
   enabled for network binds.

## Windows Security

Windows 10 version 1809+ and Windows Server 2019+ are supported. The installer
checks the minimum Windows version.

### Process Isolation

Managed provider process trees use Windows Job Objects with kill-on-close.
Graceful shutdown uses a stdin pipe so it works without an interactive console.
This is process-lifecycle containment, not a security sandbox for an untrusted engine.

### File System Security

Per-user configuration lives under `%APPDATA%\zzrouter`; service installs use
`%PROGRAMDATA%\zzrouter`. The installer restricts credential-file ACLs; runtime
checks warn about broad access. Verify effective account and directory permissions.
Go's synthetic POSIX permission bits do not establish NTFS protection.

### Network Binding

Installers default to loopback. LAN exposure needs an explicit bind and firewall
policy. Inference requires keys for network binds. Worker inference is mounted
only on the coordinator-authenticated cluster mTLS listener.

### Child Process Environment

Provider processes receive a filtered environment. Server credentials are
excluded; necessary Windows process variables are retained.

### Parameter Validation

Provider parameters are validated before execution. Direct executable launches
use argument arrays. Managed recipe edits cannot introduce arbitrary commands,
hooks or host package installation; protected operator policy controls grants.

### Service Hardening (Windows)

Elevated `scripts/install.ps1` installs a supervised service by default.
Unprivileged installations run detached. Review the service account and ACLs.
Unchanged shipped recipes may run under LocalSystem; recipe overrides require
a non-elevated identity capable of proving the protected policy boundary.

### Windows-Specific Threat Model

| Threat | Mitigation |
| --- | --- |
| Orphaned provider processes | Job Objects and process-tree termination |
| Credential file disclosure | Restricted ACLs and runtime permission warnings |
| Parameter injection | Validation and argv-based execution |
| Launcher substitution | Matching build-time SHA256 verification |
| Unauthorized recipe execution | Independent protected install policy |

## Detailed Security Guide

See the [security model](docs/security.md), [managed recipes](docs/typed_provider_recipes.md)
and [installation and updates](docs/install.md).
