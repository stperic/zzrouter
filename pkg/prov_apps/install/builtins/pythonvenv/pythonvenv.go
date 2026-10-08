// Package pythonvenv installs Python-based providers (vLLM, MLX, and any
// future pip-based runtime) into a managed venv under the zzRouter
// provider root. It emits install/upgrade/uninstall plans via the shared
// install.PlanProvider contract; install.BaseInstaller handles the
// Install/Upgrade/Uninstall/Rollback runtime.
package pythonvenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/interpreter"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Config defines a Python-based provider that installs via pip in a managed venv.
type Config struct {
	Recipe     install.RecipeResolver
	Name       string            // Provider name used for directories and registration (e.g., "vllm", "mlx")
	PipPackage string            // pip package name (e.g., "vllm", "mlx-lm")
	ImportName string            // Python import name for version check (e.g., "vllm", "mlx_lm")
	Platforms  []fsroot.Platform // Supported platforms

	OnlyBinary     *bool         // Pass --only-binary=:all: to pip (default true; false for CUDA providers)
	PipConstraints []string      // Extra pip specs to constrain transitive deps (e.g., "transformers<5")
	PipTimeout     time.Duration // pip install step timeout; 0 falls back to defaultPipTimeout
	Checks         install.RuntimeChecksResolver
	SchemaProvider string
	Environment    install.ServiceEnvFunc
	VerifyStep     *install.Step // Provider-specific post-install check (nil = default import check)
}

// Installer manages installation of Python-based providers via pip in a managed venv.
// Used by vLLM, MLX, and any future Python providers.
//
// chosen caches the interpreter resolved by the first Preflight or InstallPlan
// call so the second call doesn't re-probe the filesystem. Preflight happens
// first in the normal install flow but callers that skip straight to
// InstallPlan (e.g. programmatic reinstalls) must still get a consistent
// selection — so InstallPlan resolves on-demand if the cache is cold.
// Installer for Python-based providers (vLLM, MLX, etc.).
// ProgressReporter is satisfied through the embedded BaseInstaller —
// read i.Progress inside InstallPlan to drive the pip parser's
// SetDownloadProgress fan-out. nil when the plan is inspected without
// a running install.
type Installer struct {
	install.BaseInstaller
	cfg Config

	selectMu sync.Mutex
	chosen   *interpreter.Choice
}

// New creates a pip/venv installer from the given config.
func New(cfg Config) *Installer {
	inst := &Installer{cfg: cfg}
	inst.BaseInstaller = install.NewBaseInstaller(cfg.Name, inst)
	return inst
}

// defaultPipTimeout is the fallback budget for step 3 (pip install) when
// a provider doesn't declare its own PipTimeout. 30 minutes covers small-
// to-medium Python runtimes (mlx-lm, llama-index, anything whose wheel
// set fits in a few hundred MB) with slack for a slow mirror. Providers
// whose dependency graph pulls multi-GB binary wheels (vllm + torch + the
// cu128 extra index is ~8 GB) override via Config.PipTimeout.
//
// This used to be a hardcoded 15 * time.Minute, which the vllm install
// regularly hit on anything slower than a datacenter link — the install
// would SIGKILL at the step-deadline with 11 GB of wheels already on
// disk, leaving a half-built venv and an "install failed" error that
// said nothing about it being a timeout rather than a real failure.
const defaultPipTimeout = 30 * time.Minute

func pipInstallTimeout(configured time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return defaultPipTimeout
}

// probeInterpreterVersion runs `<python> --version` and returns the parsed
// "major.minor.patch" string. Used by HealthCheck for drift detection
// and by backfillManifest for upgrade-path manifest generation. We do NOT
// route this through interpreter.Select because the target interpreter
// here is already known — Select is a *discovery* helper, and reusing it
// would re-probe PATH when we already hold the exact path of interest.
func probeInterpreterVersion(ctx context.Context, path string) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := host.CommandContext(probeCtx, path, "--version").CombinedOutput()
	if err != nil {
		return "", err
	}
	var maj, min, pat int
	n, _ := fmt.Sscanf(string(out), "Python %d.%d.%d", &maj, &min, &pat)
	if n < 2 {
		return "", fmt.Errorf("unrecognized --version output: %q", string(out))
	}
	return fmt.Sprintf("%d.%d.%d", maj, min, pat), nil
}

// sameMinor returns true when two "major.minor.patch" strings share the
// same (major, minor) tuple. Patch differences are ignored — the whole
// point of recording the interpreter version is to catch minor-version
// drift (3.13 → 3.14), not every brew point-upgrade.
func sameMinor(a, b string) bool {
	var aMaj, aMin, aPat int
	var bMaj, bMin, bPat int
	_, _ = fmt.Sscanf(a, "%d.%d.%d", &aMaj, &aMin, &aPat)
	_, _ = fmt.Sscanf(b, "%d.%d.%d", &bMaj, &bMin, &bPat)
	return aMaj == bMaj && aMin == bMin
}

// versionSatisfies evaluates "actual in [Min, Max)". Min is inclusive,
// Max is exclusive-by-minor — consistent with the semantics interpreter.Select
// uses. A nil or zero req accepts anything.
func versionSatisfies(actual string, req *config.PythonRequirement) bool {
	if req == nil || (req.Min == "" && req.Max == "") {
		return true
	}
	var aMaj, aMin, aPat int
	_, _ = fmt.Sscanf(actual, "%d.%d.%d", &aMaj, &aMin, &aPat)
	if req.Min != "" {
		var rMaj, rMin int
		_, _ = fmt.Sscanf(req.Min, "%d.%d", &rMaj, &rMin)
		if aMaj < rMaj || (aMaj == rMaj && aMin < rMin) {
			return false
		}
	}
	if req.Max != "" {
		var rMaj, rMin int
		_, _ = fmt.Sscanf(req.Max, "%d.%d", &rMaj, &rMin)
		if aMaj > rMaj || (aMaj == rMaj && aMin >= rMin) {
			return false
		}
	}
	return true
}

// describeRange matches interpreter.describeRange for the pythonvenv-local
// error messages. Duplicated rather than exported because the stringified
// range is a UI concern that may diverge between the two packages later
// (backfill messages want a different phrasing than first-install hints).
func describeRange(req *config.PythonRequirement) string {
	if req == nil || (req.Min == "" && req.Max == "") {
		return "any Python 3.x"
	}
	switch {
	case req.Min != "" && req.Max != "":
		return fmt.Sprintf(">=%s,<%s", req.Min, req.Max)
	case req.Min != "":
		return ">=" + req.Min
	default:
		return "<" + req.Max
	}
}

func (i *Installer) SupportedPlatforms() []fsroot.Platform {
	return i.cfg.Platforms
}

// PythonPath identifies this runtime's managed interpreter, never a host interpreter.
func (i *Installer) PythonPath() string { return fsroot.ProviderVenvPython(i.cfg.Name) }

// IsInstalled gates on the version file (written as a late install step),
// not the venv python. A crashed pip install leaves the venv in place with
// incomplete packages; using the venv python as the marker would make the
// partial corpse look installed and block recovery. The version file is
// the honest "install completed" signal.
//
// IsInstalled intentionally does NOT re-exec the venv's python — drift
// detection is a separate concern handled by HealthCheck. Conflating the
// two would redefine IsInstalled from "install completed" to "install
// completed AND currently healthy", which breaks callers (dispatcher,
// uninstall gating) that rely on the completion semantics.
func (i *Installer) IsInstalled() bool {
	return fsroot.ReadInstalledVersion(i.cfg.Name) != ""
}

// HealthCheck verifies an installed Python provider's venv is still
// usable at runtime. It's the drift-detection counterpart to IsInstalled
// and is called at startup / before-first-use. Failure modes it catches:
//
//   - Missing install manifest on a pre-manifest install → backfills if
//     the existing venv python is in range, else surfaces a reinstall prompt.
//   - Manifest.Interpreter.Path no longer exists (brew uninstalled the
//     versioned keg, user moved pyenv shim, etc.).
//   - Manifest.Interpreter.Realpath differs from current EvalSymlinks of
//     Path → brew point-upgrade replaced the underlying binary. If the
//     new realpath still reports the same minor, we update the manifest
//     quietly; if the minor shifted (e.g. brew 3.13→3.14), we fail.
//   - venv's own bin/python3 (or Scripts\python.exe) can't exec → venv
//     was left behind after the framework it symlinks into was deleted.
//
// Returns nil when everything is consistent. A non-nil error describes
// the drift and is safe to surface directly to the user.
func (i *Installer) HealthCheck(ctx context.Context, reqs *config.AppRequirements) error {
	if !i.IsInstalled() {
		return fmt.Errorf("%s is not installed", i.cfg.Name)
	}

	venvPython := fsroot.ProviderVenvPython(i.cfg.Name)
	if _, err := os.Stat(venvPython); err != nil {
		return fmt.Errorf("%s venv python missing at %s: %w", i.cfg.Name, venvPython, err)
	}
	// Cheap liveness probe — if the underlying framework got GC'd by brew,
	// this fails with a dyld error the user can actually read.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := host.CommandContext(probeCtx, venvPython, "-c", "import sys; sys.exit(0)").Run(); err != nil {
		return fmt.Errorf("%s venv python at %s cannot execute: %w", i.cfg.Name, venvPython, err)
	}

	manifest, err := fsroot.ReadInstallManifest(i.cfg.Name)
	if err != nil {
		return fmt.Errorf("%s install manifest corrupt: %w", i.cfg.Name, err)
	}
	if manifest == nil || manifest.Interpreter == nil {
		// Pre-manifest install. Backfill by probing the venv's own python
		// rather than re-running Select on the host — that tells us what
		// the venv IS, not what Select thinks it should be, and the whole
		// point of backfill is to record reality without re-evaluating it.
		if err := i.backfillManifest(ctx, reqs); err != nil {
			return err
		}
		return nil
	}

	interp := manifest.Interpreter
	if _, err := os.Stat(interp.Path); err != nil {
		return fmt.Errorf(
			"%s was installed with %s (%s); that interpreter is no longer present. "+
				"Reinstall the provider to pick a current interpreter",
			i.cfg.Name, interp.Path, interp.Version,
		)
	}
	currentRealpath, err := filepath.EvalSymlinks(interp.Path)
	if err != nil {
		currentRealpath = interp.Path
	}
	if currentRealpath != interp.Realpath {
		// Underlying binary shifted. Probe version on the new realpath —
		// same minor = silently record, different minor = hard fail.
		newVer, probeErr := probeInterpreterVersion(ctx, interp.Path)
		if probeErr != nil {
			return fmt.Errorf(
				"%s interpreter at %s was upgraded in place but no longer runs: %w",
				i.cfg.Name, interp.Path, probeErr,
			)
		}
		if !sameMinor(newVer, interp.Version) {
			return fmt.Errorf(
				"%s was installed with %s but %s now reports %s (different minor). "+
					"Reinstall the provider to recreate the venv against the new minor",
				i.cfg.Name, interp.Version, interp.Path, newVer,
			)
		}
		// Same minor, different realpath → record the new realpath so
		// future HealthChecks compare against today's truth.
		manifest.Interpreter.Realpath = currentRealpath
		if writeErr := fsroot.WriteInstallManifest(i.cfg.Name, manifest); writeErr != nil {
			// Non-fatal: the install is healthy; we just can't update
			// the manifest. Surface as a warning on the next caller.
			return nil
		}
	}
	return nil
}

// backfillManifest handles the one-time upgrade path for installs that
// predate the manifest. We read the venv's internal pyvenv.cfg (faster
// than forking the interpreter and avoids a second probe), extract the
// version, and persist a manifest so subsequent HealthCheck calls have
// something to compare against. If the existing venv's Python falls
// outside the new range requirement we surface a reinstall prompt
// rather than silently trusting it — the whole purpose of widening the
// schema was to catch exactly this "already-installed-and-wrong" state.
func (i *Installer) backfillManifest(ctx context.Context, reqs *config.AppRequirements) error {
	venvPython := fsroot.ProviderVenvPython(i.cfg.Name)
	ver, err := probeInterpreterVersion(ctx, venvPython)
	if err != nil {
		return fmt.Errorf("%s venv at %s: cannot determine interpreter version: %w", i.cfg.Name, venvPython, err)
	}
	var req *config.PythonRequirement
	if reqs != nil {
		req = reqs.Python
	}
	if !versionSatisfies(ver, req) {
		return fmt.Errorf(
			"%s is installed with Python %s but the provider now requires %s. "+
				"Reinstall the provider to recreate the venv against a supported interpreter",
			i.cfg.Name, ver, describeRange(req),
		)
	}
	realpath, err := filepath.EvalSymlinks(venvPython)
	if err != nil {
		realpath = venvPython
	}
	return fsroot.WriteInstallManifest(i.cfg.Name, &fsroot.InstallManifest{
		Version: fsroot.ReadInstalledVersion(i.cfg.Name),
		Interpreter: &fsroot.ManifestInterpreter{
			Path:     venvPython, // backfill uses the venv python itself as the anchor
			Realpath: realpath,
			Version:  ver,
		},
	})
}

func (i *Installer) InstalledVersion() (string, error) {
	if i.cfg.Recipe != nil {
		return i.ResolveInstalledPackage()
	}
	python := fsroot.ProviderVenvPython(i.cfg.Name)
	if _, err := os.Stat(python); err != nil {
		return "", fmt.Errorf("%s not installed", i.cfg.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := host.CommandContext(ctx, python, "-c",
		fmt.Sprintf("import importlib.metadata; print(importlib.metadata.version('%s'))", i.cfg.PipPackage))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown", nil
	}
	return install.ParseVersionOutput(string(output)), nil
}

// Preflight checks disk, interpreter selection, venv module, and (for GPU
// providers) GPU availability. The interpreter Select call runs here, its
// outcome is cached on the Installer, and InstallPlan reuses it — one
// selection per install flow. Re-probing is wasteful and (if PATH mutates
// mid-flow) non-deterministic.
func (i *Installer) Preflight(ctx context.Context, reqs *config.AppRequirements) *preflight.Report {
	report := &preflight.Report{Provider: i.cfg.Name, AllOK: true}
	if reqs == nil {
		reqs = &config.AppRequirements{}
	}

	// Disk space + write permissions
	report.Results = append(report.Results, preflight.DiskAndPermissions(i.cfg.Name, reqs.DiskSpace)...)

	// Python interpreter selection — stashed on the installer for InstallPlan.
	choice, selectErr := i.ensureInterpreter(ctx, reqs.Python)
	report.Results = append(report.Results, interpreterResult(choice, selectErr, reqs.Python))
	if selectErr == nil {
		report.Results = append(report.Results, preflight.VenvModule(ctx, choice.Path))
	}

	// GPU — policy comes from the provider config. vLLM declares
	// policy "required" so the check becomes a hard gate; other
	// python-venv providers can declare "advisory" and still run.
	if result, ok := preflight.GPU(ctx, reqs.GPU); ok {
		report.Results = append(report.Results, result)
	}

	for _, r := range report.Results {
		if !r.Passed {
			report.AllOK = false
			break
		}
	}
	return report
}

// ensureInterpreter resolves the Python interpreter once per installer,
// caching the Choice so Preflight → InstallPlan doesn't double-probe.
// Concurrent callers are serialized through selectMu so two goroutines
// (unlikely in practice, but possible via the dispatcher) can't race
// on writing chosen.
func (i *Installer) ensureInterpreter(ctx context.Context, req *config.PythonRequirement) (*interpreter.Choice, error) {
	i.selectMu.Lock()
	defer i.selectMu.Unlock()
	if i.chosen != nil {
		return i.chosen, nil
	}
	c, err := interpreter.Select(ctx, req, interpreter.Options{})
	if err != nil {
		return nil, err
	}
	i.chosen = &c
	return i.chosen, nil
}

// interpreterResult renders a preflight.Result for the interpreter
// selection outcome. The success message names the version and the
// absolute path so the preflight output is self-describing — the user
// can see "we're going to use /opt/homebrew/opt/python@3.13/bin/
// python3.13 (3.13.4)" before any venv work starts.
func interpreterResult(choice *interpreter.Choice, err error, req *config.PythonRequirement) preflight.Result {
	if err == nil {
		return preflight.Result{
			Check:   "python interpreter",
			Passed:  true,
			Message: fmt.Sprintf("using Python %s at %s", choice.Version, choice.Path),
		}
	}
	hint := interpreter.InstallHint(req)
	if errors.Is(err, interpreter.ErrNoMatch) {
		return preflight.Result{
			Check:   "python interpreter",
			Passed:  false,
			Message: err.Error(),
			Hint:    hint,
		}
	}
	return preflight.Result{
		Check:   "python interpreter",
		Passed:  false,
		Message: fmt.Sprintf("interpreter probe failed: %v", err),
		Hint:    hint,
	}
}

func (i *Installer) checkPlatform(current fsroot.Platform) error {
	if len(i.cfg.Platforms) == 0 {
		return nil
	}
	for _, platform := range i.cfg.Platforms {
		if platform.OS == current.OS && (platform.Arch == "" || platform.Arch == current.Arch) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not supported on %s/%s", install.ErrUnsupportedPlatform, i.cfg.Name, current.OS, current.Arch)
}

func (i *Installer) InstallPlan(ctx context.Context, version string) (*install.Plan, error) {
	if err := i.checkPlatform(fsroot.CurrentPlatform()); err != nil {
		return nil, err
	}
	if i.cfg.Recipe != nil {
		return i.recipePlan(ctx, version)
	}
	if err := fsroot.ValidateVersion(version); err != nil {
		return nil, err
	}

	// Resolve interpreter. Preflight usually seeds i.chosen; programmatic
	// reinstall paths skip Preflight, so we re-run Select here. The mutex
	// in ensureInterpreter also closes the double-work race. We don't have
	// the AppRequirements here — the caller (install executor) passes them
	// to Preflight, not InstallPlan — so InstallPlan requires Preflight
	// to have run first when the provider has a non-default range. That's
	// documented behavior on the Installer type, enforced by the error below.
	chosen := i.chosen
	if chosen == nil {
		c, err := interpreter.Select(ctx, nil, interpreter.Options{})
		if err != nil {
			return nil, fmt.Errorf(
				"%s install: no interpreter cached and nil-range Select also failed: "+
					"call Preflight first with the provider's AppRequirements.Python range: %w",
				i.cfg.Name, err,
			)
		}
		i.selectMu.Lock()
		i.chosen = &c
		i.selectMu.Unlock()
		chosen = &c
	}

	installDir := fsroot.ProviderDir(i.cfg.Name)
	venvDir := fsroot.ProviderVenvDir(i.cfg.Name)
	python := fsroot.ProviderVenvPython(i.cfg.Name)
	pip := fsroot.ProviderVenvPip(i.cfg.Name)

	pipSpec := i.cfg.PipPackage
	if version != "" {
		pipSpec = fmt.Sprintf("%s==%s", i.cfg.PipPackage, version)
	}
	// Append constraint specs (e.g., "transformers<5")
	for _, constraint := range i.cfg.PipConstraints {
		pipSpec += " " + fsroot.ShellQuote(constraint)
	}

	pipFlags := ""
	if i.cfg.OnlyBinary == nil || *i.cfg.OnlyBinary {
		pipFlags = "--only-binary=:all:"
	}

	qInstallDir := fsroot.ShellQuote(installDir)
	qVenvDir := fsroot.ShellQuote(venvDir)
	qPython := fsroot.ShellQuote(python)
	qPip := fsroot.ShellQuote(pip)
	qInterp := fsroot.ShellQuote(chosen.Path) // absolute path — survives PATH churn, handles spaces

	steps := []install.Step{
		{
			Number:      1,
			Description: "Create provider directory",
			Command:     fmt.Sprintf("mkdir -p %s", qInstallDir),
			Timeout:     10 * time.Second,
			Verify:      install.StepVerify{Type: "dir_exists", Path: installDir},
		},
		{
			Number:      2,
			Description: fmt.Sprintf("Create Python %s virtual environment", chosen.Version),
			Command:     fmt.Sprintf("%s -m venv %s", qInterp, qVenvDir),
			Timeout:     2 * time.Minute,
			Verify:      install.StepVerify{Type: "file_exists", Path: python},
			// Invoked via the absolute interpreter path from interpreter.Select,
			// NOT the shell's current `python3` alias. This is the fix for the
			// Homebrew-default-drift incident — the venv self-pins against the
			// exact interpreter chosen, so later `brew upgrade` can't re-point
			// new installs into a minor the provider hasn't validated.
		},
		{
			Number:      3,
			Description: fmt.Sprintf("Install %s", i.cfg.PipPackage),
			Command:     fmt.Sprintf("%s install %s %s", qPip, pipFlags, pipSpec),
			Timeout:     pipInstallTimeout(i.cfg.PipTimeout),

			// Parse pip's per-line stdout so the install progress handle
			// (and the SSE stream it fans onto) surfaces Collecting /
			// Downloading events and within-wheel byte progress.
			// UsePTY attaches a pseudo-terminal on Unix so pip enables
			// its live progress bar; on Windows the field is ignored
			// and file-level announcements still flow.
			StdoutLine: i.makePipProgressHook(),
			UsePTY:     true,
		},
	}

	// Optional read-only hardening — gated by node.yaml providers.harden_venv.
	// Skipping this step is safe: the venv is already under the provider dir
	// which has tight perms, and some hosts (mixed-ownership, shared pip
	// cache) can't chmod the tree at all.
	if fsroot.IsHardenVenvEnabled() {
		steps = append(steps, install.Step{
			Number:      len(steps) + 1,
			Description: "Set venv to read-only (security hardening)",
			Command:     fmt.Sprintf("chmod -R a-w %s", qVenvDir),
			Timeout:     2 * time.Minute,
			Optional:    true, // May fail on Linux if venv has mixed ownership (pip cache, etc.)
			Verify: install.StepVerify{
				Type:     "command_output",
				Command:  fmt.Sprintf("test ! -w %s/pyvenv.cfg && echo readonly", fsroot.ShellQuote(venvDir)),
				Expected: "readonly",
			},
		})
	}

	if i.cfg.Checks != nil {
		checks, err := i.cfg.Checks(i.cfg.SchemaProvider, i.cfg.Name)
		if err != nil {
			return nil, fmt.Errorf("runtime diagnostics: %w", err)
		}
		command, err := install.RuntimeCheckCommand(fsroot.ProviderVenvPython(i.cfg.Name), checks)
		if err != nil {
			return nil, err
		}
		var environment map[string]string
		if i.cfg.Environment != nil {
			environment = i.cfg.Environment(i.cfg.SchemaProvider)
		}
		steps = append(steps, install.Step{
			Number: len(steps) + 1, Description: "Verify runtime dependencies and startup imports",
			Command: command, Timeout: 90 * time.Second,
			Verify: install.StepVerify{Type: "runtime_checks", Python: fsroot.ProviderVenvPython(i.cfg.Name), RuntimeChecks: &checks, Environment: environment},
		})
	} else if i.cfg.VerifyStep != nil {
		verify := *i.cfg.VerifyStep
		verify.Number = len(steps) + 1
		steps = append(steps, verify)
	} else {
		steps = append(steps, install.Step{
			Number: len(steps) + 1, Description: fmt.Sprintf("Verify %s can load", i.cfg.PipPackage),
			Command: fmt.Sprintf("%s -c \"import %s; print('OK')\"", qPython, i.cfg.ImportName),
			Timeout: 30 * time.Second,
			Verify:  install.StepVerify{Type: "command_output", Command: fmt.Sprintf("%s -c \"import %s; print('OK')\"", qPython, i.cfg.ImportName), Expected: "OK"},
		})
	}

	// Record-version MUST precede the manifest step — IsInstalled gates
	// on the version file, so writing it earlier would let a partial
	// install look complete if a later required step fails. If the
	// manifest write itself fails, IsInstalled is already true — that's
	// intentional: HealthCheck will see a missing manifest on an
	// otherwise-complete install and backfill it, which is a recoverable
	// state. The alternative (manifest before version) would flip the
	// failure class from "recoverable drift" to "orphan manifest without
	// a provider", which has no backfill path.
	//
	// The recorded version is the *actually installed* pip version, not
	// the requested one — pip may resolve to a compatible version that
	// differs from the spec, and an empty request (no version passed via
	// the API, no pinned_version wired through) must still produce a real
	// version string rather than an empty file. We probe via
	// importlib.metadata on the venv python after the pip step completes.
	qVersionFile := fsroot.ShellQuote(fsroot.ProviderVersionFile(i.cfg.Name))
	// PipPackage is quoted inside the Python -c source with double quotes
	// (the shell single-quote wrapping makes single-quotes inside the -c
	// arg awkward). Package names can only contain [A-Za-z0-9._-] so there
	// is no shell- or Python-injection surface here.
	steps = append(steps, install.Step{
		Number:      len(steps) + 1,
		Description: "Record installed version",
		Command: fmt.Sprintf(
			`%s -c 'import importlib.metadata; print(importlib.metadata.version("%s"))' > %s`,
			qPython, i.cfg.PipPackage, qVersionFile,
		),
		Timeout: 10 * time.Second,
		Verify:  install.StepVerify{Type: "file_exists", Path: fsroot.ProviderVersionFile(i.cfg.Name)},
	})

	// Write the install manifest via a PostExec hook rather than a shell
	// command: the manifest is a JSON document with structured fields,
	// not a one-line string, and Go-side encoding keeps the serialized
	// shape honest. The step still has a file_exists verify so both
	// Execute() and ExecuteStep() (guided mode) can confirm the write
	// landed.
	manifestPath := fsroot.ProviderManifestPath(i.cfg.Name)
	chosenCopy := *chosen // capture for the closure — chosen itself is an installer-mutable pointer
	manifestStep := install.Step{
		Number:      len(steps) + 1,
		Description: "Record install manifest (interpreter pin + realpath fingerprint)",
		Timeout:     5 * time.Second,
		Verify:      install.StepVerify{Type: "file_exists", Path: manifestPath},
		PostExec: func(_ context.Context) error {
			return fsroot.WriteInstallManifest(i.cfg.Name, &fsroot.InstallManifest{
				Version:     version,
				InstalledAt: utils.NowUTC().Format(time.RFC3339),
				Interpreter: &fsroot.ManifestInterpreter{
					Path:     chosenCopy.Path,
					Realpath: chosenCopy.Realpath,
					Version:  chosenCopy.Version,
				},
			})
		},
	}
	steps = append(steps, manifestStep)

	return &install.Plan{
		Provider:   i.cfg.Name,
		Version:    version,
		Platform:   fsroot.CurrentPlatform(),
		Action:     "install",
		InstallDir: installDir,
		Steps:      steps,
	}, nil
}

func (i *Installer) UpgradePlan(ctx context.Context, toVersion string) (*install.Plan, error) {
	options := install.OptionsFromContext(ctx)
	options.Action = "upgrade"
	ctx = install.WithPlanOptions(ctx, options)
	plan, err := i.InstallPlan(ctx, toVersion)
	if err != nil {
		return nil, err
	}
	if i.cfg.Recipe != nil {
		return plan, nil
	}
	return install.BuildUpgradePlan(i.cfg.Name, plan, true), nil
}

func (i *Installer) UninstallPlan(_ context.Context) (*install.Plan, error) {
	installDir := fsroot.ProviderDir(i.cfg.Name)
	qDir := fsroot.ShellQuote(installDir)
	return &install.Plan{
		Provider:   i.cfg.Name,
		Platform:   fsroot.CurrentPlatform(),
		Action:     "uninstall",
		InstallDir: installDir,
		Steps: []install.Step{
			{
				Number:      1,
				Description: "Make installation writable (undo security hardening)",
				Command:     fmt.Sprintf("chmod -R u+w %s", qDir),
				Optional:    true, // May fail if hardening step was skipped
				Verify:      install.StepVerify{Type: "dir_exists", Path: installDir},
			},
			{
				Number:      2,
				Description: fmt.Sprintf("Remove %s installation", i.cfg.Name),
				Command:     fmt.Sprintf("rm -rf %s", qDir),
				Verify: install.StepVerify{
					Type:     "command_output",
					Command:  fmt.Sprintf("test ! -d %s && echo removed", qDir),
					Expected: "removed",
				},
			},
		},
	}, nil
}
