# Changelog

## Unreleased

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
