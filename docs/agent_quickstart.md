# Agent quickstart

An operator initializes the node, approves provider installation authority and
gives the management agent an admin key through a protected credential channel.
Inference clients receive separate virtual keys. Host changes remain operator work.

```sh
Z=http://127.0.0.1:9090
curl -fsS -H "X-API-Key: $ZZROUTER_ADMIN_API_KEY" "$Z/zzrouter/v1"
curl -fsS -H "X-API-Key: $ZZROUTER_ADMIN_API_KEY" "$Z/v1/models"
```

Choose a provider compatible with the node's hardware. Inspect its environment,
install prerequisites and install plan through the advertised provider routes.
Use the managed install API; follow the returned job and verify the runtime.
See [managed recipes](typed_provider_recipes.md) for package authority.

For an installed llama.cpp provider, launch a model and request automatic
weight deployment when needed:

```sh
curl -fsS -H "X-API-Key: $ZZROUTER_ADMIN_API_KEY" \
  -H 'Content-Type: application/json' -X POST "$Z/zzrouter/v1/runs" \
  -d '{"provider":"llamacpp","launch_mode":"native","model":"Qwen/Qwen2.5-0.5B-Instruct-GGUF","auto_deploy":true}'
```

Follow `deploy_job_id` when present and the launch `job_id`. Poll the jobs API
or its SSE stream until successful, then confirm the run is ready. Use a node
name such as `worker-1` when targeting a paired worker; an address is not a node
selector. Select a model endpoint advertised by `/v1/models`.

Create a client key with `zzrouter keys create my-agent --rpm 60` and place the
returned credential in `AGENT_API_KEY` through your protected credential channel:

```sh
curl -fsS -H "Authorization: Bearer $AGENT_API_KEY" \
  -H 'Content-Type: application/json' "$Z/v1/chat/completions" \
  -d '{"model":"Qwen/Qwen2.5-0.5B-Instruct-GGUF","messages":[{"role":"user","content":"Hello"}]}'
```

If a model's own chat template refuses a request (`chat_template_rejected`),
`chat_templates` in `GET /zzrouter/v1` gives the API steps to install and
select a template that accepts it. zzRouter ships no chat templates.

Observe runs at `/zzrouter/v1/runs`, stop one with `DELETE /runs/:id` under that
management prefix, and branch on error codes rather than message text.
The [OpenAPI specification](../internal/server/openapi.yaml) is the field reference.
