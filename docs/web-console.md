# Browser console

Open the coordinator's root URL in a browser to reach `/console/`. The default is
`http://localhost:9090/`. Direct links to `/console/` also work. Use the exact configured hostname, scheme and
port: `localhost` and `127.0.0.1` are different browser origins. The sign-in page
links to the configured origin when the current address differs.
For remote access, follow the [HTTPS ingress checklist](mcp.md#remote-https-checklist)
before signing in.

Sign in with a zzRouter key. Administrator keys open Overview, Nodes, Workloads, Clients, Providers,
Routes, Keys & teams and Settings. User keys open My Access, with their
own model policy, limits, recorded usage and MCP grants. Keys remain the identity
model; the console does not introduce a separate account or role database.

## Shared selection and editing

Overview, Nodes, Workloads, Clients, Providers, Routes and Keys & teams share one nonmodal
right-side panel. Select a graph icon, card or table row to inspect details. A
native name button also supports keyboard selection. The selected item remains
highlighted. Editable resources expose one Edit action inside the panel; Save
changes and Cancel use the same footer. Create/Add actions open this same panel.
Clients show observed activity and a connection guide; installation settings
remain read-only where no management API exists.

Save reads the current resource and updates its list summary before returning to
details. If that read fails, the panel retains the completed-write result and
offers a read-only retry. Partial provider outcomes and uncertain writes stay
explicit. Key creation/rotation still shows the verified secret once before
returning to details. Nested team creation preserves the unissued key draft.

Cancel discards the editor and returns to details; Close or Escape clears the
selection. Dirty drafts require confirmation before navigation, refresh or
sign-out. Browser Back/Forward preserves the visible draft if discard is declined.
Pending writes block navigation and trigger the existing unload warning. Slow
refresh/recovery responses cannot replace a newer selection or draft. Search,
pagination and the map viewport remain intact while opening and closing panels.
At narrow widths, the panel occupies the workspace and Close returns to its list
or map. Forms share readable control sizes and a consistent action footer.

## Overview and node details

The Overview maps Clients to the Coordinator, then to Compute nodes and Cloud
providers. Each node, client and provider is clickable. Local compute has
priority: every known machine appears under Compute nodes with observed health.
Local providers appear beneath their owning compute node unless the API reports
them disabled. Disabled providers remain available in node settings and through
their details links. Unknown enablement remains visible if inventory is unavailable.
The tree does not infer installations from the provider catalog.
Down nodes remain visible. The header
counts unique machine identities, including the coordinator once.

The coordinator appears twice visually: once for coordination and routing, then
again under Compute nodes with the compact label Worker. Details and List view
show the full role Coordinator + Worker. Both representations
open the same machine. This does not create a second node or duplicate capacity.
The sidebar uses Nodes; Worker is a role, not a separate inventory category.
See [project terminology](client-architecture.md#project-terminology).

Clicking an item opens a nonmodal panel to the right. The map stays interactive;
clicking a different item replaces that panel without rebuilding the map or
resetting zoom. When the panel narrows the map, it pans just enough to retain the
clicked icon and label. Close or Escape returns focus to the map or selected list item.
On narrow screens the panel replaces the map workspace and keeps its Close control visible.
Zoom controls, drag to pan and Fit adjust the viewport. List view provides the
same entities and actions for keyboard and screen-reader use, and remains usable
if the graph package or layout fails.

The map uses larger, darker system-font labels and a taller canvas so Fit remains
readable for a typical cluster. Labels are clickable and mask connectors behind
the text. Compute cards contain the icon, status and memory readings in one
clickable surface. Long card titles truncate; details and List view retain the
full name. Other labels wrap at spaces and hyphens and reserve measured layout
space, including when a display name changes during refresh. Fit still
shows the entire network; larger inventories can use zoom or List view.

Status dots sit at the top-right of each icon: green for observed healthy nodes
or clients active at refresh, red for down, amber for degraded, and gray for
unknown or historical observations. Text labels preserve the meaning independently
of color. Cloud configuration and local enablement alone do not establish health.
Provider marks identify every shipped local/cloud provider and Hugging Face;
unknown custom providers use a generic mark. These marks appear consistently in
the map, its list, provider lists and node details.

Compute cards show measured used/total memory below their header. RAM and discrete
GPU cards are separate; unified memory appears once. Details list every card.
Unavailable readings remain explicit, including older peers and unsupported
per-card probes. Compact summaries omit GPU devices with neither measured usage
nor a reported capacity; details retain them. Measured zero remains zero. The console never derives used
memory from available memory. An asterisk marks last reported readings; details
show their observation time and age. Public-only health responses preserve the
last authenticated inventory without renewing its timestamps.

A gentle pulse indicates fresh admitted-request or engine-probe activity, with
matching text in the list and details. The map has no separate activity text.
A loaded service alone is not active
work. Pulses and activity labels expire independently of refresh. Pause updates,
hidden pages and reduced-motion preferences stop animation. Metric updates keep
the existing layout and viewport.

Node details show hardware, last successful observation, address, local provider
installation/enablement and reported service state. Unavailable reads and unknown
health remain explicit. Details for an unhealthy node are last reported data,
not evidence that a model can run. A missing service observation does not mean
stopped; on-demand providers need not run continuously.

Clicking a local provider opens its installation, enablement and service observation
for that specific machine, with node health and last-seen context. Identical
providers on different machines have distinct selections and links. List view
includes the owning machine in each provider label. Edit node settings uses the
same node-scoped editor; Cancel and successful Save retain the selected provider context.

Edit node settings changes provider enablement on the selected node through the
existing node-addressed PATCH API. Edited fields are rechecked before writing;
that API has no atomic revision guard, so concurrent editors still require
coordination. Node names are certificate-bound identities, not editable labels.
Provider installation remains in the terminal client.

+ Add node accepts the HTTPS cluster address, fingerprint and pairing code printed
by the new node's terminal pairing workflow. The browser authenticates enrollment
as the coordinator administrator with exact-origin CSRF protection. The response
confirms pairing; a separate node observation confirms health. Delivery may
continue after an interrupted response, and an error can follow commitment.
Inspect Nodes before retrying. Pairing is never automatically resubmitted.

Overview refreshes every 15 seconds while visible and idle. Automatic refresh
pauses while a panel is open, keyboard focus is in the list, or a change is
pending. Status updates keep positions, zoom and pan; membership or label-size
changes rerun layout while retaining the viewport. Explicit Refresh updates details
and asks before discarding a dirty editor. Node saves wait for current inventory
and return to the same overview selection.
Refresh remains explicit outside Overview and Workloads. Status means the displayed observation,
not an upstream inference test. Hash links preserve selected nodes/providers and
browser Back/Forward; pending writes prevent leaving the editor. All entity
views use the same nonmodal panel.

## Workloads and diagnostics

Workloads has Runs and Jobs views, a compute-node filter and search. Each row
opens the same details panel. Node/provider details link to that node's workloads.
Reads address each node explicitly with bounded concurrency. Nodes reported down
are skipped with one quiet offline summary. Refresh updates node health and resumes
reads when they return. Unexpected read failures remain named, and previously read
rows are marked last reported. A missing-node selection never broadens silently.

Runs show models and node services, their provider, node, status and observed
activity. Retained pins are stopped records. Details include reported memory
commitments, failure/refusal evidence, evictions and any native browser link
returned by the API. Environment values and launch commands are not displayed.
Start run opens the shared form panel with complete local catalog models and
providers that report a managed run lifecycle. Preview uses the existing run
assessment; explicit Start uses an idempotency key and disables automatic
deployment. Acceptance and job completion do not establish readiness: the panel
reads the exact run on its owning node. Stop confirms the exact node/run identity,
then reads it back. Interrupted writes retain a read-only inspection result and
are never automatically replayed.

Jobs show actual step counts, percentages, bytes and download speed when reported.
Missing progress remains indeterminate. Older jobs load explicitly and deduplicate
by node and job ID; the view discloses that changing event order can shift page
boundaries. Details include errors, warnings and the retention deadline. A quiet
job is not classified as failed, and a missing job does not imply success.

Workload snapshots refresh every five seconds while visible and idle, with Pause
updates and Refresh controls. Reads do not overlap, replace focused controls or
overwrite a draft. Failures stop automatic polling until explicit Refresh.
Provider details retain lifecycle state, backoff, readiness-job and last-exit
evidence. Node details retain sanitized transport errors. Route previews explain
the API's denied/skipped reasons while preserving the unchecked readiness,
capacity and inference limits. Shared errors show safe codes, node/request IDs
and bounded field evidence as text.

## Observe clients

Clients shows admitted authenticated API/MCP request activity grouped by key
source, key ID, protocol and authentication method. Give each application its own
named key for clearer identification. Map, list and detail labels explicitly show
`Key: {name}`, or `Key ID: {id}` when unnamed. The name is from the last observed
request. Several apps sharing one key can appear as
one activity identity; this is not a unique device or persistent session count.
Active at refresh means requests were in progress at the displayed snapshot;
otherwise the last observed activity is shown. Historical activity does not
prove current authorization. Browser console polling and nested MCP/internal
requests do not create clients or inflate counts.

Observations live only in coordinator memory, expire after 24 idle hours, and
reset on restart. At most 1024 groups are retained; active groups are not evicted.
Capacity omissions are disclosed. No credentials, addresses or request content
are retained. GET /zzrouter/v1/clients is coordinator-admin-only.

Connect client opens endpoint instructions and Your authorizations. Those grants
remain scoped to the signed-in key, even for an administrator. Grant counts are
separate from observed activity and never presented as connected clients.

## Connect providers

Providers opens Local providers first, followed by Cloud providers and Model registries, including
disabled services and Hugging Face. Select a local provider to see its reported
installation and enablement on each node. Use the terminal client for installing
providers and managing local runtimes. Any editable connection still belongs to
the coordinator; it does not change a worker's credentials.
Enablement and installation flags describe the node's effective provider state.
They do not distinguish every installation phase or say whether a model is
running. Use the terminal job view for installation progress.

Cloud and registry forms come from the provider's declared connection schema,
including endpoint, region and account fields where applicable.

Selecting an unconfigured cloud provider or registry opens its setup form
directly. Enter a complete replacement, including each required secret. Saved
secrets cannot be read back or borrowed when changing a destination. Show/Hide
reveals only the value currently being entered; it is still cleared on saving,
navigation and session invalidation.

Test connection checks the current form without saving or enabling anything.
Editing any field invalidates its result. Save and enable stores credentials and
requests normal onboarding for a disabled cloud provider. Save changes updates
an enabled provider without enabling it again. Advanced settings includes Save
without enabling, credential removal and explicit installation-credential
restoration. A configured but disabled provider can be enabled from its details
without entering its credentials again. Saving is independent of testing.
The terminal connection form's Save and connect action also requests provider
onboarding.

Results show a short outcome and retain the separate saving, durability,
activation and publication facts under Save details. Connection details keeps
credential source and node scope out of the main setup flow.
Successful writes automatically read current details. A stale revision or uncertain
response requires an explicit read before making another change. Remove credentials
suppresses external fallback. Use installation credentials explicitly resumes the
coordinator's declared environment or credential cache.

A Hugging Face token is optional for public models. Its diagnostic may reject an
empty token even though anonymous public browsing works. A successful token
check does not establish access to every gated repository; approval may also be
needed. Managed saving requires `security.credentials.key_file` at startup.

## Manage routes

Routes are stable model names backed by one or more destinations. The Routes
workspace shows providers, compute nodes, observed destination health and assigned
teams. Select a route to inspect its right-side details, then choose Edit to change
its description, selection strategy or destinations. Create route adds a reusable model name; clients send
that exact name as their `model`. A cloud destination may be configured on a
worker as well as on the coordinator.

Priority tries destinations in order. Least loaded and Fastest observed use the
existing live strategy implementations; they can choose a cloud destination
whenever it is eligible. The editor preserves advanced settings it does not
expose, including request parameters, health configuration, timeouts and tags.
Saving an automatic route claims it for manual management. A route with assigned
teams cannot be deleted through this API until those assignments are removed.

Teams choose Direct models & routes, Assigned routes only, or No inference.
Existing teams retain direct access unless changed. Assigned routes only blocks
direct physical model names, provider/node pins and opaque operations that cannot
prove a route. Provider permissions and model restrictions apply to each
resolved destination; spending and usage limits remain shared team policy. Keys
inherit the policy without individual route selectors. Model discovery publishes
authorized route aliases, including OpenAI model introspection and Ollama tags.
Ollama show returns route topology, not an arbitrary replica's weight metadata.

Preview as team is a read-only API check against the saved route. It uses current
team access, pricing, shared budget, health, cooldowns and the live strategy. It
never sends inference, starts a model, reserves money or consumes quota. It does
not test key-specific limits, rate/concurrency limits, request compatibility,
readiness or memory. Its cost is a token-based projection, not a provider invoice.
Edits invalidate previews; configuration versions prevent old destination labels
from describing a newer API result.

The implementation extends existing `/model-groups` and `/teams` APIs. `route_access`
is `{ "mode": "direct" }`, `{ "mode": "selected", "routes": ["coding"] }`, or
`{ "mode": "none" }`. An omitted policy preserves direct access; selected requires
at least one current route. Explicit null or malformed policies are rejected,
including in persisted team configuration. Unrelated edits retain a missing saved
assignment. A missing, empty or expired assigned route fails closed; recreating
its name restores the assignment. The assignment/deletion guard is not a
cross-store transaction, so concurrent configuration edits or expiry can leave a
dangling assignment, which cannot grant direct-model access.

Route GET supplies a configuration ETag. Console updates/deletes send `If-Match`;
creation sends `If-None-Match: *`. Conditional checks run inside the group store's
mutation lock. A stale write returns 412 and requires reload. Advanced API users
can still make unconditional writes. These additive coordinator contracts do not
change worker request shapes. Provider binding separately requires protocol 23
as described in [cluster compatibility](protocol_versioning.md).

## Manage access

Keys and Teams have searchable lists with friendly names, access, budgets and
status. Selecting a row opens details; Edit opens the same panel’s editor. Key details show the inherited team
policy, shared budget and usage limits; team details link to member keys. Existing
individual key limits remain visible read-only when configured. A narrow screen
shows the panel at full width with a Close control to return to the list.

Create a named key for each person or client. Enter its name first; the console
generates an editable stable ID. Select a shared team, or create one without
leaving or losing the key draft. Teams own provider access, model permissions,
budgets and usage limits. Keys inherit these rules and show a read-only preview
before issuance; they do not repeat the policy controls. Use a dedicated team
when one key needs different rules. Membership cannot be changed later; replace
the key to move teams. Existing personal keys remain inspectable and retain
credential lifecycle actions. Their personal-team policy is immutable; create
a replacement key assigned to a shared team to configure its access.

Shared teams choose All providers, Selected providers or No providers. Selected
uses searchable checkboxes grouped into Local providers and Cloud providers,
with provider marks and retained choices for providers missing from the catalog.
These permissions use exact provider identities across the cluster, not machine
placement. Optional model narrowing provides searchable model/group choices and
manual exact-name entry. Empty model allowances mean all models within the
allowed providers. An empty Selected providers selection is an error, never an
unrestricted policy. A legacy omitted provider policy means All providers.

Provider policy applies before inference routing preferences and group fallback,
to compatibility discovery, and to known opaque destinations. Restricted teams
cannot use opaque MCP mounts with no identifiable provider. Invalid persisted
team policies stop startup rather than dropping access restrictions. A team
that restricts providers also disables anonymous compatibility access. Provider
browser sessions capture the team policy at sign-in and retire if it changes.
The existing 15-second authorization watcher closes active connections as well
as rejecting new requests; reordering an unchanged provider set preserves access.

Team budget controls keep No limit/Set limit, USD and calendar period together.
The budget is shared by all member keys. Zero means no spending limit, not cloud
access disabled. Existing API-configured individual key limits still apply in
addition to the team limits; the console displays them read-only and never
silently clears them. Budget summaries show recorded spending, in-flight reservations,
available amount and the next UTC reset. Untracked or unavailable usage is
labeled explicitly instead of presented as zero. An existing accounting window
keeps its period until rollover; the usage response supplies that current window.
Limits cover metered requests through zzRouter across the allowed providers,
not separate per-provider allowances or the provider account's full invoice.
Requests reserve estimates and later settle actual costs. Unknown-priced
requests are refused under a budget; explicitly free requests can continue
after paid budget exhaustion. Rates and reservation settings are under Advanced
usage limits in the team editor.

Keys can be renamed, given an expiry, suspended, resumed, rotated and
revoked. Shared-team changes apply to every member; deletion requires no member
keys. Existing expiry can be extended or replaced, not cleared.

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

The shared browser inventory view reads the existing `/nodes` and paginated
`/providers` APIs. Provider categories and coordinator credential configuration
come from `/providers/catalog?kind=all`. Presentation joins these existing
observations by node and provider identity; it adds no aggregate service or
second health-check mechanism. Both Overview and local-provider details use
the same node-health and stale-inventory presentation.

The topology uses locally bundled Cytoscape.js 3.35.0 and ELK.js 0.12.1. ELK runs
in a same-origin worker; no CDN or runtime package installation is needed.
The CSP retains self-only scripts and explicitly permits self-only workers.
Provider SVG marks come from [Lobe Icons](https://github.com/lobehub/lobe-icons)
1.102.0, with project marks for MLX, llama.cpp and Strata. A closed provider-key
mapping selects fixed embedded assets; labels cannot introduce remote URLs.
Licenses and source notices ship in `/console/vendor/THIRD_PARTY_NOTICES.txt`.
Maintainers regenerate assets with `npm ci --ignore-scripts` and `npm run build`
in `scripts/console-vendor`. Normal Go builds embed the checked-in outputs.

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

Run `node --test internal/server/web/console/tests/*.test.mjs` without installing
packages. Tests include the actual vendored graph core and ELK layout, stale
completion guards, viewport preservation, panel/history lifetimes and editors.
Go integration tests exercise fixed assets, worker CSP, coordinator gates,
safe policy projections and protected credential operations through the real
server constructor. Browser acceptance uses synthetic provider upstreams and
disposable state; live vendor access and tunnel policies are separate checks.
