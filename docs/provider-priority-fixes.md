# Provider compatibility priority fixes

Status: implemented, independently reviewed and deployed as `0.2.3+11171843`
on the coordinator and physical Linux/Windows workers, November 17, 2026.
The subsequent [console cloud workflow](console-cloud-workflow.md) is deployed
as coordinator build `0.2.3+11171947`; workers retain the compatible priority build.
Validation and qualifications are recorded in the
[implementation evidence](../.Codex/reports/provider-priority-fixes-2026-11-17.md).
Live results and outstanding cloud qualification are recorded in the
[deployment evidence](../.Codex/reports/provider-priority-deployment-2026-11-17.md).

This change addresses serving-correctness findings in
[the refreshed comparison](../reports/Provider%20parity%20after%20deployment.md).
The starting deployment is `0.2.3+11171617`. The feature worktree also contains
earlier work; validation and independent review must isolate this increment.

## Implemented result

| Finding | Resolution |
| --- | --- |
| Gemini/Bedrock URL composition | A cloud path declaration is resolved once and shared by direct, fallback, stored-response, default-backend and realtime paths. |
| Bedrock catalog/probe claims | Explicitly absent catalog returns 501 `model_discovery_unavailable`; absent connection probe returns `unknown` / `probe_unavailable` without HTTP. |
| Translation loses controls | Responses maps reasoning effort, strict tools and text format/verbosity. Both translators preserve native JSON options and protect reconstructed controls; unrepresentable requests are refused. |
| Count-token limit bypass | The existing unmetered count probe owns its one-token limit. Caller aliases and streaming controls cannot override it; ordinary Messages rejects conflicting limit aliases. |
| Completion prompt shapes | Text, text batches, token IDs and token batches pass without numeric precision loss. Native omitted/null/empty-string defaults remain valid. |
| Realtime GA helper | Configured realtime backend and existing opaque admission serve native session bodies, including nested models and transcription requests. |
| Ollama worker import | Blob HEAD/POST, pull and create share streaming mTLS ownership. Conflicting, unsupported or unreachable targets fail before local mutation. |
| Capability accuracy | Gemini declares embeddings. Version-sensitive local Responses and other unverified declarations remain qualified. |

There is no new public API/MCP family, worker transport or cluster protocol
version. Implementation stays in existing backend, translator, configuration
reconciliation and Ollama service owners. Compared with the starting source
snapshot, production Go changes total +656/-341 lines (net +315).

## Ownership and approach

1. Cloud destinations: provider configuration declares its OpenAI-compatible
   path. `pkg/config/backend` owns composition for direct dispatch, fallback,
   stored Responses and default-backend forwarding. Native mounts remain raw
   vendor paths. Credential snapshots and connection revisions include routing
   configuration that changes a destination. No engine-name branches.
2. Cloud discovery: the existing model-listing declaration must distinguish its
   default from an explicitly unavailable catalog. Bedrock must not probe an
   endpoint AWS does not implement or claim credentials were verified without
   evidence. Keep connection testing and search truthful independently of
   inference availability.
3. Protocol fidelity: existing Responses and Messages translators own the
   conversion. Preserve fields with equivalent wire semantics and explicitly
   refuse requested behavior that cannot be represented. Native request options
   stay outside shim validation. Complete the existing completion parser for native text,
   token and batch prompt forms. Keep count-token probes bounded despite native
   limit aliases. Route realtime HTTP helpers through the configured realtime
   backend and existing opaque admission policy, preserving native session bodies.
4. Ollama mutations: the existing service, model resolver and coordinator mTLS
   forwarding own create/blob placement. Use one selected node throughout
   import, preserve streaming and status, refuse conflicting/unknown targets,
   and keep administrator authorization and catalog publication intact.
5. Capability declarations: refresh only evidence-backed, unconditional
   declarations. Version/runtime/model restrictions must not be replaced with
   a broad capability promise. Keep the known Messages image regression guard.

## Contracts and failure paths

- REST and MCP keep shared authorization, budgets, credentials, model access
  and cancellation through the same handlers.
- Native mounts keep raw vendor paths and direct URLs keep their documented
  escape-hatch semantics.
- Invalid node selection must never silently mutate the coordinator provider.
  Blob upload remains streaming under existing request limits.
- Translation refusals identify the unsupported parameter before inference.
- Preserve operator overrides when reconciling bundled configuration.
- Evaluate provider-sync changes against `docs/protocol_versioning.md`. Cloud
  configuration is coordinator-owned and is not synced. Existing worker
  compatibility routes can carry node targeting without a new internal API.

## Validation

Regressions capture composed URLs, request bytes and authentication; cover
fallback and saved-response siblings; assert unavailable discovery issues no
HTTP request; exercise rich completion shapes and translated-field refusals;
and use mTLS test workers for create/blob placement and failure propagation.
Official upstream contracts guide fixtures; synthetic replies remain labeled.
Affected race suites, module vet, pinned lint, build and console provider tests
are run alongside an independent review of the incremental diff. The
[review report](reviews/provider-priority-fixes-2026-11-17.md) records two resolved
major findings, three resolved minor findings and no outstanding findings.

Compiling, isolated Go-overlay mutations confirm the regressions detect broken
cloud URL composition, narrowed completion input, dropped reasoning controls,
count-number precision loss, realtime admission bypass and both token-limit
alias defects. New mTLS import/streaming cases passed 20 repetitions under the
race detector before their names were aligned with the Integration selector.

Broader operation manifests, stateful LoRA/slot APIs, training and new MCP
transports remain separate capability work. This pass does not claim live
cloud certification without actual upstream credentials and successful calls.
Azure's existing catalog and health declarations are unchanged and still need
live qualification. Native Responses declarations are not broadened based on a
newer lab binary when the bundled install version differs. Physical worker import
and cloud acceptance results belong to the deployment evidence above.


## Live follow-up: request cap errors

The first physical import proved worker upload, create, catalog ownership and
cleanup. Its tiny GGUF could not generate under the deployed Ollama `q8_0` KV
cache setting because its attention heads were only eight elements wide. A
published Stories42M replacement has 64-element heads, but its 39,680,864-byte
upload crossed the coordinator's intentional default 10 MiB request cap. A
private HTTP reproduction confirmed that streaming this capped body returns a
wrapped `http.MaxBytesError`; Ollama received only the capped prefix. The current
forwarder mislabeled that refusal as a 502.

Architecture plan: preserve `node.max_request_size` as the ingress owner. The
shared middleware will answer a known `Content-Length` above its configured cap
with the path's existing 413 responder and abort before route execution. The
Ollama streaming owner will recognize wrapped `http.MaxBytesError` values and
report 413 using the actual limit for unknown-length/chunked bodies. Other
transport failures retain 502, and native upstream statuses remain unchanged.
There are no path exemptions, new settings, cap increases or worker ingress
policy changes. A chunked upload can reach the upstream before its cap is known;
its bounded incomplete body must not be reported as a successful blob.

Validation will use real HTTP ingress with the actual shared middleware and
streaming forwarder: known oversized bodies never reach the upstream, unknown
oversized bodies return 413, exact-limit and ordinary streaming requests pass,
and ordinary routes keep the same cap and dialect. An isolated compiling
mutation check will prove each refusal assertion discriminates the fix.

The two OpenAI upload readers also report the `http.MaxBytesError.Limit`
that actually refused a nested reader, instead of claiming the larger outer
protocol upload cap. Focused unknown-length tests will place a smaller shared
bound inside each reader and assert that the returned detail names that bound.

Independent review found that the new terminal known-length refusal would run
before the existing CORS middleware. The shared engine order is adjusted to
read deadline, existing CORS policy, size limiter, then cluster detection. Thus
Slowloris protection precedes every terminal branch, allowed browser origins can
read both early and streamed 413 replies, and denied origins still receive no
allow-origin header. Tests call the actual engine initializer for allowed and
denied origins with known, unknown and under-cap request lengths.

The same early boundary also needs correlation/version metadata before it can
refuse. Existing RequestID and APIVersion middleware move to one shared public
engine registration after protected-header scrubbing, responder selection and
recovery, before observability and terminal middleware. Redundant public group
registrations are removed. Independent internal and worker compatibility
engines retain their own registrations. Actual-engine tests check echoed and
minted IDs on management 413s, authentication errors, 404/405 and normal replies,
and verify a production compatibility group does not mint a second ID.

Implemented follow-up: production Go changes total a net seven added lines
against the exact pre-follow-up snapshots, including shared metadata ownership
and CORS ordering. The final cap, streaming, CORS and metadata regressions passed
20 repetitions under the race detector (11.964 seconds); server vet passed.
Independent review also passed its focused race and vet checks. Six isolated,
compiling Go-overlay mutations failed their intended assertions: removing the
known-length preflight, removing streamed size-error classification, reporting
the wrong outer upload cap, moving CORS after the terminal size check, omitting
common metadata, and reintroducing a group-level request-ID remint. Overlay
verification never rewrote the working production files. Final pinned lint and
the broader source/build gates are recorded with the independent review and
release verification.
