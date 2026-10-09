# Changelog

## Unreleased

- Add optional `readiness_probe.stall_timeout` to detect silent engine starts.
  vLLM now allows 900s overall and fails after 240s without output; existing
  installs receive these values through managed provider reconciliation.
- Cluster protocol and minimum advance from 2 to 3: older workers reject
  `readiness_probe.stall_timeout` during strict provider configuration loading.
  Upgrade all peers together following
  [the protocol maintenance procedure](docs/protocol_versioning.md).

- Add opt-in node GPU memory reclamation through
  `gpu.evict_idle_on_memory_pressure` in `node.yaml` (default off). Launches that
  cannot fit stop eligible idle runs in activity order, wait briefly for memory
  release, and retry admission once. Keep-alive expiry shares the eligibility
  check and cannot claim a run with an active lease.

- Derive Messages and Responses translations for all providers declaring
  Chat Completions without the corresponding native API. Shipped provider
  configs now declare native endpoints only, and reconciliation carries that to
  existing installs. An explicit `*_compat` entry still loads and means the same
  thing. Catalogs and running snapshots report the same effective endpoints.
- Provider-declared engine failure classes now cover startup, process exit and
  inference. Memory exhaustion returns 409 `out_of_range`; input, template and
  tool-calling refusals return their specific 400 codes. Generic server errors
  keep their status. Existing installs receive the declarations through reconcile.
- Cluster protocol and minimum advance from 1 to 2: older provider schemas
  reject `runtime.failure_patterns`. Upgrade all peers together following
  [the protocol maintenance procedure](docs/protocol_versioning.md).

- Removed `model_defaults`, the release-managed per-model-family tier. No
  release shipped data in it after 0.1.0. Per-family settings belong in the
  operator's `models` tier, which accepts glob patterns. `/resolved` no longer
  reports a `model-default` tier. An install that still carries the block has
  it removed on start.
- Built with Go 1.26.8 (compiler, runtime and `net/http` bug fixes; no security
  fixes over 1.26.6).

## 0.1.0 (2026-10-08)

First public release.

- Distributed model management through an authenticated API and terminal client.
- OpenAI, Anthropic Messages and Ollama compatible inference.
- Managed llama.cpp, vLLM and MLX runtimes, external Ollama and cloud providers.
- Virtual keys, teams, model access, quotas and usage accounting.
- Model features, provider parameter tiers and variants.
- No chat templates ship. When a model's own template refuses a request,
  llama.cpp errors come back as 400 `chat_template_rejected`, and
  `chat_templates` in `GET /zzrouter/v1` gives the API steps to install one.
- Cluster mTLS and signed updates from `stperic/zzrouter`.
- Every cluster node must run the same cluster protocol (`zzrouter-node version --json`).
- Built with Go 1.26.6; `govulncheck` reports no reachable vulnerabilities.
- Reproducible release builds: every platform of a tag carries the same version.

No private release history is included. Until 1.0, review compatibility notes
before upgrading.
