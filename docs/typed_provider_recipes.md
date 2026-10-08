# Managed Python recipe repair

`install.runtimes` is release-owned base data in a provider's `config.yaml`.
`defaults.install` and `nodes.<name>.install` are JSON merge-patch overlays on
that data. Only those two tiers accept overrides. Arrays replace, explicit
`false` is preserved, and `null` deletes an override to restore inheritance.
Recipes contain package/version constraints, indexes, companion distributions
and build expectations, startup imports, fixed verification checks, wheel-only
mode, a bounded timeout and a release-owned toolkit layout. They contain no
commands, hooks, URLs to scripts, host package actions or executable paths.

The shipped vLLM, MLX and MLX vision recipes all use the same Python installer.
Base installs use the provider pin when no version is supplied. Upgrades and
unversioned feature installs select the latest package version satisfying both
recipe and operator constraints. Feature selection is `runtime:"mlx-vlm"` on
MLX's existing install, upgrade, plan and verify routes; it uses the feature's
own package and never MLX's parent pin.

## Authority and threat boundary

An unchanged embedded recipe has release authority and needs no policy file.
This works under the macOS operator UID, Linux service UID and Windows LocalSystem.
A writable base file does not grant release authority: the executing node compares
it with its embedded recipe. Any effective override, including restating a shipped
value, requires independent operator authority. An override aimed at another node
may be stored without granting permission to execute it locally.

The startup-only `providers.install_policy_file` in `node.yaml` points to an
absolute, protected policy file. The API, peer sync and reconciliation cannot
change that path or file. Linux/Mac require root ownership, protected non-symlink
ancestors and denial of service write access including ACLs. A root service cannot
establish this boundary. Windows requires protected SYSTEM/Administrators ownership
and DACLs, no reparse points, and a non-elevated service identity. LocalSystem
cannot authorize overrides. Provisioning or changing service identity is human work.

Nothing provisions a policy implicitly; the moment a human runs the installer as
root is the bootstrap. `install.sh` run as root (Linux) writes one at
`/etc/zzrouter-install-policy.yaml` and sets `providers.install_policy_file`, never
replacing an existing policy or setting; `--no-install-policy` skips it. Run
unprivileged, including every macOS install, it prints the commands instead
(`/private/etc/...` on macOS, where `/etc` is a symlink the node refuses). The file
comes from `zzrouter-node install-policy template`,
which grants exactly the packages, imports, entrypoints and indexes of this
release's shipped recipes for the platform, so a new grant (another index, an
acceptance probe module) is always a human edit. A node with no policy refuses
overrides with an error naming these steps. `install.ps1` writes none: LocalSystem
cannot authorize overrides.

A PATCH validates syntax and obtains authority acknowledgement from every known
node whose effective recipe changes before saving. An offline affected worker or
missing/unsafe policy refuses the PATCH with `errors[].code=preflight_failed`.
Unrelated parameter writes and unaffected nodes do not require install policy.
Running jobs retain their accepted recipe and environment, while independent policy
is checked again before pip or imports execute. Policy changes revoke the pending
job and require a new preview. Existing releases may be installed without a policy.

Generate a starting policy with `zzrouter-node install-policy template`.
Review the complete package/import/entrypoint/index grants before provisioning
it. A grant approves root package families and resolver-selected transitive
dependencies, rather than every transitive distribution individually.

Index inputs require clean HTTPS URLs without credentials, fragments or queries.
Private destinations need explicit operator approval. Index approval does not
constrain pip's redirected artifact/CDN egress. Extra indexes have no priority and
can cause dependency confusion. Use an operator-controlled mirror/proxy when that
boundary matters. Plans report this limitation. A disposable resolver venv generates
an exact inventory of runtime packages, versions, artifact URLs and SHA256 hashes;
execution installs that inventory with `--require-hashes --no-deps`. Source builds
require both recipe opt-in and human permission. PEP 517 build dependencies are
outside the hash-pinned runtime inventory and can execute during resolution. Prefer
wheel-only recipes when that is unacceptable. Pip configuration files and inherited
PIP/PYTHON options do not determine package selection; trusted node proxy/CA settings
remain available. Managed venvs retain configured read-only hardening.

## API transaction and identity

The normal repair sequence uses the existing API:

1. `PATCH /providers/:name/parameters` with a defaults/node install overlay.
2. `POST /providers/:name/install/plan?node=...` with runtime, version, action,
   force and optional disposable. Read the immutable `plan_id`, resolved recipe,
   provenance, policy fingerprint, exact dependency inventory and steps.
3. `POST /providers/:name/install?node=...` with `force:true` and
   `expected_plan_id`, or the existing upgrade route. Follow its job to completion.
4. `POST /providers/:name/install/verify?node=...` with runtime. Basic verification
   checks dependencies/imports/device availability; it does not claim inference.
5. Launch through `/runs`, then chat and stream `/v1/messages` to prove the actual
   model-triggered path. No implicit smoke endpoint is introduced.

A plan ID binds recipe, policy, node/runtime, selected version, interpreter,
controlled install environment, provider runtime environment, checks, dependency
inventory, action, force and isolation. Changing those inputs requires a fresh
preview. File observations do not change identity or step numbering. Automatic
execution may omit `expected_plan_id` and receives its actual accepted identity.
Guided `execute-step` requires it and re-resolves under the runtime lock. The TUI
passes the accepted version/runtime/identity through its asynchronous commands.

A candidate is installed beside the active venv, verified, relocated and swapped.
The active candidate is verified again before its version/manifest are recorded.
Any execution, completion verification or finalization failure restores the prior
venv and metadata, including failures after swap. Jobs report rollback completion
and whether an existing runtime was preserved. A failed fresh install removes new
installed markers. Active provider runs refuse install/uninstall/cleanup, including
feature runtimes, under the same gate used by launch. The previous runtime stays
available until the next successful transaction. Installed provenance and desired
recipe drift appear in the environment view.

## Managed CUDA and disposable proof

The shipped CUDA recipe uses the pip-packaged CUDA 13 compiler, runtime, CRT,
NVVM and CCCL companions. A fixed release-owned layout stays inside the venv.
`CUDA_HOME`, `CUDA_PATH`, `CUDA_LIB_PATH`, `CUDACXX` and `FLASHINFER_NVCC` select that layout after
all caller/config environment tiers. Trusted execution prepends the managed compiler
and venv directories to a captured PATH, retaining absolute host paths for C++ tools,
and replaces inherited `LD_LIBRARY_PATH` with the managed library directory.
`CUDA_LIB_PATH` points at that same directory so library loaders cannot fall back
to a host toolkit. Caller
PATH and library-path overrides remain refused. Verification checks PATH's effective
nvcc, both compiler selectors, and real paths of the compiler and loaded CUDA runtime
library inside the managed venv. Loaded CUDA, Torch and compiler versions must agree.
This path does not use the host
CUDA toolkit. A protected human-provisioned NVIDIA driver and compatible host C++
compiler remain prerequisites. Diagnostics explain these; no install step changes
them. Missing compiler/layout findings describe repair or human remediation.

`disposable:true` on plan/install creates a managed isolated runtime without
replacing active version/manifest. Launch `/runs` with its `disposable_plan_id` and
runtime. Only a verified managed plan directory is selectable; callers cannot
supply executable paths. Launch checks current recipe/policy/environment identity
and refuses to reuse a run from a different runtime or isolation identity. Stop the
run, then `DELETE /providers/:name/install/disposable/:plan_id?node=...&runtime=...`.
Cleanup changes no active installation or config.

Verify inference in a disposable runtime before replacing a working installation.
Basic import/device checks do not establish model-triggered execution. Stop the
proof run and delete its disposable plan when finished. See the
[API reference](api.md) and [security model](security.md).
