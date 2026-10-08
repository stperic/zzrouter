# MCP for coding agents

zzRouter exposes `POST /zzrouter/v1/mcp` on the coordinator for Claude Code and
Codex. Tools call the existing REST handlers with your admin key; validation,
cluster routing, jobs and errors are the REST contract. Workers do not expose
this endpoint. Both 2026-07-28 and legacy 2025-06-18 clients are supported.

## Connect

Make `ZZROUTER_ADMIN_API_KEY` available to your client through your normal
credential channel, and replace `<coordinator>` with its hostname.

Claude Code:

```sh
claude mcp add --transport http zzrouter \
  http://<coordinator>:9090/zzrouter/v1/mcp \
  --header "X-API-Key: ${ZZROUTER_ADMIN_API_KEY}"
```

Codex: add this to `~/.codex/config.toml` (or your project configuration):

```toml
[mcp_servers.zzrouter]
url = "http://<coordinator>:9090/zzrouter/v1/mcp"
env_http_headers = { "X-API-Key" = "ZZROUTER_ADMIN_API_KEY" }
tool_timeout_sec = 60
default_tools_approval_mode = "approve"
```

Non-interactive `codex exec` rejects tools not marked read-only unless
`default_tools_approval_mode = "approve"`. For interactive sessions use
`"writes"`, which prompts only for non-read-only tools. A per-tool override:

```toml
[mcp_servers.zzrouter.tools.zz_update_apply]
approval_mode = "prompt"
```

These policies control client approval; the server still enforces your API key.
See the [Codex configuration reference](https://developers.openai.com/codex/config-reference).

`env_http_headers` maps header names to environment variable names. See the
[Codex MCP configuration](https://developers.openai.com/codex/mcp) for these
settings. Codex versions exposing the optional `mcp_2026_07_28` feature can
opt into the modern protocol; legacy mode also works:

```toml
[features]
mcp_2026_07_28 = true
```

Merge this into an existing `[features]` table rather than declaring it twice.

## Tools by task

| Task | Tools |
|---|---|
| Cluster and provider inventory | `zz_overview`, `zz_nodes`, `zz_providers` |
| Provider installation | `zz_provider_install` |
| Model discovery and download | `zz_model_search`, `zz_model_files`, `zz_model_download`, `zz_models` |
| Run lifecycle | `zz_run_preview`, `zz_run_start`, `zz_runs`, `zz_run_restart`, `zz_run_stop` |
| Smoke test and diagnosis | `zz_chat`, `zz_logs` |
| Follow or cancel work | `zz_job`, `zz_job_cancel` |
| Parameters | `zz_params_get`, `zz_params_set` |
| Updates | `zz_update_status`, `zz_update_check`, `zz_update_apply` |
| Other REST tasks | `zz_api_search`, `zz_api_get`, `zz_api` |

Start with `zz_overview` and `zz_providers`. Provider inventory includes disabled
inference providers and excludes model registries. `managed=false` means no
installed copy is owned by zzRouter; it does not forbid installation.
Successful installation enables the provider during finalization, before the job completes.
For an existing installation, use `zz_api` to PATCH `/zzrouter/v1/providers/{name}` with
`{"enabled":true}` and a `node` query when needed. Cloud onboarding uses POST
`/zzrouter/v1/providers/{name}/verify` after configuring credentials.

Check `zz_models` first for an available local model. Only if it is absent,
search with `zz_model_search`; for a HuggingFace GGUF, choose a file and size
with `zz_model_files`. Download with `zz_model_download` and follow the jobs,
then read `zz_models` on the target node for the exact local name before
`zz_run_preview` and `zz_run_start`. Existing catalog `details.wire_endpoints`
and `details.features` report serving capabilities when known; they are not
a guessed model task. The download's registry model name
is not necessarily the local name. A `node` input is a node name, never an
address. Lists default to 50 results; use `limit` and `offset` to bound output.

## Follow jobs

Start tools return IDs immediately. Reuse the same `idempotency_key` when
retrying the same start request. Call `zz_job` with each returned `job_id` and
`wait_seconds: 45`; if it is still running, call again. Downloads return jobs
per node. Follow a launch's `deploy_job_id` too when present. Keep the
`deployment_id` to cancel a download through `zz_job_cancel`; ordinary work
cancels by `job_id`. After launch succeeds, use `zz_chat` for one short smoke
test. Pass `run_id` to `zz_runs`, `zz_logs`, `zz_run_restart` or `zz_run_stop`;
pass `job_id` to `zz_job`. The stop reply confirms completion. Stopped runs are
removed, so a later run lookup returns 404 by design. Launch job results omit
the serving node's local probe URL because it is not a coordinator-facing URL.

`zz_api_search` searches the embedded [OpenAPI contract](../internal/server/openapi.yaml) and
returns methods, paths, operation IDs, summaries and each parameter's name, location
and required flag.
`zz_api_get` reads a GET route; `zz_api` handles other methods and carries a
destructive hint. Both refuse internal, MCP, streaming, retired and unmounted
routes and require literal paths without URL encoding or traversal. Put query
parameters in `query`; use `content_type: "application/merge-patch+json"` for
parameter merge patches. REST Problems appear as tool errors with their
original status, code, detail and field errors.

## Conformance

Against a running dev coordinator:

```sh
scripts/mcp-conformance.sh http://127.0.0.1:9091/zzrouter/v1/mcp
```

The official suite cannot send an API key. Target a local key-injecting proxy
or a loopback test setup, as described in the script. The suite is pinned to
`@modelcontextprotocol/conformance@0.1.16` with the checked-in expected-failure
baseline; this version has no 2026-07-28 server scenarios. Add
`--spec-version 2026-07-28` when a release supports them. Fixture-only tools and
unsupported MCP features remain baseline failures.
