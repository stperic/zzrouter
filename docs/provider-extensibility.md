# Provider extensibility

This specification describes the implemented provider-extensibility contract.
The [independent implementation review](reviews/provider-extensibility-implementation-2026-11-17.md)
records the resolved findings. Validation and deployment qualifications are in
the implementation report under `.Codex/reports/`.

## Product contract

Adding a provider, parameter or native operation within an implemented protocol
and execution adapter must not require a new public API route or MCP tool.
Provider definitions describe supported behavior; shared backend code owns
authorization, target identity, credentials, execution and accounting.

The existing OpenAI, Anthropic and Ollama compatibility APIs remain available.
The operation interfaces supplement them for provider-specific capabilities.
A genuinely new protocol, authentication scheme, state model or transport can
require a shared adapter change without changing the public discovery envelope.

The [compatibility contracts](api.md#compatibility-and-native-options) describe
native options, translation refusals and realtime admission. Cloud path
declarations and unavailable catalogs use the existing backend resolver; see
[provider endpoint configuration](configuration.md#provider-endpoints).

## Definition and evidence

Provider operation definitions live beside existing provider configuration, not
in a second registry. Each has a stable provider-qualified identity, version,
bounded binding and input/output schemas. Launch flags continue to use the
existing provider parameter schema and tier resolver.

Definitions are separate from runtime evidence. A declaration does not prove that
an installed version or model supports an operation. Discovery reports the
definition revision and effective target evidence; unknown remains unknown.
The same definition revision is checked again at execution.

An on-demand provider can declare a tokenizer diagnostic beside its existing
runtime and launch schema:

```yaml
operations:
  tokenize:
    version: 1
    description: Tokenize text with this running engine
    adapter: run_json_read
    audience: admin
    effect: read
    target_scope: run
    transport: json
    runtimes: [base]
    binding: {method: POST, path: /tokenize}
    input_schema:
      type: object
      required: [content]
      properties:
        content: {type: string}
      additionalProperties: false
    output_schema:
      type: object
      required: [tokens]
      properties:
        tokens:
          type: array
          items: {type: integer}
```

This example is the llama.cpp text shape. vLLM uses `prompt` instead of `content`
and returns `count`, `max_model_len` and `tokens`. Shipped bindings support their
base runtimes and a text-only subset. They follow the primary
[llama.cpp server documentation](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)
and [vLLM tokenizer protocol](https://github.com/vllm-project/vllm/blob/main/vllm/entrypoints/serve/tokenize/protocol.py).
An older installed engine can still reject a declared endpoint or field.

Definitions accept only implemented version/adapter/audience/effect/target/transport
combinations. The first combination is `1/run_json_read/admin/read/run/json`.
Bindings allow GET or POST with a fixed engine-relative path. Other provider
kinds cannot declare this process-run adapter. `runtimes` is explicit; `base`
means the base runtime, while named feature runtimes must exist on that provider.

Validation limits each provider to 64 operations, each schema to 64 KiB, depth
32, 4,096 nodes and 64 references. References must remain local; recursive or
external resolution is refused. Compiled validators belong to the validated
configuration snapshot. Descriptors returned to clients are detached copies.
The dialect is JSON Schema 2020-12; legacy `dependencies` is refused. Use
`dependentSchemas` or `dependentRequired` so all active schema containers pass
the same bounds.
Changing a descriptor changes its deterministic revision; no global registry or
runtime schema download participates in dispatch.

Managed definition reconciliation seeds missing shipped operation names while
preserving existing operator additions and descriptor overrides. An explicit
`operations: {}` disables seeding for that provider. Listener declarations are
release-owned engine facts. Existing operator `schema:` extensions remain the
way to add ordinary launch flags. Upgrade behavior stays in the existing template
ownership system rather than introducing another override file.

## API and MCP

Administrator operations use `/zzrouter/v1`; user operations use `/inference/v1`.
Both namespaces have the same fixed discovery and execution shapes, with existing
audience gates and operation-specific authorization:

| Route | Purpose |
| --- | --- |
| `GET /operations` | Filtered summaries of visible executable operations. |
| `GET /operations/:id` | One definition, schemas, requirements and revision. |
| `POST /operations/:id/execute` | Validate and execute against an admitted target. |
| `GET /connections` | Read native and routed connection information. |

MCP tools call these REST routes. They do not implement another inference,
authorization or provider-routing layer. Clients discover summaries and request
one detailed schema when needed rather than loading every provider's schema.

The first native-operation adapter is an administrator-only, exact-run, bounded
JSON diagnostic. Metadata cannot lower its minimum role. Native tokenizer
operations demonstrate independent provider payloads over the same adapter.
User inference continues through established compatibility handlers, including
the expanded MCP inputs. Streaming and binary transports are not implied by a
JSON operation schema.

The tool family is fixed for each audience:

| Action | Administrator MCP | User MCP |
| --- | --- | --- |
| List | `zz_operations` | `zzrouter_operations` |
| Describe | `zz_operation` | `zzrouter_operation` |
| Execute | `zz_operation_execute` | `zzrouter_operation_execute` |
| Connections | `zz_connections` | `zzrouter_connections` |

The initial user operation list is empty because no user execution adapter is
enabled. Administrator diagnostics remain invisible to user describe/execute
calls. User connection discovery is available now, under existing provider,
model and route visibility. Neither administrator credentials on the user
namespace nor user credentials on the administrator namespace change audience.

List requests accept `provider`, `node`, `model`, `runtime`, `offset` and `limit`;
connections also accept `run_id`. Node selectors are known names, never addresses.
Omitted node selects all nodes. Pages default to 50 and allow at most 100 entries.
Lists use `{data, total, has_more}`. Describe and execute use the existing
`{success, message, data}` envelope; failures use Problem Details.

Copy the version, definition revision, target and target revision from discovery
into the execute request. For example, POST
`/zzrouter/v1/operations/llamacpp.tokenize/execute` with:

```json
{
  "version": 1,
  "definition_revision": "<discovered revision>",
  "target": {
    "provider": "llamacpp",
    "node": "studio",
    "run_id": "<discovered run ID>",
    "model": "<discovered model>",
    "runtime": ""
  },
  "preconditions": {"target_revision": "<discovered target revision>"},
  "input": {"content": "Hello"}
}
```

MCP execute adds `operation_id` to the same request fields. Unknown envelope
fields are refused. The selected input schema governs nested JSON, and the
output schema validates the actual upstream result. Definition/target staleness
returns 409; clients rediscover instead of silently retrying against new state.
The initial adapter caps JSON request/response bodies at 1 MiB and execution at
30 seconds. Redirects are not followed. MCP independently retains its existing
1 MiB reply limit, which includes the REST envelope.

User MCP chat retains `model`, `messages`, `max_tokens`, `temperature` and
`idempotency_key`, with rich message objects and an `options` object for other
JSON inference fields. Embeddings retain `model` and `input`, accepting native
JSON text/token inputs and `options`. Options cannot override the fixed fields;
streaming is refused. The REST inference pipeline owns supported parameters,
permissions, budgets and accounting. The administrator chat tool remains a
bounded smoke test.

## Ordinary connection discovery

Connection information is available through both API and MCP for ordinary testing,
diagnostics, comparison or direct use. It is not conditional on zzRouter missing
a feature and does not introduce a permission ceremony or network configuration.

Replies distinguish the native engine URL, routed compatibility URL and any
browser-proxy URL. They identify the provider, node, model/runtime and run where
applicable, the listener's network scope, authentication requirements and current
lifecycle. A loopback address belongs to the named node, not the remote client.
Remote reachability remains unknown unless measured from the relevant client.
Credentials and secret-bearing URL components are excluded.

Direct requests use the native provider's own authentication and do not pass
through zzRouter's request policy, routing, accounting or logging. Reading an
address neither opens a listener nor starts or pins a run.

`runtime.listener` identifies the host launch parameter, host environment
variable and/or engine default. Discovery captures the effective host when the
run is created instead of reinterpreting it after configuration changes.
Replies include `native_url`, `routed_urls`, optional `browser_proxy_url`,
`listener`, `native_authentication`, `routed_authentication`,
`remote_reachability` and `evidence`. The latter records source and observation
time, not a successful native probe. Hosted providers use the existing connection
resolver and its revision. Stopped or record-only runs do not advertise a live
native listener.

A live process can have a known node-local upstream URL without a listener
declaration. Discovery returns that canonical address with `run_upstream`
evidence while leaving binding scope, authentication and remote reachability
unknown. It does not substitute the run's advertised host or probe the engine.

## Execution invariants

- An operation ID selects a trusted definition, never an arbitrary caller URL.
- Schemas validate shape. Existing adapters and access rules enforce semantics.
- Exact-run operations acquire the existing instance lease on the owning node.
  A stopped, replaced or mismatched run is refused without automatic fallback.
- Coordinator-to-worker execution uses the existing mTLS routing boundary.
- Schema/revision/role/target failures occur before the upstream request.
- Cancellation, bounded replies and upstream failures use existing conventions.
- Stateful Responses remain bound to their original caller and backend identity.
- Provider updates pass existing strict validation before publication and sync.

## Ownership and refactoring

| Responsibility | Existing owner extended |
| --- | --- |
| Validated definitions and revisions | `pkg/config`, typed provider schemas and template reconciliation |
| Native request binding and bounded JSON | `pkg/prov_apps/protocol` |
| Exact process identity and lease | `pkg/prov_apps/instance` and `ProviderAppManager` |
| Native credentials and hosted revision | `pkg/config/backend` resolver |
| Caller-scoped stored Responses | `pkg/dispatch/affinity` and existing dispatch observers |
| HTTP orchestration and projection | Shared `internal/server` handlers for both audiences |
| MCP | Existing REST dispatcher with accepted credential and cancellation |

The architecture adds no parallel permission engine, provider registry, run
selector or plugin language. Native Ollama mutations use the existing role gate.
Stored Responses use an immutable pre-dispatch origin and caller/tenant scope;
the worker enforces its own exact run binding through the existing mTLS path.
Lost or changed origins refuse instead of re-resolving a provider or model.

## Compatibility and validation

Operation declarations are new strict provider configuration. Protocol 25 requires
minimum 25; old nodes must not receive definitions they cannot interpret.

Acceptance covers two provider definitions and an added flag without route/tool
changes, invalid and stale definitions, exact worker targeting, rich MCP JSON,
role and caller boundaries, cancellation and truthful connection metadata.
The implementation report distinguishes synthetic local/mTLS integration tests
from live upstream-engine and deployment qualification.
