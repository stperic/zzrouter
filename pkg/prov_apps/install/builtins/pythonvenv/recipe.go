package pythonvenv

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/interpreter"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
	"github.com/stperic/zzrouter/pkg/utils"
)

//go:embed toolkit.py
var managedToolkitLayout string

func (i *Installer) recipePlan(ctx context.Context, version string) (*install.Plan, error) {
	snapshot, err := i.cfg.Recipe(i.cfg.SchemaProvider, i.cfg.Name)
	if err != nil {
		return nil, err
	}
	recipe := snapshot.Recipe
	var requirement *config.PythonRequirement
	if snapshot.Requirements != nil {
		requirement = snapshot.Requirements.Python
	}
	choice, err := interpreter.Select(ctx, requirement, interpreter.Options{})
	if err != nil {
		return nil, err
	}
	chosen := &choice
	version, err = resolveRecipeVersion(ctx, snapshot, version, install.OptionsFromContext(ctx).Action, upstream.LatestMatching)
	if err != nil {
		return nil, err
	}
	if err := fsroot.ValidateVersion(version); err != nil {
		return nil, err
	}
	checks := schema.RuntimeChecks{Kernels: "unknown"}
	if i.cfg.Checks != nil {
		checks, err = i.cfg.Checks(snapshot.Provider, snapshot.Runtime)
		if err != nil {
			return nil, err
		}
	}
	checks = snapshot.RuntimeChecks(checks)
	environment, err := install.ControlledEnvironment()
	if err != nil {
		return nil, err
	}
	directory := fsroot.ProviderDir(i.cfg.Name)
	requirements := []string{*recipe.Package + "==" + version}
	for _, name := range slices.Sorted(maps.Keys(recipe.Companions)) {
		constraint := companionConstraint(snapshot, name, value(recipe.Companions[name].Constraint))
		requirements = append(requirements, name+constraint)
	}
	flags := []string{"--disable-pip-version-check", "install", "--no-input", "--index-url", *recipe.Indexes.Primary}
	if *recipe.OnlyBinary {
		flags = append(flags, "--only-binary=:all:")
	}
	if recipe.Indexes.Extra != nil {
		for _, index := range *recipe.Indexes.Extra {
			flags = append(flags, "--extra-index-url", index)
		}
	}
	flags = append(flags, requirements...)
	timeout, _ := time.ParseDuration(*recipe.Timeout)
	runtimeEnv := map[string]string{}
	if i.cfg.Environment != nil {
		runtimeEnv = i.cfg.Environment(snapshot.Provider)
	}
	if err := install.ValidateRuntimeEnvironment(runtimeEnv); err != nil {
		return nil, err
	}
	options := install.OptionsFromContext(ctx)
	action := "install"
	if options.Action != "" {
		action = options.Action
	}
	plan := &install.Plan{Provider: i.cfg.Name, Version: version, Platform: fsroot.CurrentPlatform(), Action: action, InstallDir: directory, Recipe: &snapshot, Environment: environment, Disposable: options.Disposable, SupplyChainNotice: "Index approval does not constrain artifact/CDN egress. Use an operator-controlled proxy or mirror when destination control is required. Extra indexes have no priority and can introduce dependency confusion."}
	if !*recipe.OnlyBinary {
		plan.SupplyChainNotice += " Authorized source builds also execute resolver-selected PEP 517 build dependencies; those are outside the hash-pinned runtime inventory."
	}
	baseID := install.Fingerprint(struct{ Recipe, Policy, Node, Runtime, Version, Interpreter, Environment, RuntimeEnvironment, Checks string }{snapshot.Fingerprint, snapshot.PolicyFingerprint, snapshot.Node, snapshot.Runtime, version, chosen.Path + ":" + chosen.Realpath + ":" + chosen.Version, install.Fingerprint(environment), install.Fingerprint(runtimeEnv), install.Fingerprint(checks)})
	plan.PlanID = baseID
	guard := func(ctx context.Context) error { return install.CheckRecipeAuthority(ctx, snapshot, i.cfg.Recipe) }

	configure := func() error {
		return i.configureRecipePlan(plan, chosen, runtimeEnv, checks, flags, requirements, timeout, options)
	}
	if err := configure(); err != nil {
		return nil, err
	}
	plan.Preflight = func(ctx context.Context) error {
		if err := guard(ctx); err != nil {
			return err
		}
		if err := install.ValidateRecipeSources(ctx, snapshot); err != nil {
			return err
		}
		roots := map[string]string{*recipe.Package: "==" + version}
		for name, companion := range recipe.Companions {
			roots[name] = companionConstraint(snapshot, name, value(companion.Constraint))
		}
		inventory, err := resolvePackages(ctx, directory, chosen.Path, environment, flags, roots)
		if err != nil {
			return err
		}
		plan.Inventory = inventory
		plan.PlanID = install.Fingerprint(struct {
			Snapshot  string
			Inventory []install.ResolvedPackage
		}{baseID, inventory})
		return configure()
	}
	return plan, nil
}

func resolveRecipeVersion(ctx context.Context, snapshot install.RecipeSnapshot, version, action string, latest func(context.Context, string, string) (upstream.Release, error)) (string, error) {
	recipe := snapshot.Recipe
	if version == "" {
		if action != "upgrade" {
			version = snapshot.PinnedVersion
		}
	}
	if version == "" {
		release, err := latest(ctx, *recipe.Package, recipeConstraint(snapshot))
		if err != nil {
			return "", err
		}
		version = release.Tag
	}
	if err := upstream.VersionAllowed(version, value(recipe.VersionConstraint)); err != nil {
		return "", err
	}
	if snapshot.Policy != nil {
		if err := upstream.VersionAllowed(version, snapshot.Policy.Packages[*recipe.Package]); err != nil {
			return "", fmt.Errorf("%w: %v", install.ErrInstallPolicy, err)
		}
	}
	return version, nil
}

func (i *Installer) configureRecipePlan(plan *install.Plan, chosen *interpreter.Choice, runtimeEnv map[string]string, checks schema.RuntimeChecks, flags, requirements []string, timeout time.Duration, options install.PlanOptions) error {
	snapshot := *plan.Recipe
	recipe := snapshot.Recipe
	directory, version, action, environment := plan.InstallDir, plan.Version, plan.Action, plan.Environment
	guard := func(ctx context.Context) error { return install.CheckRecipeAuthority(ctx, snapshot, i.cfg.Recipe) }
	identity := install.BoundPlanID(plan.PlanID, action, options)
	stage := filepath.Join(directory, ".candidate", identity)
	if options.Disposable {
		stage = install.DisposableDir(i.cfg.Name, identity)
	}
	venv := filepath.Join(stage, "venv")
	python := venvPython(venv)
	livePython := fsroot.ProviderVenvPython(i.cfg.Name)
	stageEnv := maps.Clone(runtimeEnv)
	if stageEnv == nil {
		stageEnv = map[string]string{}
	}
	liveEnv := maps.Clone(stageEnv)
	stageToolkit := install.ToolkitEnvironmentAt(venv, recipe, chosen.Version)
	liveToolkit := install.ToolkitEnvironmentAt(fsroot.ProviderVenvDir(i.cfg.Name), recipe, chosen.Version)
	for key, value := range stageToolkit.Variables() {
		stageEnv[key] = value
	}
	for key, value := range liveToolkit.Variables() {
		liveEnv[key] = value
	}
	verify := install.StepVerify{Type: "runtime_checks", Python: python, RuntimeChecks: &checks, Environment: stageEnv, ExecutionEnvironment: environment, Toolkit: stageToolkit, Transient: true}
	pinnedFlags := slices.Clone(flags)
	if len(plan.Inventory) > 0 {
		pinnedFlags = append(slices.Clone(flags[:len(flags)-len(requirements)]), "--require-hashes", "--no-deps", "-r", filepath.Join(stage, "requirements.txt"))
	}
	command := quoteCommand(python, append([]string{"-I", "-m", "pip"}, pinnedFlags...)...)
	verifyCmd, err := install.RuntimeCheckCommand(python, checks)
	if err != nil {
		return err
	}
	state := &runtimeTransaction{directory: directory, stage: stage}
	plan.Rollback = func() error {
		if err := state.rollback(); err != nil {
			return err
		}
		return removeManagedTree(stage)
	}
	candidateGuard := func(ctx context.Context) error {
		if err := guard(ctx); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(stage, "plan_id"))
		if err != nil || string(data) != identity {
			return install.ErrStalePlan
		}
		return nil
	}
	plan.Steps = []install.Step{
		{Number: 1, Description: "Prepare candidate runtime beside the active installation", PreExec: guard, PostExec: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := prepareCandidate(stage); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(stage, "plan_id"), []byte(identity), 0600); err != nil {
				return err
			}
			if len(plan.Inventory) == 0 {
				return fmt.Errorf("dependency preflight required before execution")
			}
			return os.WriteFile(filepath.Join(stage, "requirements.txt"), []byte(install.InventoryRequirements(plan.Inventory)), 0600)
		}, Verify: install.StepVerify{Type: "dir_exists", Path: stage, Transient: true}},
		{Number: 2, Description: "Create managed Python virtual environment", PreExec: candidateGuard, Command: quoteCommand(chosen.Path, "-m", "venv", venv), Timeout: 2 * time.Minute, Verify: install.StepVerify{Type: "file_exists", Path: python, Transient: true}},
		{Number: 3, Description: "Install the accepted dependency inventory", PreExec: candidateGuard, Command: command, Timeout: timeout, StdoutLine: i.makePipProgressHook(), UsePTY: true},
		{Number: 4, Description: "Verify candidate dependencies and startup imports", PreExec: candidateGuard, Command: verifyCmd, Timeout: 90 * time.Second, Verify: verify},
		{Number: 5, Description: "Activate candidate, verify, and restore previous runtime on failure", PreExec: candidateGuard, PostExec: func(ctx context.Context) error {
			if results := install.RunRuntimeChecksSnapshot(ctx, python, checks, stageEnv, environment, stageToolkit); !checksPassed(results) {
				return fmt.Errorf("candidate verification failed: %v", results)
			}
			return state.activate(ctx, checks, liveEnv, environment, version, snapshot, chosen.Path, chosen.Realpath, chosen.Version, plan.Inventory, identity)
		}, Verify: install.StepVerify{Type: "runtime_checks", Python: livePython, RuntimeChecks: &checks, Environment: liveEnv, ExecutionEnvironment: environment, Toolkit: liveToolkit}},
	}
	if options.Disposable {
		plan.Rollback = func() error { return nil }
		finalVerify := verify
		finalVerify.Transient = false
		plan.Steps[len(plan.Steps)-1] = install.Step{Description: "Verify and record disposable runtime without replacing the active installation", PreExec: candidateGuard, PostExec: func(ctx context.Context) error {
			results := install.RunRuntimeChecksSnapshot(ctx, python, checks, stageEnv, environment, stageToolkit)
			if !checksPassed(results) {
				return fmt.Errorf("disposable runtime verification failed: %v", results)
			}
			return install.WriteDisposableManifest(i.cfg.Name, identity, install.DisposableManifest{Provider: snapshot.Provider, Runtime: snapshot.Runtime, PlanID: identity, Version: version, Recipe: snapshot, PythonVersion: chosen.Version, Checks: checks, Inventory: plan.Inventory, RuntimeEnvironmentFingerprint: install.Fingerprint(runtimeEnv)})
		}, Verify: finalVerify}
	}
	if value(recipe.Toolkit) != "" {
		setup := install.Step{Description: "Prepare release-owned compiler layout inside the managed runtime", PreExec: candidateGuard, Command: quoteCommand(python, "-I", "-c", managedToolkitLayout), Timeout: time.Minute}
		plan.Steps = append(plan.Steps[:3], append([]install.Step{setup}, plan.Steps[3:]...)...)
	}
	for index := range plan.Steps {
		plan.Steps[index].Number = index + 1
		plan.Steps[index].ExecutionEnvironment = slices.Clone(environment)
		if plan.Steps[index].Verify.Type == "runtime_checks" {
			plan.Steps[index].Verify.PreVerify = guard
			merged, err := install.MergeExecutionEnvironment(environment, plan.Steps[index].Verify.Environment)
			if err != nil {
				return err
			}
			composed, err := plan.Steps[index].Verify.Toolkit.Compose(merged)
			if err != nil {
				return err
			}
			plan.Steps[index].ExecutionEnvironment = composed
		}
	}
	return nil
}

func checksPassed(results []install.RuntimeCheck) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if !result.Passed {
			return false
		}
	}
	return true
}
func companionConstraint(snapshot install.RecipeSnapshot, name, constraint string) string {
	if snapshot.Policy != nil && snapshot.Policy.Packages[name] != "" {
		if constraint != "" {
			constraint += ","
		}
		constraint += snapshot.Policy.Packages[name]
	}
	return constraint
}

func recipeConstraint(snapshot install.RecipeSnapshot) string {
	constraint := value(snapshot.Recipe.VersionConstraint)
	if snapshot.Policy != nil && snapshot.Policy.Packages[*snapshot.Recipe.Package] != "" {
		if constraint != "" {
			constraint += ","
		}
		constraint += snapshot.Policy.Packages[*snapshot.Recipe.Package]
	}
	return constraint
}

func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func quoteCommand(command string, args ...string) string {
	parts := []string{fsroot.ShellQuote(command)}
	for _, arg := range args {
		parts = append(parts, fsroot.ShellQuote(arg))
	}
	return strings.Join(parts, " ")
}
func venvPython(directory string) string {
	if fsroot.CurrentPlatform().OS == "windows" {
		return filepath.Join(directory, "Scripts", "python.exe")
	}
	return filepath.Join(directory, "bin", "python")
}

func prepareCandidate(stage string) error {
	if err := removeManagedTree(stage); err != nil {
		return err
	}
	return os.MkdirAll(stage, 0700)
}

func setTreeWritable(directory string, writable bool) error {
	return filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() &^ 0222
		if writable {
			mode = info.Mode().Perm() | 0200
		}
		return os.Chmod(path, mode)
	})
}

func removeManagedTree(directory string) error {
	if err := fsroot.VerifySymlinkSafe(directory); err != nil {
		return err
	}
	if err := setTreeWritable(directory, true); err != nil {
		return err
	}
	return os.RemoveAll(directory)
}

type runtimeTransaction struct {
	directory, stage string
	activated        bool
	previous         bool
	recordsMoved     []string
	started          bool
	newRecords       bool
	directoryModes   map[string]os.FileMode
	chmod            func(string, os.FileMode) error
}

// moveDirectory temporarily unlocks the root for macOS cross-parent renames.
func (t *runtimeTransaction) moveDirectory(from, to string) (bool, error) {
	if err := fsroot.VerifySymlinkSafe(from); err != nil {
		return false, err
	}
	if err := fsroot.VerifySymlinkSafe(to); err != nil {
		return false, err
	}
	info, err := os.Lstat(from)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("runtime directory required: %s", from)
	}
	if t.directoryModes == nil {
		t.directoryModes = map[string]os.FileMode{}
	}
	mode, saved := t.directoryModes[from]
	if !saved {
		mode = info.Mode().Perm()
	}
	if t.chmod == nil {
		t.chmod = os.Chmod
	}
	t.directoryModes[from] = mode
	if err := t.chmod(from, mode|0200); err != nil {
		return false, err
	}
	if err := os.Rename(from, to); err != nil {
		return false, errors.Join(err, t.restoreDirectoryMode(from))
	}
	delete(t.directoryModes, from)
	t.directoryModes[to] = mode
	return true, t.restoreDirectoryMode(to)
}

func (t *runtimeTransaction) restoreDirectoryMode(path string) error {
	if err := t.chmod(path, t.directoryModes[path]); err != nil {
		return err
	}
	delete(t.directoryModes, path)
	return nil
}

func (t *runtimeTransaction) activate(ctx context.Context, checks schema.RuntimeChecks, environment map[string]string, executionEnvironment []string, version string, snapshot install.RecipeSnapshot, interpreter, realpath, pythonVersion string, inventory []install.ResolvedPackage, identity string) error {
	if err := fsroot.VerifySymlinkSafe(t.directory); err != nil {
		return err
	}
	live := filepath.Join(t.directory, "venv")
	previous := filepath.Join(t.directory, ".previous")
	if err := removeManagedTree(previous); err != nil {
		return err
	}
	if err := os.MkdirAll(previous, 0700); err != nil {
		return err
	}
	t.started = true
	if _, err := os.Lstat(live); err == nil {
		moved, err := t.moveDirectory(live, filepath.Join(previous, "venv"))
		t.previous = moved
		if err != nil {
			return t.fail(err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, name := range []string{"version", "install.json"} {
		path := filepath.Join(t.directory, name)
		if _, err := os.Stat(path); err == nil {
			if err := os.Rename(path, filepath.Join(previous, name)); err != nil {
				return t.fail(err)
			}
			t.recordsMoved = append(t.recordsMoved, name)
		}
	}
	if err := relocateScripts(filepath.Join(t.stage, "venv"), live); err != nil {
		return t.fail(err)
	}
	moved, err := t.moveDirectory(filepath.Join(t.stage, "venv"), live)
	t.activated = moved
	if err != nil {
		return t.fail(err)
	}
	toolkit := install.ToolkitEnvironmentAt(live, snapshot.Recipe, pythonVersion)
	results := install.RunRuntimeChecksSnapshot(ctx, venvPython(live), checks, environment, executionEnvironment, toolkit)
	for _, result := range results {
		if !result.Passed {
			return t.fail(fmt.Errorf("activation verify %s: %s", result.Name, result.Reason))
		}
	}
	t.newRecords = true
	if err := os.WriteFile(fsroot.ProviderVersionFile(snapshot.Runtime), []byte(version+"\n"), 0600); err != nil {
		return t.fail(err)
	}
	manifest := &fsroot.InstallManifest{Version: version, InstalledAt: utils.NowUTC().Format(time.RFC3339), Interpreter: &fsroot.ManifestInterpreter{Path: interpreter, Realpath: realpath, Version: pythonVersion}, RecipeFingerprint: snapshot.Fingerprint, PolicyFingerprint: snapshot.PolicyFingerprint, Recipe: &snapshot.Recipe, PlanID: identity, Inventory: inventory}
	if err := fsroot.WriteInstallManifest(snapshot.Runtime, manifest); err != nil {
		return t.fail(err)
	}
	if fsroot.IsHardenVenvEnabled() {
		if err := setTreeWritable(live, false); err != nil {
			return t.fail(fmt.Errorf("harden managed runtime: %w", err))
		}
	}
	return nil
}

func (t *runtimeTransaction) fail(cause error) error {
	if err := t.rollback(); err != nil {
		return fmt.Errorf("installation failed: %w; rollback failed: %v", cause, err)
	}
	return fmt.Errorf("installation failed: %w; rollback completed", cause)
}

func (t *runtimeTransaction) rollback() (resultErr error) {
	defer func() {
		for path := range t.directoryModes {
			resultErr = errors.Join(resultErr, t.restoreDirectoryMode(path))
		}
	}()
	if !t.started {
		return nil
	}
	live := filepath.Join(t.directory, "venv")
	previous := filepath.Join(t.directory, ".previous")
	var modeErrors []error
	if t.activated {
		failed := filepath.Join(t.stage, "failed-venv")
		moved, err := t.moveDirectory(live, failed)
		if !moved {
			return err
		}
		t.activated = false
		modeErrors = append(modeErrors, err)
	}
	if t.previous {
		moved, err := t.moveDirectory(filepath.Join(previous, "venv"), live)
		if !moved {
			return errors.Join(append(modeErrors, err)...)
		}
		t.previous = false
		modeErrors = append(modeErrors, err)
	}
	if t.newRecords {
		for _, name := range []string{"version", "install.json", "install.json.tmp"} {
			if err := os.Remove(filepath.Join(t.directory, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		t.newRecords = false
	}
	for len(t.recordsMoved) > 0 {
		name := t.recordsMoved[0]
		path := filepath.Join(previous, name)
		if _, err := os.Stat(path); err == nil {
			if err := os.Rename(path, filepath.Join(t.directory, name)); err != nil {
				return err
			}
		}
		t.recordsMoved = t.recordsMoved[1:]
	}
	t.activated = false
	t.previous = false
	t.recordsMoved = nil
	t.started = false
	return errors.Join(modeErrors...)
}

func relocateScripts(candidate, live string) error {
	subdir := "bin"
	if fsroot.CurrentPlatform().OS == "windows" {
		subdir = "Scripts"
	}
	entries, err := os.ReadDir(filepath.Join(candidate, subdir))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(candidate, subdir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > 1<<20 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "\x00") {
			continue
		}
		text := strings.ReplaceAll(string(data), candidate, live)
		if text != string(data) {
			if err := os.WriteFile(path, []byte(text), info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

// ResolveInstalledPackage reads the configured package without importing it.
func (i *Installer) ResolveInstalledPackage() (string, error) {
	packageName := ""
	manifest, err := fsroot.ReadInstallManifest(i.cfg.Name)
	if err != nil {
		return "", err
	}
	if manifest != nil && manifest.Recipe != nil && manifest.Recipe.Package != nil {
		packageName = *manifest.Recipe.Package
	} else {
		snapshot, err := i.cfg.Recipe(i.cfg.SchemaProvider, i.cfg.Name)
		if err != nil {
			return "", err
		}
		packageName = *snapshot.Recipe.Package
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := host.CommandContext(ctx, fsroot.ProviderVenvPython(i.cfg.Name), "-I", "-c", "import importlib.metadata,sys;print(importlib.metadata.version(sys.argv[1]))", packageName)
	environment, err := process.ChildEnvironment(nil)
	if err != nil {
		return "", err
	}
	command.Env = environment
	data, err := command.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Install executes the resolved plan without deleting the previous environment.
func (i *Installer) Install(ctx context.Context, version string, progress func(string)) error {
	if i.cfg.Recipe == nil {
		return i.BaseInstaller.Install(ctx, version, progress)
	}
	plan, err := i.InstallPlan(ctx, version)
	if err != nil {
		return err
	}
	if plan.Preflight != nil {
		if err := plan.Preflight(ctx); err != nil {
			return err
		}
	}
	plan.BindOptions(install.OptionsFromContext(ctx))
	return i.executeRecipe(ctx, plan, progress)
}

// Upgrade uses the same candidate transaction as a forced reinstall.
func (i *Installer) Upgrade(ctx context.Context, version string, progress func(string)) error {
	if i.cfg.Recipe == nil {
		return i.BaseInstaller.Upgrade(ctx, version, progress)
	}
	plan, err := i.UpgradePlan(ctx, version)
	if err != nil {
		return err
	}
	if plan.Preflight != nil {
		if err := plan.Preflight(ctx); err != nil {
			return err
		}
	}
	plan.BindOptions(install.OptionsFromContext(ctx))
	return i.executeRecipe(ctx, plan, progress)
}

func (i *Installer) executeRecipe(ctx context.Context, plan *install.Plan, progress func(string)) error {
	lock := fsroot.NewLockFile(i.cfg.Name)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	if err := plan.Execute(ctx, progress); err != nil {
		if rollbackErr := plan.Rollback(); rollbackErr != nil {
			return fmt.Errorf("installation failed: %w; rollback failed: %v", err, rollbackErr)
		}
		return fmt.Errorf("installation failed: %w; previous runtime preserved or restored", err)
	}
	return nil
}
