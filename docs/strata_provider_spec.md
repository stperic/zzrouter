# Strata provider plan

Status: implemented on branch `feat/strata-provider` in `/Users/eric/Devl/_private/zzrouter-oss-wt-strata`; live acceptance passed on the current tree, 2026-10-16 (see Live acceptance after the review).

## Upstream identity

zzRouter installs **[Niko1221/Strata](https://github.com/Niko1221/Strata)**, the Qwen3.8 Flash Next inference engine. Its upstream setup and [API documentation](https://github.com/Niko1221/Strata/blob/main/docs/DETAILS.md) describe the OpenAI API, Anthropic Messages API, Responses API, `/health` and `/v1/models`. [mehbul/strata](https://github.com/mehbul/strata) is a different AMD focused Rust engine and is outside this provider's scope. The provider's `version_source.repo` and installer source must both name `Niko1221/Strata`; tests should lock that pairing.

## Goal and shared contract

An operator can select a node, install Strata through zzRouter's existing provider install API, start and stop its service, discover its advertised model, and route chat and agent requests through the same catalog and exact model identity used by vLLM, MLX, llama.cpp and Ollama. The Strata installer owns Strata setup details. Shared routing, catalog, credentials, cluster selection and MCP tools stay provider neutral. No new provider type, Go plugin loader or Strata-specific public inference endpoint is needed.

Strata runs one configured model in a persistent daemon. In zzRouter it is an `external` OpenAI provider **with a managed installer and service**, like Ollama's service lifecycle. Here `external` describes HTTP transport, not who installed the daemon. `/runs` remains an on-demand model operation; Strata's install and start/stop controls use `/providers/{name}/install` and `/providers/{name}/service/*`. Agent responses must expose these available actions so a listed but stopped model is not presented as ready.

## Current architecture and ownership

| Concern | Owner | Decision |
| --- | --- | --- |
| Provider definition | `pkg/config/templates/files/providers/external/strata/` | Disabled by default like Ollama, `protocol: openai`, loopback root endpoint, `/health`, correct wire endpoints, upstream version source and install requirements. |
| Installation | `pkg/prov_apps/install/builtins/strata/` | Dedicated `ProviderInstaller` registered by `builtins.NewDispatcher`. It invokes the versioned upstream release's `setup.sh` with noninteractive, install-only flags and reports steps through the normal job/plan API. |
| Service | `pkg/prov_apps/manager_services.go` | Reuse `DaemonInstaller` and existing service control, process ownership, supervisor and health. `service.manager: none` selects zzRouter supervision; the installer supplies an exact Python server command and a managed executable identity. |
| Catalog | `pkg/modelregistry/source/openai/` and `pkg/modelregistry/registry_scan.go` | Generic bounded `/v1/models` scan for enabled external OpenAI providers. Publish only upstream IDs; inventory failure belongs to its provider. |
| Model identity | Existing catalog, resolver and public/MCP handlers, unchanged | Strata rows use the shared `name@node` identity. Same-name rows across providers keep main's existing behavior. |
| Model files | Strata's setup and dedicated data directory | Strata owns formats, downloads, fit and GPU planning. zzRouter does not infer local GGUF rows from those files or delete an advertised model through the catalog. |

### Managed install sequence

1. Preflight checks a supported node, writable provider root, Python 3.10+ with venv on Linux, required tools (including `c++`) and enough disk for the upstream model. The CUDA toolkit is upstream's policy, not zzRouter's: current Niko1221 releases have no Linux engine archive, so setup compiles one, finds the newest toolkit itself (`STRATA_NVCC` in the provider `environment` pins one), and without a usable toolkit stops at its engine step, before the model download, because it cannot sudo from the service. The node's Strata endpoint must be loopback and unoccupied. A custom remote Strata URL remains a connect-only configuration.
2. Resolve a stable upstream release from `version_source` or the requested version. Fetch `Niko1221/Strata` at its `v` tag into a versioned zzRouter-owned source directory. Reject unexpected repository, version syntax or existing operator-owned paths. The selected release is recorded only after setup succeeds.
3. Run that release's `setup.sh --yes --vision <yes|no> --no-start --no-browser --host 127.0.0.1 --port 8080 --data-dir <managed data dir>`. `--yes` takes upstream's recommended family and size for the node; `--vision` is `yes` only when the template declares the `vision` feature. Setup runs in the controlled install environment (`install.ControlledEnvironment`) plus the provider `environment`, never zzRouter's process environment, with its private `.venv/bin` first on `PATH` so upstream uses the CMake and Ninja versions it installs. The model download is large and resumable; the job remains cancellable and shows streamed setup progress. On macOS, use its documented setup flags and Apple Silicon path. Do not run upstream setup under sudo.
4. Verify exactly one generated `strata-*.json` model config, a private Python environment and its server entry point. Mark the install as managed and record the release after those checks. A failed setup leaves the previous version and data intact; rerunning can resume the download.
5. Start the server through the existing service API using the generated config and managed Python. Enable Strata on the coordinator, as for Ollama, so sync keeps it enabled on the hosting node. After `/health` is ready, run a targeted worker model scan, then call the model by its catalog name. `/health` and a real request, rather than catalog presence alone, establish readiness.

The source directory and downloaded model data are separate so upgrading code never overwrites model files. An upgrade prepares the new release before switching the recorded version. Rollback switches the version marker to the prior verified release after the service stops; start the service again afterward. Uninstall stops and detaches the managed service, but retains release files and large model data for recovery. Never remove an operator-owned Strata checkout.

## Provider and agent behavior

The provider template starts disabled and keyless on `127.0.0.1`. zzRouter's caller key stops at the router. A remote connection needs its own Strata bearer credential and node ownership; no secret belongs in the template. In a cluster, each node reads only its own loopback daemon: the Strata node advertises the model, the coordinator routes to that node, and nodes without Strata report its inventory as unavailable, as they do for Ollama.

The catalog publishes only IDs returned by Strata's `/v1/models`. Each row keeps the exact upstream `source_id`, assigned provider, observing node and an external OpenAI source marker. Generic deletion rejects these remote rows. Agents reach Strata through the unchanged `zz_models`, `zz_run_start` (which reports external models ready without a run) and `zz_chat` tools.

Declare `chat_completions` and `responses` from [upstream's API documentation](https://github.com/Niko1221/Strata/blob/main/docs/DETAILS.md#the-responses-api-and-codex-cli). Strata's Responses API is stateless: clients send the full conversation in each `input`; stored conversations and `previous_response_id` are unsupported. Use `messages_compat` until a live native Messages fixture proves tool calls, tool results, streaming and images inside tool results. Do not declare embeddings, completions, rerank, vision or tools merely because a route exists. Preserve normal virtual key, quota, logging, metering and cluster paths.

## Acceptance

1. Tests verify the disabled provider template, strict config loading, upstream repository lock, install plan and cancellation, managed ownership, rollback/uninstall boundaries, service command and common provider API shape.
2. Synthetic transport tests cover catalog errors and collisions, exact route round trips, cloud/source separation, remote deletion refusal and agent selection. A real fixture from the installed release must replace assumptions about `/health`, `/v1/models`, usage, tools, streaming and Responses.
3. Install on a capable node such as `localai` via zzRouter's provider install flow. Verify the installed version, generated config, service state, catalog row and a real chat/tool and streamed Responses request through the coordinator. Verify another provider still routes normally. Record the tested release and remaining limitations here.

### Live handoff, 2026-10-16 (earlier revision; route-ID and enabled_scope details below were removed in the Review)

- Branch `feat/strata-provider` is an uncommitted worktree at `/Users/eric/Devl/_private/zzrouter-oss-wt-strata`. The main checkout is `/Users/eric/Devl/_private/zzrouter-oss`. The temporary authenticated diagnostic helper used for live checks has been removed. Do not print client credentials.
- `localai` is the Linux worker at `10.1.5.10`, with an RTX PRO 6000 Blackwell GPU. zzRouter install job `inst_ec9098ec4756b68d` completed successfully. Managed provider status reports Strata `0.1.44.1`, installed, enabled and process running. Service status reports healthy. Engine and model data live under `/opt/zzrouter/providers/strata/`.
- This upstream release has no Linux engine archive. NVIDIA's versioned CUDA 13.0 toolkit was installed for the Blackwell build while the default `/usr/local/cuda` remains CUDA 12.6 for existing providers. Upstream compilation succeeded with its private CMake 4.4.3. The selected IQ3_S model was downloaded, packed, and loaded. Initial model load took about one to two minutes.
- Strata's own `/v1/models` reports loaded source ID `qwen3.8-flash-next-iq3_s`. A targeted zzRouter `POST /zzrouter/v1/models/scan?node=localai` was needed after readiness because the first scan ran before the service bound its port. The coordinator now lists it as a Strata `external_openai` model on `localai` with an exact `zzr1:` route ID; catalog count rose from 82 to 83, preserving the existing MLX, vLLM and llama.cpp rows.
- Live requests through the coordinator and exact route ID returned HTTP 200: chat completion produced `Hello` with usage and `zzrouter.provider=strata`; non-streaming Responses produced `Hello`; a forced `get_weather` tool call returned `tool_calls` with `{"city":"Paris"}`; a tool-result follow-up returned `Sunny`. The complete Responses stream included `response.created`, output text deltas and `response.completed`. Its nested `response.model` now equals the caller's exact `zzr1:` route ID, verified after rebuilding both nodes. An existing Ollama model returned HTTP 200 and `Hello.` through the same coordinator after rollout.
- The first worker restart exposed a config ownership bug: coordinator sync pushed its disabled Strata template over the worker's successful local enable. A general `enabled_scope: node` external-provider setting now preserves each worker's enabled value while syncing other fields. After updating both nodes, a public `PATCH /zzrouter/v1/providers/strata?node=localai` enabled Strata. A second worker restart and coordinator sync preserved `enabled: true`, and the managed service automatically returned healthy. The focused config persistence and provider-sync tests passed.

### Review, 2026-10-16

Direction from the user: adding Strata must not change the interface for the other providers. The review removed what did:

- The `zzr1:` route-ID layer (`pkg/model/routeid`, resolver, `/v1/models`, `/v1/model/info`, `/v1/models/{id}`, MCP `zz_models.route_id` and the `zz_chat` provider selector). It changed published IDs and resolver results for every provider, deleted an existing resolver test case, made `GET /v1/models/{bare name}` return 404, and its `zz_chat` provider selector never reached routing because `dropCoordinatorHeaders` strips `X-zzrouter-Provider` from every request. Strata's real ID needs none of it.
- `enabled_scope: node` and its YAML rewrite in provider sync, a second ownership rule beside the existing one. Strata now follows Ollama. That sync overwrites a worker-local enable is a pre-existing bug for every provider and belongs in its own fix.
- MCP instruction and tool-probe rewording that dropped tool names, and the nested Responses-stream `response.model` rewrite. That rewrite fixes a pre-existing defect shared by all providers, so it also belongs in its own change.

Kept: the Strata template and installer, `service.manager: none`, which the schema already accepted and Go code rejected, the generic external OpenAI catalog scan (the user chose generic over Strata-only), now bounded by the same scan timeout as Ollama, and the refusal to delete provider-owned rows.

Installer cleanup, same day:

- The Go CUDA search (`nvidia-smi`, `/usr/local/cuda-*` globbing, `nvcc --version` parsing, a hardcoded "12.x needs 13" rule, `STRATA_NVCC` injection and a plan-time refusal) was removed. It duplicated upstream's `find_nvcc`/`need_cuda`. zzRouter's shared toolkit probe was considered and rejected: it finds `nvcc` only on the service `PATH`, so it would refuse installs upstream completes (localai's CUDA 13 sits beside a 12.6 default) unless the operator changed `PATH` for every provider.
- `--family qwen` was dropped; `--vision` now follows the template's declared `vision` feature.
- Setup previously inherited zzRouter's whole process environment, credentials included, through `Step.Env`. It now gets `install.ControlledEnvironment` plus the validated provider `environment`, like pythonvenv steps; a test pins that the admin key never reaches it. This path is not live-tested yet.
- The Python 3.10 floor moved from two Go literals to the template's `requirements.python`, which preflight and the plan both read. Setup refuses an empty service `PATH` rather than search the checkout.
- Open: `Rollback` skips its running-daemon guard when the current release's config is unreadable, because `DaemonCommand` then returns nothing. That is the broken-release case rollback is for; stop the service first.
- Upgrade and rollback keep their own release-directory layout. The shared `BuildUpgradePlan` backup copies the whole provider directory, model data included (100+ GB), and no shared convention keeps provider data outside it.

### Live acceptance after the review, 2026-10-16

Both nodes run build `0.2.3+10162239`, built from this branch: the coordinator from the worktree binary, `localai` from `/opt/zzrouter/versions/0.2.3+10162239-strata` (previous: `0.2.3+10162100-strata-wt`). Before the switch, `enabled_scope: node` was removed from both nodes' Strata `config.yaml` (the strict loader refuses it) and the coordinator's copy set to `enabled: true`; template reconcile added `requirements.python` on both. After the worker restart, zzRouter restarted the Strata daemon and Ollama, and the catalog listed the model without a manual scan.

A forced reinstall on `localai` (install job, release 0.1.44.1) ran the new plan: Python 3.11 from `requirements.python`, `setup.sh --yes --vision no` without `--family`, under the controlled environment. Setup found everything in place and exited 0 in about 7 seconds, so it confirmed the environment is accepted but did not exercise package installs, compilation or a download under it. Service status afterwards: supervisor zzRouter, running, healthy.

Through the coordinator, with a temporary admin virtual key deleted afterwards, all returned HTTP 200: the `/v1/models` id `qwen3.8-flash-next-iq3_s@localai`; `GET /v1/models/{id}` by that id and by the bare name; chat by both names (`X-zzrouter-Provider: strata`, usage reported); a forced `get_weather` tool call with `{"city":"Paris"}` and a tool-result follow-up answering "sunny and 22°C"; non-streaming Responses; a Responses stream carrying `response.created`, `response.output_text.delta` and `response.completed`; translated Messages; and an Ollama model on the same node.

Observed: Strata reports `status` in `/v1/models` as an object (`{"value": "loaded"}`), not a string, so the catalog drops that detail. Cosmetic.

### Validation and remaining limits

`GOTOOLCHAIN=go1.26.9 go test ./...`, `go vet ./...`, and golangci-lint (`0 issues`) passed on the final code. `git diff --check` passed. The installed golangci-lint was built with Go 1.26, so it must run with that toolchain rather than the default Go 1.27. The temporary live diagnostic helper was removed after those checks. The macOS coordinator runs from the lab working directory using the Strata worktree binary; the Linux worker runs the staged worktree build under systemd, with older versioned releases retained for rollback.

Native Messages remains a compatibility route until its image-bearing tool results are exercised. Upstream Responses is stateless and does not support `previous_response_id`. Apple Silicon installation is implemented but has not been live tested on a macOS worker. The worktree remains uncommitted for review; no commit or PR was requested.
