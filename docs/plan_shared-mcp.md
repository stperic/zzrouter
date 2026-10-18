# Shared MCP conventions

Owner-selected scope, 2026-10-18: move the existing sibling `zzmcp` implementation and its tests into `common/mcp`, update zztrace, and replace zzRouter's generic MCP helpers. Keep the official SDK version and product behavior unchanged.

## Ownership and existing mechanisms

- `common/mcp` owns the existing coded errors, schema validation and normalization, object results, server options, stdio/HTTP transports and per-call HTTP metadata. Move the implementation and tests together; do not add a second framework or product imports.
- zztrace changes its imports and build/setup paths to use the single sibling `common` module. Historical trial records retain their original repository identities.
- zzRouter uses shared server construction, error middleware, HTTP serving and schema normalization. Its `toolError.StructuredContent` supplies the existing RFC 9457 problem and optional probe evidence. The dispatcher retains REST routing, allowed headers, request IDs, cancellation bridging, tracing and authorization isolation.
- Keep zzRouter's SDK `AddTool` registration: it validates declared output schemas and preserves the SDK's input coercion and validation errors. Its thin adapter binds tools to REST operations and chooses product summaries. Switching to a different result/validation contract is outside this refactor.
- Shared HTTP metadata supplies headers and the original remote address per tool call. The product's outer-request state retains only its request ID, call counter and cancellation/trace context.
- The original `zzmcp` checkout becomes a historical source with a clear relocation notice. Its accepted trace records and old source revisions remain available; it is no longer a dependency of either consumer.

## Contracts and failure paths

Preserve tool names, schemas, instructions, cache hints, text and structured results, SDK validation failures, RFC 9457 fields and probe evidence. Missing HTTP/product metadata must fail before dispatch. Inner REST requests must retain cancellation and the original remote address, copy only approved headers and trace headers, and carry no outer authentication context values. HTTP limits, cross-origin checks and both supported protocol eras remain enforced by the same SDK options.

## Verification

Run the moved shared race suite, vet and build; zztrace's full `npm run verify`; router MCP package race tests, MCP HTTP/auth/wire integration tests, lint, vet and build. Retain product schema and error tests, moving only generic normalization tests to the shared package. Review the implementation independently before committing. Check fresh committed sibling clones with no `zzmcp` checkout. Publication, hosted releases and live installation remain outside this local integration.
