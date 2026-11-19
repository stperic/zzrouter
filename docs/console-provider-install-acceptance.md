# Console provider install on localai

Live Safari exercise on 2026-11-18 as a Console administrator. The coordinator served `0.2.3+11181314` and the worker `localai` reported `0.2.3+11171843`. ComfyUI started disabled with no running instance and no files in its managed model inventory. The in-app browser and Chrome control were unavailable in the test harness. The install proceeded in Safari after administrator sign-in.

## Observed path

1. Opened **Providers → Local providers → comfyui**. The provider showed **Setup required**. Selected `localai` in **Compute node**, opened **Manage runtime**, then chose **Install or repair**.
2. Left the provider defaults in place. The version field was blank, the optional Manager component was unchecked, and force replacement was off.
3. Selected **Review install plan**. The UI showed **Preparing the exact install plan…** for two minutes, then **Could not reach the coordinator**. The coordinator recorded HTTP 502 at 120 seconds. Its health check still passed.
4. Selected **Check prerequisites**. All five checks passed on the worker, including available disk space, Python 3.11.2, and venv support. The preflight response took 176 ms. The prior connection error stayed visible next to the passing result.
5. Retried **Review install plan**. The coordinator returned the plan in 49.5 seconds. The Console showed ComfyUI `0.44.0`, `localai`, no optional components, and five steps covering preparation, venv creation, dependency installation, verification, and activation.
6. Chose **Run all steps** and confirmed the Safari dialog. The Console showed **Install provider: waiting for the coordinator…** for 43 seconds before HTTP 202. It opened the accepted `localai` job and showed **60%**, **Install the accepted dependency inventory**, and **Step 3 of 5** while the step was still running.
7. The job reached **done** and **100%** about 2 minutes 12 seconds after acceptance. The UI still showed its acceptance message and the job ID twice. Selected **Inspect provider** to load fresh worker observations. The Console showed **Installed Yes**, **Managed Yes**, **Enabled Yes**, and **Version 0.44.0**. The provider's primary action was **Choose a model**. The worker had no running ComfyUI instance when checked after installation.

## Improvements for the default install

- **Offer Install directly on the node row.** When a provider shows **Setup required**, put its primary install action next to the affected node. Carry the node and provider into the wizard. The current path requires finding the provider, selecting a node, opening runtime management, and then choosing an action labeled **Install or repair**.
- **Run prerequisites automatically.** A default install should check the worker and show a specific remediation only when a check fails. Keep version, optional components, replacement, and step-level controls under advanced settings. Clear obsolete feedback when a later check succeeds and keep plan errors attached to the step that produced them.
- **Track dependency resolution as a job.** The browser gives plan preparation 120 seconds while the worker resolver has a ten-minute bound. The first attempt expired with a misleading connectivity message. Return a durable job ID promptly, distinguish worker resolution from a disconnected browser, and let the operator reopen progress after navigation. Record the observed stage and elapsed time without guessing a percentage. [Console plan request](../internal/server/web/console/provider-install.mjs) · [Worker resolver](../pkg/prov_apps/install/builtins/pythonvenv/preflight.go)
- **Keep the default confirmation compact.** Show provider, node, version, and optional selections with a single in-page **Install** action. Keep the full reviewed plan, scripts, and per-step verification available for operators who need them. The current flow presents a full plan followed by a generic Safari **OK** dialog.
- **Show the stage during submission and execution.** The UI waited 43 seconds with no phase detail while the server prepared the plan again before accepting execution. Surface that work and attach the provider, node, and job to the activity view once. Reconcile a lost response against the same operation before offering another attempt. During dependency installation, show **Step 3 of 5** and its elapsed time. An overall percentage is useful when the underlying work reports measurable progress.
- **End on a verified provider state.** Replace the acceptance message with the terminal result, check the installed version and enabled state on the worker, and show those facts inline. Keep the job ID in one place. Distinguish an enabled provider from a running instance. The runtime panel showed **State live** while no ComfyUI instance was running.
- **Choose the next action from the provider's run scope.** The Console sent **Choose a model** to Discover for every installed, enabled provider. ComfyUI's primary action should start the node-scoped runtime on `localai`. Offer its native UI once the run is ready and browser access is available. The Discover badges identify registry entries, and Console Chat currently sends Chat Completions requests. A ComfyUI image-generation action in Chat would need its own workflow integration.

## Acceptance checks for the follow-up

- From an uninstalled provider's node row, start the default managed install with the target and release visible.
- Exercise a plan taking more than two minutes, a prerequisite failure, a lost browser response during submission, and reopening the same job after navigation. Verify actionable messages and a single accepted install.
- Verify success from the worker's installed version and enabled state. Exercise a model-scoped provider's discovery handoff and ComfyUI's node-scoped launch handoff. Confirm approved advanced selections and exact-plan verification remain available.

## Protocol-25 Safari retest

The coordinator test build `0.2.3+11182100` used the lab's protocol-25 baseline. ComfyUI `0.44.0` was already installed on `localai` from the exercise above. Both workers rejoined after the coordinator restart. The fresh-install path used a disposable browser fixture and focused worker tests.

1. Opened **Providers → Local providers → comfyui → localai → Choose workflow**. The catalog showed two included workflows, Stable Diffusion 1.5 and FLUX.1 Schnell. Chose Stable Diffusion 1.5 for its 4.27 GB checkpoint and 512 × 512 output.
2. The workflow marked `v1-5-pruned-emaonly.safetensors` missing. Selected **Download model files**. The download ran on `localai`, and the linked job reached 100% with **Transfer completed**. Reopened the workflow and verified **All required model files are present on this node**.
3. Selected **Open in Chat**. Chat selected **Stable Diffusion 1.5** and identified **comfyui · localai**. Submitted a product-photo prompt and watched Chat start the managed ComfyUI runtime on the worker. Safari rendered the result, and **Download PNG** saved a valid 512 × 512 RGB PNG.
4. Used **Change** in the same Chat tab to select **SmolLM2 on localai**. Sent a text prompt and received a completed text response.

### Findings from the retest

- **Show transfer progress where the download starts.** The workflow showed **Downloading**. The exact percent and transfer status were available only after opening the job. The follow-up UI now uses the existing job progress component on each downloading file, with a browser test covering the inline percentage.
- **Keep the workflow review easy to resume.** Opening the job moved to Workloads. Safari Back returned to provider details, so reopening the workflow took two more selections. The full download record remains in Workloads. A direct link back to the exact workflow review would preserve the operator's context.
- **Make Open in Chat the next action as soon as files are ready.** The verified screen had a disabled **Download model files** above the ready message and **Open in Chat** below the file details. The follow-up UI removes the superseded download action and places **Open in Chat** next to the readiness message.
- **Expect a cold start on the first prompt.** Chat initially displayed **Checking the ComfyUI runtime…** while Workloads confirmed the run was starting on `localai`. More specific startup and queue status would help users tell which work is underway.
- **Explain which workflows run in Chat.** The included presets support Chat. The official catalog can resolve model files and hand off to the native ComfyUI runtime. An in-Console distinction helps users choose the correct workflow before downloading weights.
- **Treat slow advanced review separately.** The default install prepares inside a tracked worker job. The advanced exact-plan preview still uses the request timeout observed in the original exercise.

### Verification and deployment state

The startup, checkpoint download, image generation and PNG download were live. The Playwright fixture also verifies a fresh default provider install, automatic worker verification, model deployment, rendering, download and switching to text Chat. The Mac coordinator is running the acceptance build. Its previous binary is backed up. The worker binary and its existing ComfyUI installation stayed in place. The inline transfer progress and ready-state layout updates passed the browser fixture after this deployment.

## Follow-up changes in the feature worktree

- A Discover card tagged for ComfyUI opens its provider workflow setup. The generic inference download form excludes node-scoped runtimes and could show MLX before checking file compatibility.
- A workflow review keeps its provider, worker and template in the URL. Opening a download job stays in the same panel, and a reload recovers the worker's existing transfer. A single model inventory read supplies readiness for all included presets on a worker.
- Advanced plan preparation now returns a job before dependency resolution. Its retained result can be reopened from Workloads. Execution continues to verify the exact plan ID.
- The Overview provider panel includes the node address and distinguishes an on-demand runtime from a supervised service. At the live check, the coordinator and both workers were healthy and had no managed runs. ComfyUI and Ollama were enabled on `localai`, Ollama on `PC-ERIC`, and MLX on the Mac.
- Chat generation uses ComfyUI's temporary preview lifecycle. Save the PNG from Chat to retain a copy.

## Reviewed execution and Discover handoff

- Safari waited for the reviewed execution request while the worker resolved
  dependencies. Install, upgrade and guided steps now use the same worker job
  to resolve and verify the plan under the existing runtime lock. A stale plan
  fails before any install step runs. **Review a new plan** reopens its options.
- The install API returns the owning node and job links on acceptance. Read the
  verified `meta.plan_id` or terminal `meta.error_code` on that job. The OpenAPI
  uses the same accepted-job envelope for preparation and execution.
- From a ComfyUI-tagged Discover result, select the compute node. The existing
  workflow picker opens the official catalog with the model name prefilled.
  The Chrome fixture exercised this with two nodes and a browser reload.
- The coordinator-to-worker fixture checked that request cancellation after
  acceptance leaves the job running and that stale plans never execute.

## Live Safari retest on the updated worker

The Mac and `localai` used test build `0.2.3+11182219` from the protocol-25
acceptance baseline. Both were healthy after the test, and Ollama was running on
`localai`. The previous binaries are available for rollback. The first worker
deployment attempt rolled back when the health-check script assumed `jq` was
installed. The retry used the worker's existing Python JSON parser.

1. In **Providers → comfyui → localai → Manage runtime → Install or repair**,
   reviewed a same-version `0.44.0` repair with the existing component
   selection. Safari opened the preparation job while dependency resolution
   ran. The exact plan was ready to review when the job finished.
2. Confirmed **Run all steps**. Safari showed the execution job and its
   dependency-resolution stage within one second. The job completed all five
   steps, and the Console verified installed and enabled status and version
   `0.44.0` on `localai`.
3. Searched for `Comfy-Org/Wan-2.2` in Discover. **Find a ComfyUI workflow**
   retained `Wan-2.2` through a page reload. Choosing `localai` opened the
   official catalog with matching Wan 2.2 workflows.
4. Opened the included Stable Diffusion 1.5 preset. The Console verified the
   existing checkpoint. **Open in Chat** generated a live image on the RTX
   PRO 6000 in 13.91 seconds as measured by Safari. **Download PNG** saved a
   valid 512 × 512 RGB image. Switching to SmolLM2 on `localai` in the same tab
   completed a text request.

### UI follow-up

- Show **Live log stream** for a running `inference_log` row in Workloads.
  It stays open to report completed requests, so a percentage or waiting
  animation is misleading. The displayed model can be from the most recent
  log event.
- Show **Installing** or **Repairing** on the node card while its runtime
  job is active. During the repair, that card said **Not installed** alongside
  **View install jobs**.
- Offer **Choose workflow** on successful reviewed ComfyUI repairs, as the
  default install flow does. The repair result currently returns to the review
  or provider details before the workflow handoff.
- Keep the workflow query scoped to its catalog source. Switching from the
  prefilled official Wan search to Included workflows carried the search
  through and required manually clearing it to see the image presets.

## Live retest after the job and Console cleanup

The protocol-25 integration worktree supplied coordinator build
`0.2.3+11182254` and worker build `0.2.3+11182246`. The coordinator build
includes the final Console assets and route catalog. Both nodes were healthy
after the test, and Ollama was running on `localai`. The previous binaries
remain available for rollback.

1. Prepared a ComfyUI `0.44.0` repair in Safari and reopened the exact plan
   from Workloads after a coordinator restart. **Verify step** displayed its
   worker job in under one second. Step 5 finished with **Passed** and
   **runtime prerequisites checked**.
2. Started the reviewed repair and submitted the same plan from a second
   tab while it ran. The duplicate was refused and **View install job**
   opened the original worker job. The coordinator fixture also checked
   that there was exactly one job in its registry.
3. The provider card showed **Installing** during replacement. The final
   Console build also hides the derived **Disabled** badge during that state.
   Regression tests cover the active badge and exact job link. Safari showed
   **Live log stream** for all three nodes' inference log rows, including
   streams that had yet to report a request.
4. The repair finished at 100% and verified that ComfyUI `0.44.0` was
   installed and enabled. **Choose workflow** opened directly from the result.
   Switching to Included workflows showed both presets, and switching back
   to the official catalog restored the Wan search.
5. Stable Diffusion 1.5 reused its existing checkpoint. Chat generated an
   image in 14.26 seconds as measured by Safari, and **Download PNG** saved
   a valid 512 × 512 image.
