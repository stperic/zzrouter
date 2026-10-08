# Live Chat Completions captures

Captured directly from installed engines on 2026-10-06 using a new test run for each engine. Requests used `stream: true` and the user prompt `Reply with only OK.` No router stream copier was involved. Only the test runs created for these captures were stopped afterward. The llama.cpp and MLX captures needed no deployment. The vLLM capture followed the approved Part A router deployment and managed API reinstall/upgrade documented in [the provider-install handoff](../../../../docs/handoff_provider_install_api.md).

| Fixture | Engine | Model | Limit | Captured termination |
| --- | --- | --- | --- | --- |
| `chat_llamacpp_b10549.sse` | llama.cpp b10549 on worker-1 | qwen2.5-0.5b-instruct-q4_k_m | 32 tokens | `finish_reason: stop`, then `[DONE]` |
| `chat_mlx_0_31_3.sse` | MLX 0.31.3 on macbook-pro | Qwen3.5-9B-MLX-4bit | 64 tokens | `finish_reason: length`, then `[DONE]` |
| `chat_vllm_0_29_0.sse` | vLLM 0.29.0 on worker-1 | Qwen/Qwen2.5-0.5B | 32 tokens | `finish_reason: length`, then `[DONE]` |

The MLX fixture reached its token limit while generating reasoning. It verifies a completed token-limited reply, not an unlimited reply. Its absolute model directory was replaced with `/models/`; all content, reasoning, usage and finish fields are preserved.

The vLLM fixture is the unmodified HTTP response from the engine's loopback
Chat Completions endpoint. It reached its token limit. It used
`attention-backend=FLASH_ATTN` and `VLLM_USE_FLASHINFER_SAMPLER=0`, plus eager
execution, 20% GPU memory, a 1024-token context and the
`qwen3.8-system-anywhere.jinja` template, which releases no longer ship. This
is the base Qwen model, not an instruction-following quality test. The capture preserves every frame,
including the usage-only frame and the actual finish reason. vLLM 0.27.1
could not start after its CUDA repair because FlashInfer failed to import on
Python 3.11; 0.29.0 fixed that import but its default FlashInfer sampler still
failed on this host. The explicit supported backends avoided that failure.
These results prove this scoped launch, not default-backend health.

These captures support retaining the finish-reason requirement for the three tested engine versions. `[DONE]` alone remains insufficient because the router copier can synthesize it on upstream EOF or read failure (`pkg/dispatch/wire/copy.go`).
