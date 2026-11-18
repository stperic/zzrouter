# Cloud provider setup and chat

Status: implemented, independently reviewed and deployed as coordinator build
`0.2.3+11171947`, November 17, 2026. Live Anthropic acceptance passed.

## Intended workflow

1. Open Providers > Cloud providers and save the provider credentials on the
   coordinator. The provider card describes this connection, not worker counts.
2. Use Test connection either before saving a candidate or afterward against
   the saved connection. Testing never saves, enables or changes credentials.
3. Discover a model and choose Add cloud model. Cloud registration requires no
   download, storage estimate, local provider or compute-node selection.
4. Confirm registration from the existing deployment and model APIs, then open
   the registered model in Chat. Chat uses the same authorized model catalog and
   inference API as other clients.

## Ownership and contracts

The existing provider connection service owns credential resolution, destination
validation and probing. Testing saved credentials must resolve them on the
server without returning them to the browser. Candidate tests remain complete
replacements: blank secrets must not silently borrow saved secrets for a changed
destination. Probe results report verified, failed or unavailable evidence and
never imply that every model or billing entitlement is usable.

The existing deployment service owns registration and coordinator placement.
The console renders cloud registration separately from local download controls
while retaining the same deployment endpoint, authorization and idempotency.
Registration success is confirmed from returned state and refreshed inventory;
an accepted job alone is insufficient. A failure or uncertain write is shown
without automatically repeating it. Local download selection remains unchanged.
Cloud model details use registration and declared catalog metadata. They do not
request local weight inspection or display inapplicable storage/quantization
placeholders. Local model details retain their existing inspection and errors.

Cloud connection cards and details describe coordinator-managed connections.
Local provider cards keep their compute-node installation and enablement
observations. This presentation change does not rewrite worker configuration or
remove existing diagnostic metadata from the API.

Browser-authorized inference uses the shared upstream-header scrubber. The
browser Origin and Referer describe the console request, not the server's request
to the model provider. They must be removed at that boundary along with browser
cookies and CSRF proof. Ingress still validates the session, origin and CSRF
proof; native API callers keep their existing header contract. No vendor-specific
browser-access opt-in or browser-held provider key is introduced.

## Validation

Regression tests cover saved versus candidate credentials, authorization and
probe failure, single cloud registration, completion and uncertain outcomes,
model discovery in Chat, and the existing local download workflow. Browser checks
exercise both desktop and narrow layouts. Live acceptance uses the configured
Anthropic connection, a discovered Haiku model and small bounded requests.
Credentials and session tokens are excluded from evidence files.

Before the UI changes, live REST registration succeeded on the coordinator and
Haiku appeared in `/v1/models`. Messages, Chat Completions and Responses each
returned HTTP 200 with `OK`. These observations establish that the underlying
cloud path works; they do not establish that the original console flow is easy
to complete. Final UI and API results will be recorded in the deployment report.

A real signed-in Chrome test subsequently selected Haiku but received HTTP 401
when streaming. The same streamed API-key request succeeded. The upstream reply
identified CORS/browser access: forwarding the browser Origin caused Anthropic
to treat the server request as direct browser access. The shared scrubber fix
therefore needs browser-authority, cluster/caller sibling, native-caller and
ingress-rejection regression coverage in addition to the UI checks above.

## Verified outcome

The final real Chrome workflow tested the saved Anthropic connection (HTTP 200,
`ok`), discovered and added Haiku 4.5 (HTTP 202, completed registration), read
back its exact catalog identity, opened clean cloud details, and used Try in Chat
for a streamed HTTP 200 response containing `OK`. The browser made no unsupported
local model-inspection request and reported no script or asset failures. The
temporary Haiku 4.5 registration was removed; Haiku 5.5 and the three original
local model copies remain available. Anthropic credentials were never read back
or changed; their saved revision remains the same.

Final validation: 299 console tests, 26 Chrome workflow checks with a focused
model/layout rerun after the final ordering correction, full server tests,
focused race tests repeated 20 times, vet and pinned lint. Independent review
has no remaining findings. Compiling or loadable prior-code mutations reproduce
the missing saved-test mode, cloud Add friction, browser upstream refusal,
unsupported cloud detail request and local diagnostic ordering regression.

The increment adds 61 production Go/JavaScript lines after removing the unused
cloud node-inventory renderer and sharing probe presentation. OpenAPI and tests
are counted separately. See the [review](reviews/console-cloud-workflow-2026-11-17.md)
and [deployment evidence](../.Codex/reports/provider-priority-deployment-2026-11-17.md).
Other cloud services were not live-certified by these Anthropic tests.
