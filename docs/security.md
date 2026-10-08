# Security model

zzRouter gives agents an authenticated API for model and provider operations.
Human operators control host accounts, drivers, OS packages and independently
protected install policy. The API does not grant SSH or general host execution.

## Client and cluster authority

Static admin keys grant management and inference access. Inference keys and
virtual keys grant narrower access; virtual keys are Argon2id-hashed and can be
limited by team, model access, requests, tokens, concurrency and budget.
Do not give an inference client the admin credential.

Anonymous inference is allowed only when configured. The installer enables it
for loopback; network binds require keys. Team model allowlists also prevent
anonymous access. Anonymous cloud access is disabled by default.

Cluster pairing establishes a shared CA and node identities. Worker inference
and internal requests travel over mTLS, with coordinator identity checks on
worker inference. The coordinator is the enforcement point for forwarded client
quotas. Protect the CA private key and verify pairing fingerprints outside the
network channel. See [CA rotation](runbook_cluster_key_rotation.md).

## Host and runtime boundary

Unchanged embedded install recipes carry release authority. Effective recipe
overrides require a human-provisioned protected allowlist and a nonprivileged
service identity capable of proving that it cannot modify that policy. The API
cannot grant new packages, imports or indexes. See
[managed recipes](typed_provider_recipes.md).

Provider child environments are filtered to prevent server credentials from
reaching engines. Parameters are validated and normally passed as argv rather
than shell commands. These measures do not sandbox a malicious engine: run
trusted providers with an appropriately limited service account.

## Installation and updates

On a managed Linux service installation, the binary tree is root-owned. The
service user requests an update through a file; a root updater verifies and
installs it. The service user must not be able to rewrite its binaries.

Updates verify an offline Sigstore bundle for the exact repository, workflow
and tag, then bind the downloaded archive to signed SHA256 checksums. The node
and launcher move together. See [installation and updates](install.md).

Report vulnerabilities privately as described in [SECURITY.md](../SECURITY.md).
