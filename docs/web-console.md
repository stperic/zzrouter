# Browser console

Open `/console/` at the coordinator's configured OAuth issuer. The default is
`http://localhost:9090/console/`. Use the exact configured hostname, scheme and
port: `localhost` and `127.0.0.1` are different browser origins. The sign-in page
links to the configured origin when the current address differs.

Sign in with a zzRouter key. Administrator keys open Overview, Providers,
Keys & teams, MCP connections and Settings. User keys open My Access, with their
own model policy, limits, recorded usage and MCP grants. Keys remain the identity
model; the console does not introduce a separate account or role database.

## Connect providers

Providers includes disabled cloud services and registries, including Hugging
Face. Forms come from the provider's declared connection schema, including
endpoint, region and account fields where applicable. Local providers without
an editable connection offer terminal setup guidance.

Enter a complete replacement, including each required secret. Saved secrets
cannot be read back or borrowed when changing a destination. Test is a diagnostic
for the current form content. It keeps masked inputs for an explicit Save;
editing any field invalidates the displayed test result. Saving does not test
the upstream. Save and connect additionally requests existing provider onboarding.

Results report saving, durability, activation and publication separately.
Reload after a successful write, stale revision or uncertain response before
making another change. Remove suppresses external fallback. Use external values
explicitly resumes the coordinator's declared environment or credential cache.

A Hugging Face token is optional for public models. Its diagnostic may reject an
empty token even though anonymous public browsing works. A successful token
check does not establish access to every gated repository; approval may also be
needed. Managed saving requires `security.credentials.key_file` at startup.

## Manage access

Create a named key for each person or client. Select a shared team at creation
to apply model allowances and shared quotas, or let the service create a personal
team. Key membership cannot be changed later. Replace the key to move teams.
Personal-team policy cannot be edited independently.

Keys can be renamed, given limits or an expiry, suspended, resumed, rotated and
revoked. Shared teams support model allowances, limits and suspension; deletion
requires no remaining member keys. Empty model allowances mean all models.
Zero quota values mean unenforced at that scope. Both key and team policies
remain applicable. Existing expiry can be extended or replaced, not cleared.

Editors send only changed fields and check for changes to those fields before
submission. The access API has no atomic revision guard; concurrent editing
still requires coordination. New and rotated key values are shown once and
cleared on dismissal, navigation, expiry or a changed browser session. Returning
from a password manager keeps the value when the browser session is unchanged.
Rotating your current key shows a recovery disclosure for the new value even
though the old key no longer authenticates. Copy and save it before leaving.
If a response is lost,
reload the keys list and rotate an existing key whose value was not received.
If it was your only administrator key, use another administrator or local
terminal recovery. That recovery guidance survives failed session refreshes and
retries until a successful sign-in. Never automatically retry key creation.

While a durable change is pending, navigation, editor Close, provider switching
and sign-out are disabled. The browser warns before unloading the page. An
interrupted or unreadable response leaves a generic unknown-outcome notice even
after an authentication change; inspect current state before making another
change. A diagnostic provider Test remains cancelable because it saves nothing.
If a session check overlaps a new-key response, the value stays hidden until
the check succeeds. Verification failure offers a read-only retry while keeping
the value only in transient memory. Changed access or expiry discards it. If
that was your own replacement key, recovery guidance remains available without
starting another refresh or retrying the rotation.

Administrator keys authorize management MCP. Inference MCP requires a separate
user key. MCP connections shows issuer-derived URLs and owned grants. Signing
out ends browser access; it does not revoke existing MCP grants.

## Architecture and boundaries

The console is embedded HTML, CSS and native JavaScript modules on the existing
coordinator listener. It has no frontend build dependency or additional server.
`browser_console.go` serves a fixed asset allowlist with same-origin CSP and
no-store headers. All data and changes use existing services through their HTTP
APIs. The TUI remains the interface for local installation, model/runtime work,
jobs and installation recovery.

`api.mjs` owns requests and session refresh; page and form lifetimes reject late
responses. A refresh clears protected content before loading bootstrap and `/me`,
then confirms the bootstrap proof is unchanged before exposing either identity
or CSRF. Focus, bfcache restoration and cross-tab auth events recheck authority.
Focus probes preserve the existing view when issuer, session-bound proof and
authentication still match; they never install a new proof under old content.
A failed probe keeps the form or disclosure but pauses new changes until a
successful verification. Explicit sign-out remains available when no write is
pending and clears protected content before sending the logout request.
Changed authority and cross-tab auth events clear the
view before a full refresh. The own-key rotation response and its disclosure
allow only the expected unauthenticated transition on the same issuer and proof.
Rejected or interrupted mutations are never replayed. The server's session-bound
CSRF remains the final guard if browser identity changes after the last read.

No credentials are kept in browser storage, URLs or application logs. Login
input clears immediately. Provider candidates live only in the active form;
masked values clear on successful saving, navigation and session invalidation.
Cross-tab messages carry only an authentication-state event.
Pagehide scrubs transient secrets; a full reload does not preserve values or
in-memory outcome notices.

Settings shows effective public URL and authentication facts. Listener, issuer,
TLS and protection-key configuration remains a local installation concern and
requires restart. See [configuration](configuration.md#mcp-browser-login),
[security](security.md#mcp-browser-authorization), and
[client architecture](client-architecture.md).

## Validation

Run `node --test internal/server/web/console/tests/*.test.mjs` for dependency-free
adapter tests. Go integration tests exercise fixed assets, coordinator gates,
safe policy projections and protected credential operations through the real
server constructor. Browser acceptance uses synthetic provider upstreams and
disposable state; live vendor access and tunnel policies are separate checks.
