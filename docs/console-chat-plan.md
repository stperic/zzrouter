# Console chat

## Scope and ownership

Add Chat for both user and administrator sessions, with the TUI's model/route,
optional node, system prompt, temperature, top-p, output-token limit, streaming,
cancellation, returned reasoning and response statistics. The existing
`/v1/models` and `/v1/chat/completions` handlers own discovery, routing,
provider/model access, quota and billing. No second inference endpoint or
browser policy engine is introduced.

The console shell owns navigation, identity and page lifetime. Its shared API
owns same-origin fetch, CSRF, authentication failures and cancellation. Extract
the reusable fetch seam from JSON decoding, so a stream is not decoded as JSON
or terminated by the management request timeout. Inference is cancelable on
Stop, page exit and session invalidation; it is never automatically retried.
An authorization refusal is displayed in the conversation without discarding a
valid session. Authentication loss clears protected state as it does elsewhere.

Reusable browser chat source lives beside the console under
`internal/server/web/chat`. Vercel AI SDK 7's OpenAI-compatible provider reads
the existing SSE protocol; assistant-ui owns conversation/composer behavior.
The source accepts transport and settings from its host and has no console
navigation, credentials or routing rules. Follow GW's library primitives and
Markdown patterns without importing its CMS, tools or persistence. Pin packages
and bundle them through `scripts/console-vendor`; lazy-load checked-in assets.
Go remains the sole runtime server. Keep the console's self-only CSP.

## Browser authentication

Extract browser-only reference authentication from `requestAccess`, preserving
origin checks, CSRF mutation checks, live role/key resolution and binding.
Opt browser inference into only GET `/v1/models` and POST
`/v1/chat/completions`. Compatibility header precedence and cluster-trusted
requests remain unchanged. Explicit credential headers never fall back to a
cookie. A browser cookie or browser proof is an authentication attempt: invalid,
expired or revoked sessions never fall through to anonymous inference, even on
an installation permitting anonymous clients. Other compatibility surfaces
retain their existing behavior. Browser cookies and CSRF must not reach any
upstream, including cluster workers.

The accepted browser binding lives in the request context, shared by self-service
identity, client observation and upstream credential scrubbing. Proxy Basic or
Digest credentials accompanying accepted cookie authority never reach an upstream.
Explicit native-key authority keeps its existing forwarding policy, including
when unused browser cookies accompany that native key.

## Interaction and failures

Chat has a model/route selector and a quiet settings summary. Settings opens
the existing right-side panel with Apply/Cancel, system prompt and optional
numeric values (blank means model default). Node names come from API evidence;
the selected model identifier is sent unchanged, with X-Node only when explicitly
selected. Chat-capability evidence filters known non-chat models; no guessed
provider rules. Empty or failed discovery has an explicit refresh state.

The conversation supports safe Markdown/code, copy, regenerate, New chat and
Stop. Show elapsed time and reported usage, never invented token totals.
Returned reasoning is collapsible. Incomplete/error streams retain readable
partial output and a visible outcome. Settings snapshot at send time; changing
the draft cannot alter a running request. No tools, attachments or automatic
model selection/fallback beyond the server's existing routing. Conversation
and system prompt remain in page memory and are cleared on exit/session loss.

## Validation

- Real server tests: cookie discovery/chat, CSRF/origin refusal, explicit-key
  precedence, expired/revoked browser attempts with anonymous enabled, team
  access and limits, worker/role boundaries, upstream cookie/proof stripping.
- JavaScript tests: shared request ownership; real SDK through a synthetic SSE
  upstream, split frames, parameters/default omission, usage/reasoning, abort,
  errors/truncated streams and no automatic retry. No vendor mocks.
- Reproducible bundle, TypeScript check, existing console tests, Go race tests,
  build/vet and affected lint. Independent architecture and final diff review.
- Browser acceptance at desktop and mobile sizes: discovery, settings, send,
  streamed rendering, Stop, regenerate, new chat, navigation/session clearing,
  keyboard controls, safe Markdown and overflow. Use disposable state first.

No cluster protocol change is required. Document the additive browser contract
and report production-line growth, excluding generated assets and tests.

## Implementation and verification

The implementation follows the ownership above. The reusable source uses an
assistant-ui local runtime and an injected AI SDK model adapter. A small
`eventsource-parser` transform preserves the existing named loading events and
DONE-only endings before the SDK parses the OpenAI stream. Zod is configured
without generated code before SDK initialization to retain the self-only CSP.

Validation completed on 2026-11-14:

- 170 console JavaScript tests passed, including eight tests using the actual
  bundled SDK transport. TypeScript checking and the vendor build passed.
- Focused browser-session/chat race tests and the wider browser, compatibility,
  credential, worker-compatibility and route-access race selection passed.
  The complete `go test -race ./internal/server -count=1` suite then passed.
  `go vet ./internal/server` and the server build passed.
- Affected-package lint reports seven existing findings. The identical command
  on the unchanged base reports the same seven findings, with identical output.
- Desktop and mobile Chrome acceptance used disposable API fixtures and actual
  console assets: parameters, streaming, safe Markdown, Stop, regeneration,
  policy refusal, identity loss and failed session verification passed, with
  no page errors, horizontal overflow or CSP violations. These are synthetic
  browser fixtures; live provider inference is a separate deployment check.
- Independent architecture and implementation reviews were completed. Review
  findings about incomplete responses and Stop during failed session checks
  were corrected and covered by regression checks.
- A fresh whole-diff review caught forwarding of proxy Basic/Digest credentials
  alongside cookie authority. The shared request binding now prevents this at
  the central upstream boundary. A real-session forwarding matrix passes;
  removing that protection in a compiling overlay makes the regression fail.
  The complete server test run also identified the old OpenAPI security
  expectation, which now checks browser access on exactly the two new operations.
- Another whole-diff review found multiline SSE data was not re-emitted with
  each line's required prefix. A fragmented multiline/CRLF regression fails
  against the prior bundle and passes after preserving those prefixes.
- The final fresh review of the complete staged change found no remaining
  should-fix items and independently passed the focused Go race, eight Chat
  tests, TypeScript and whitespace checks.
- Integration with provider connection improvements passes all 183 console
  tests and both Chat and provider-setup Chrome acceptance flows at desktop and
  mobile sizes. The integration conflict was limited to additive release notes.

Chat history and its system prompt are intentionally page-local. Leaving Chat
or losing the session clears them. Tools, attachments and saved conversations
are outside this change.

Runtime production code grows by 473 lines against the deployed base, excluding
generated vendor assets, tests, documentation and build tooling. This adds the
chat view, reusable transport, styles and scoped browser authentication; the
existing SDK and UI libraries own protocol parsing and conversation state.
