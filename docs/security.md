# Security model

zzRouter gives agents an authenticated API for model and provider operations.
Human operators control host accounts, drivers, OS packages and independently
protected install policy. The API does not grant SSH or general host execution.

## Client and cluster authority

Static admin keys grant management and inference access. Inference keys and
virtual keys grant narrower access; virtual keys are Argon2id-hashed and can be
limited by team, model access, requests, tokens, concurrency and budget.
Do not give an inference client the admin credential.

A key or team spend limit requires known pricing before inference. Unpriced
targets are removed from model-group chains; if none remain, the request returns
403 `model_unpriced` with an administrator action and no retry or exhausted-budget
fields. Upstream-reported cost cannot substitute for pre-request rates. Recognized
local engines and explicit zero overrides are free, including after monetary
exhaustion. Zero spend limits remain unenforced. Workers retain the synthetic
static-admin bypass because the coordinator alone enforces client budgets.

Model resolution, including catalog population, runs before RPM/TPM checks, so
a caller over its RPM limit still incurs a resolution.

Anonymous inference is allowed only when configured. The installer enables it
for loopback; network binds require keys. Team model allowlists also prevent
anonymous access. Anonymous cloud access is disabled by default.

Cluster pairing establishes a shared CA and node identities. Worker inference
and internal requests travel over mTLS, with coordinator identity checks on
worker inference. The coordinator is the enforcement point for forwarded client
quotas. Protect the CA private key and verify pairing fingerprints outside the
network channel. See [CA rotation](runbook_cluster_key_rotation.md).

Cluster identity, CA and bootstrap initialization retain strict persistence
errors. If directory durability cannot be confirmed, startup can stop even after
the file replacement committed. Preserve existing trust files when diagnosing
and retrying initialization; do not regenerate the CA to bypass a storage error.

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

## Managed upstream secrets

`common/secrets` encrypts complete provider/registry records with AES-256-GCM,
random nonces and authenticated format metadata. A separately supplied startup
key protects `credentials.enc`; it is never generated beside the ciphertext or
returned by the API. Back up the key and encrypted state separately and verify
the matching pair during recovery. Encryption detects tampering or a wrong key,
but cannot detect restoration of an older valid backup or protect against a
compromised process holding the key.

One writer owns each store until service shutdown. Revision checks prevent stale
updates; removal retains a disabled marker rather than falling back to old
external values. Secret status exposes only presence, never a reveal operation.
Managed values are not injected into the environment or distributed to workers.
Candidate validation binds credentials to their submitted destination, refuses
redirects, and returns safe status codes without upstream bodies.

Atomic persistence distinguishes pre-rename failure from a committed replacement
whose directory durability could not be confirmed. The latter updates active
memory and reports `saved_durability_uncertain`; clients must refresh state before
another write. Activation and publication have their own outcomes. Windows uses
protected key ACL checks and exclusive file ownership, but the atomic-write
primitive does not claim Unix directory-sync guarantees there.

The [client architecture](client-architecture.md) records common-library and
product-policy boundaries. Provider key rotation uses the same revisioned replace
operation. Encryption-key rotation additionally requires coordinated external
key provisioning and restart recovery; retain both keys until verified.

## MCP browser authorization

The coordinator's local OAuth authority is enabled by default, with a fixed
localhost issuer using the configured node port and TLS mode. Set
`auth.oauth.issuer_url` to a canonical HTTPS origin for remote clients. HTTP
issuers are accepted only for loopback. `auth.oauth.enabled: false` opts out
of browser login while retaining key-authenticated MCP. Behind a TLS reverse proxy, bind the node to loopback
or otherwise restrict its plaintext listener. The configured issuer is fixed;
request Host and forwarding headers cannot select it. Workers have no login or
OAuth endpoints.

The browser authenticates using an existing admin or user key. It remembers the
identity for up to 12 hours and requires consent for each client. MCP clients
receive independent opaque tokens, not the API key. Admin and inference MCP
resources remain separate. OAuth credentials authorize only their MCP resource;
they are not general REST credentials. Inner MCP REST requests reauthenticate
and retain the original key/team identity, current permissions and quotas.
OAuth credentials never travel to providers or workers.

Consent labels client-supplied names as unverified and displays the validated
callback address before approval. Check that destination against the application
you intend to connect; a familiar display name alone does not identify a client.

Browser management sessions use host-only HttpOnly cookies and exact-Origin plus
CSRF checks on mutations, including sign-in and sign-out. Sessions retain the
original role and current key/team policy. `/me` exposes only the caller and
owned grants. Invalid explicit API credentials never fall back to a cookie.
Cookie-authenticated reads, session bootstrap and OAuth browser pages also reject
an explicit Origin other than the configured issuer. A credentialed CORS allowlist
does not grant other sites access to browser sessions. Native API credentials and
public OAuth protocol endpoints retain their configured cross-origin behavior.
Browser cookies confer no authority on MCP, compatibility, pairing, internal or
worker routes and are removed before all provider and cluster forwarding.

HTTPS uses `__Host-zzrouter_oauth_session` and `__Host-zzrouter_oauth_browser`,
always with Secure, HttpOnly, `Path=/` and no Domain, including deletion cookies.
Explicit loopback HTTP uses the separate plain `zzrouter_oauth_session` and
`zzrouter_oauth_browser` names and retains its `/oauth` scoped-cookie cleanup.
Repeated active cookie names are rejected even when their values are identical.
An HTTPS installation upgraded from plain cookie names requires a fresh sign-in;
old plain values are never accepted or promoted, even if their stored session
still exists. Existing OAuth grants and tokens are unchanged. Both old and new
private names are removed before provider or cluster forwarding.

The `/console/` shell contains no account data and serves only fixed embedded
assets with a same-origin content security policy. Login/logout rotate the CSRF
proof, which is signed for the current browser session. An old form cannot use
a new identity even if delayed responses overwrite a proof cookie. The console
clears protected content before refresh, confirms bootstrap and identity are
coherent, and rejects late page/identity results. It never automatically replays
a mutation. Credentials stay in transient forms or one-time key disclosures,
with no localStorage, sessionStorage, URL token or credential-bearing cross-tab
message. See [console behavior](web-console.md).

Focus checks never install a new proof under an existing form. Unchanged session
probes preserve transient candidates and one-time disclosures; changed authority
clears them before refresh. A pending own-key rotation and its disclosure allow
the expected unauthenticated result only with the original issuer and signed
session proof. Network failure pauses new writes without destroying that
disclosure. Expiry, cross-tab auth changes and pagehide still clear it. Pending
durable writes block in-app navigation and leave only a generic outcome warning
if an unavoidable reset interrupts their observation.
One-time key responses remain hidden while an overlapping session check is
unresolved or failed. Read-only verification can resume presentation only for
the same authority; changed access or expiry aborts and discards the result.

Access tokens last 15 minutes. Refresh tokens rotate, with an absolute 30-day
connection lifetime. Reuse of a consumed refresh token revokes its connection. Explicit revocation
also accepts a consumed refresh handle only for its original client, so a lost
rotation reply does not prevent disconnecting the grant.
Key rotation, deletion, suspension, expiry or role changes invalidate its
connections. `/oauth/connections` lets a signed-in operator view and revoke
connections made under that key. Signing out clears the browser session;
it does not revoke existing MCP connections.

`oauth.json` beside the node's keys and teams persists clients, grants, browser
sessions and credential hashes through atomic owner-only writes. No raw API
keys, access tokens, refresh tokens or session cookies are persisted there.
Preserve this file across coordinator restarts. It is coordinator-local state,
not cluster-synchronized state. Protect it along with other node configuration.

The local authority bounds retained authorization URLs to 8 KiB and allows up to
64 active connections per identity binding and 128 across the authority. A
binding retains at most 16 browser sessions; successful sign-in replaces its
oldest session when necessary without revoking other identities or MCP grants.
Each connection retains at most 4,096 consumed refresh
proofs, enough for the 30-day lifetime at the normal 15-minute refresh interval;
exhaustion requires reconnecting only that client. Unused public registrations
expire after 24 hours; approved dynamic registrations retain finite expiry
through their latest grant. Expired codes and unreferenced grants are reclaimed.
Inactive client records can be evicted under cache pressure while active grants
and pending consent remain protected. Login and registration budgets use the
transport peer address, never unverified forwarded headers. Pending consent is
bounded per peer and per peer/client pair. A reverse proxy can aggregate peers,
so configure client-specific abuse controls at the ingress for remote access.
The client and browser-session collections remain bounded separately.
Slow network reads, metadata fetches and response writes do
not hold the state transaction lock. OAuth form bodies and response writes have
10-second endpoint-specific deadlines.
