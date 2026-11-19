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

Native Ollama management mutations require an administrator even on a loopback
node that permits anonymous inference. A user's permission to query a provider
does not permit model creation, deletion, copying, pulling, pushing or blob upload.

Stored native Responses bind the authenticated caller and tenant to the original
connection revision or exact run. Missing, expired, cross-owner or unavailable
origins never fall through to another backend. Anonymous callers can use native
`store:false` requests under existing inference policy. Worker calls carry only
a protected owner digest through existing mTLS routing; caller-supplied ownership
hints are stripped and the digest is not sent to the native provider.

Provider operation definitions select implemented adapters and cannot reduce
their minimum role. The initial exact-run JSON diagnostic adapter is
administrator-only. Native endpoint discovery is an ordinary read under existing
provider/model/route visibility. It returns no credential values and does not
open listeners; subsequent direct traffic uses the provider's own authentication
and bypasses Router policy and accounting. See
[provider extensibility](provider-extensibility.md).

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
random nonces and authenticated format metadata. On first startup, the node
prepares a private 32-byte key at `private/credentials.key` under its actual service
account. An explicit `security.credentials.key_file` takes precedence and is
never generated, replaced or repaired automatically. The API never returns the key.
A missing automatic key beside existing ciphertext refuses startup with recovery
guidance. Invalid explicit keys also fail closed.

Local file permissions protect the automatic key. A complete configuration-tree
copy contains both key and ciphertext, so it does not provide independent key
custody. The recovery commands exclude the key from the archive and export it
separately. Use an external secret-managed key when independent custody is required.
Back up the key and encrypted state separately and verify the matching pair during
[recovery](recovery.md). Encryption detects tampering or a wrong key,
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

People sign in with their local account and password after owner setup or an
invitation. Existing admin/user key sign-in remains an advanced option. Browser
sign-in uses the same session authority, remembers identity for up to 12 hours,
and requires consent for each client. MCP clients
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
Browser authority is opt-in on declared management/enrollment routes. Among
compatibility operations, only GET `/v1/models` and POST `/v1/chat/completions`
accept the coordinator's session; chat requires exact Origin and the session's
CSRF proof and uses the existing key/team inference policy and quotas. Invalid
browser attempts never become anonymous. Cookies confer no authority on MCP,
internal or worker routes. Browser cookies and CSRF headers are removed before
all provider and cluster forwarding. Once a browser session authenticates a
request, unrelated proxy Authorization credentials are removed there too.
Explicit native API keys keep their existing forwarding contract.

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

## Access audit and recovery boundaries

The access history in `access/access.sqlite` records committed people/enrollment
mutations, password changes, authority revocations and app-key/team/quota changes.
An event for a change to access records commits in the same transaction as the
change, so a change is never kept without its record. Usage and spend resets,
whose state lives outside the database, are recorded after their own commit,
best effort. Actors identify a person as `person:<id>`, an app key by its ID
(which never contains `:`), static installation authority as `static:<id>`, or a
verified local operator as `local:operator`. Events contain identifiers and safe change
metadata, not passwords, grant tokens, credential references or local recovery
proof. History is append-only: the database refuses updates and deletions of
recorded events.

**System → Access history** is administrator-only and reads bounded pages of
that history, newest first by event time, events of one time in recorded order,
optionally filtered by actor, subject, action and time. It does not record every
OAuth login/logout. An event's time is the coordinator's clock when it was
recorded, so an event recorded while the clock ran ahead or after it stepped
back shows that time. A filter value is matched literally as a bound query
argument, never as SQL or a pattern. With no filter, a time range, one exact
filter, or one exact filter and a time range, a page reads at most two index
entries beyond the events it returns and never sorts. With two or three exact
filters, a page walks the index of one of them, chosen in a fixed order
(subject, then actor, then action), inside the time range and checks the others
on the rows it walks, so it may read every event of that one filter in the
range before the page fills. Changes to access records wait while a page is
read, so on a very large history narrow such a query with a time range. A page
cursor survives new events and restarts; a malformed cursor, one
another store issued, or one sent with other filters is refused and the reader
refreshes. Define retention operationally; nothing prunes the history
automatically.

The database file is internal, not an interface. Inspect a backup copy, never the
live file, and change it with no other tool, its schema included: a node refuses
to start, with `integrity_failed`, on a database whose schema objects (tables,
indexes, triggers, views, or the statistics `ANALYZE` adds) are not exactly those
its release creates. If that happened, restore a checked backup: until an
activation command exists, place a staged, prepared image by hand as
[recovery](recovery.md) describes. While a tool holds the live file, each key,
team or people change waits five seconds for it and then answers `503
access_store_busy` with `Retry-After`; nothing is changed, and a retry succeeds
once the tool lets go. An access-history read answers the same way while the tool
writes. A node that cannot open its store because a tool holds the file stops
with a message saying so, not a damage reason. If the coordinator cannot even
tell whether a change was committed, it answers `503 access_store_failed` and
exits so its service manager restarts it.

Online backups (`POST /zzrouter/v1/system/state/backups`, for an administrator or
the local operator's proof) are full copies of the access database: password
and app-key hashes, teams, invitations and the access history. They live in the
configuration's `access/backups` directory, owner-only (restricted ACL on
Windows), under names the coordinator derives from an opaque ID; no request
names a path, a file or a destination, and no answer carries a path or archive
content. Retention removes them: each is kept for the 24-hour replay window,
then only the three newest, and none past seven days, within 1 GiB. The
status (`GET /zzrouter/v1/system/state`) lists every retained copy by kind,
creation time and size. They never travel in an installation bundle. Only the
local operator, with the installation's local proof on its host, can download
one (`zzrouter access state export`); no API key, session or MCP tool can, and
each download is recorded (`store.backup_exported`) before its first byte. A
copy an operator takes elsewhere holds the same credential hashes outside
retention: protect it as the installation bundle is protected. An older backup restored later can bring back
access revoked since.

Offline recovery shares an installation lock with node startup. Restore only
stages into a new directory, and never activates state across multiple filesystems.
It prepares the staged access database image so that sessions, delegated tokens
and app-key references issued before the restore stop resolving.
See [recovery](recovery.md) for included data, separate dependencies and required
post-activation checks. Restoring an old valid backup can resurrect revoked access.

A `handoff` archive (experimental) moves a coordinator to another machine. It holds
the access database image, with password verifiers, app-key hashes, grants and
the access history, and the encrypted provider connections, whose key is exported
separately as for an installation archive. It also holds routes, spend, pricing
overrides and the provider definitions and assets, byte for byte: a secret an
operator wrote into a provider definition travels with it. Protect it like an
installation archive.
It holds no private key and no node-local trust: not `node.yaml`, not the OAuth
authority state, the cluster CA and its deny list, the node identity or
`private/`, and no environment. Environment variables the dotenv files or the
service environment set (the static installation keys `ZZROUTER_ADMIN_API_KEY`
and `ZZROUTER_API_KEY`, provider credentials such as `OPENAI_API_KEY` or
`HF_TOKEN`, and every variable a provider definition reads) stay behind: set them
on the destination, where a connection that reads its key from the environment
fails without it. Workers pair again with the new coordinator, and browsers and
delegated clients sign in again. `recovery check` refuses a handoff carrying any
entry beyond its fixed list, a source, `separate` list or entry name that is not
short plain text, or no provider definition; a refusal quotes the entry name.
