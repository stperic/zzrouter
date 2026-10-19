// Package strata installs the Niko1221/Strata inference engine in a
// zzRouter-owned directory. Model preparation runs inside the launcher.
package strata

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/interpreter"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
	"github.com/stperic/zzrouter/pkg/security"
)

//go:embed launcher.py
var launcher string

const cudaMajor = 13

const repository = "Niko1221/Strata"
const repositoryURL = "https://github.com/" + repository + ".git"

var releaseVersion = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){2,3}$`)

// Installer manages versioned Strata engines and the shared draft layer.
type Installer struct {
	install.BaseInstaller
	latestRelease func(context.Context, string) (string, error)
	// serviceEnv reports the environment the provider config declares, such
	// as upstream's STRATA_NVCC toolkit pin.
	serviceEnv install.ServiceEnvFunc
}

// New creates the Strata installer. serviceEnv may be nil.
func New(serviceEnv install.ServiceEnvFunc) *Installer {
	i := &Installer{latestRelease: latestVersion, serviceEnv: serviceEnv}
	i.BaseInstaller = install.NewBaseInstaller("strata", i)
	return i
}

func latestVersion(ctx context.Context, provider string) (string, error) {
	release, err := install.LatestUpstreamRelease(ctx, provider)
	if err != nil {
		return "", err
	}
	return release.Version, nil
}

// SupportedPlatforms limits installs to the launcher's Linux platform.
func (i *Installer) SupportedPlatforms() []fsroot.Platform {
	return []fsroot.Platform{{OS: "linux", Arch: "amd64"}}
}

func releaseDir(version string) string {
	return filepath.Join(fsroot.ProviderDir("strata"), "releases", version)
}

func previousVersionFile() string {
	return filepath.Join(fsroot.ProviderDir("strata"), "previous-version")
}

// IsInstalled requires managed release files and the current embedded launcher.
func (i *Installer) IsInstalled() bool {
	_, err := i.InstalledVersion()
	return err == nil
}

// InstalledVersion reports the complete managed release or its verification failure.
func (i *Installer) InstalledVersion() (string, error) {
	if !fsroot.ManagedMarkerExists(i.Name) {
		return "", fmt.Errorf("strata is not managed by zzRouter on this node")
	}
	v := fsroot.ReadInstalledVersion(i.Name)
	if !releaseVersion.MatchString(v) {
		return "", fmt.Errorf("strata installed version %q is invalid", v)
	}
	if err := verifyRelease(v); err != nil {
		return "", err
	}
	return v, nil
}

func launcherPath() string {
	return filepath.Join(fsroot.ProviderBinDir("strata"), "strata-launch")
}

func verifyLauncher(context.Context) error {
	if err := fsroot.VerifyProviderPath(fsroot.ProviderDir("strata"), filepath.Join("bin", "strata-launch")); err != nil {
		return err
	}
	body, err := os.ReadFile(launcherPath())
	if err != nil {
		return err
	}
	if sha256.Sum256(body) != sha256.Sum256([]byte(launcher)) {
		return fmt.Errorf("strata launcher differs from this zzRouter build; reinstall Strata")
	}
	return nil
}

func verifyRelease(version string) error {
	root := fsroot.ProviderDir("strata")
	for _, path := range []string{
		filepath.Join(releaseDir(version), "engine", "strata"),
		filepath.Join(releaseDir(version), ".venv", "bin", "python"),
		filepath.Join(releaseDir(version), "serve", "server.py"),
		filepath.Join(root, "data", "mtp", "rt", "experts.bin"),
	} {
		if _, err := os.Stat(path); err != nil {
			return err
		}
	}
	if err := fsroot.VerifyProviderPath(root, filepath.Join("releases", version, "engine", "strata")); err != nil {
		return err
	}
	return verifyLauncher(context.Background())
}

// Preflight checks engine build prerequisites.
func (i *Installer) Preflight(ctx context.Context, reqs *config.AppRequirements) *preflight.Report {
	if reqs == nil {
		reqs = &config.AppRequirements{DiskSpace: "20GB"}
	}
	results := preflight.DiskAndPermissions(i.Name, reqs.DiskSpace)
	results = append(results, preflight.Commands("git", "curl", "tar")...)
	if runtime.GOOS == "linux" {
		results = append(results, preflight.Commands("c++")...)
		choice, err := interpreter.Select(ctx, reqs.Python, interpreter.Options{})
		if err != nil {
			results = append(results, preflight.Result{Check: "python", Message: err.Error(), Hint: "Install a Python with venv that meets the provider's requirements.python"})
		} else {
			results = append(results, preflight.Result{Check: "python", Passed: true, Message: choice.Version})
			results = append(results, preflight.VenvModule(ctx, choice.Path))
		}
	}

	return preflight.NewReport(i.Name, results)
}

func (i *Installer) environment() map[string]string {
	if i.serviceEnv == nil {
		return nil
	}
	return i.serviceEnv(i.Name)
}

// InstallPlan builds an engine without selecting or downloading model weights.
func (i *Installer) InstallPlan(ctx context.Context, requested string) (*install.Plan, error) {
	src, err := install.ProviderVersionSource(i.Name)
	if err != nil {
		return nil, err
	}
	if src.Type != config.VersionSourceGitHubRelease || src.Repo != repository {
		return nil, fmt.Errorf("strata version_source must be github_release %s", repository)
	}
	if err := fsroot.ValidateVersion(requested); err != nil {
		return nil, err
	}
	version := src.Normalize(requested)
	if version == "" {
		version, err = i.latestRelease(ctx, i.Name)
		if err != nil {
			return nil, fmt.Errorf("resolve Strata release: %w", err)
		}
	}
	if !releaseVersion.MatchString(version) {
		return nil, fmt.Errorf("strata release %q must be a numeric version with three or four components", version)
	}
	tag := src.Tag(version)
	root := fsroot.ProviderDir(i.Name)
	dir := releaseDir(version)
	data := filepath.Join(root, "data")
	qRoot, qDir, qData := fsroot.ShellQuote(root), fsroot.ShellQuote(dir), fsroot.ShellQuote(data)
	qRepo, qTag := fsroot.ShellQuote(repositoryURL), fsroot.ShellQuote(tag)
	if err := fsroot.VerifyProviderPath(root, filepath.Join("releases", version)); err != nil {
		return nil, err
	}
	if err := fsroot.VerifyProviderPath(root, "data"); err != nil {
		return nil, err
	}
	clone := fmt.Sprintf("if [ -d %s/.git ]; then test \"$(git -C %s remote get-url origin)\" = %s && test \"$(git -C %s describe --tags --exact-match HEAD)\" = %s; else test ! -e %s && git clone --depth 1 --branch %s %s %s; fi", qDir, qDir, qRepo, qDir, qTag, qDir, qTag, qRepo, qDir)
	verifyClone := fmt.Sprintf("test \"$(git -C %s remote get-url origin)\" = %s && test \"$(git -C %s describe --tags --exact-match HEAD)\" = %s && echo ok", qDir, qRepo, qDir, qTag)

	setupEnv, err := i.setupEnvironment(dir)
	if err != nil {
		return nil, err
	}
	plan := &install.Plan{
		Provider: i.Name, Version: version, Platform: fsroot.CurrentPlatform(),
		Action: "install", InstallDir: root,
		SupplyChainNotice: "Builds Niko1221/Strata from the selected release and downloads its shared MTP draft layer.",
	}
	plan.Steps = []install.Step{
		{Number: 1, Description: "Create Strata install and data directories", Command: fmt.Sprintf("mkdir -p %s %s %s", qRoot, fsroot.ShellQuote(filepath.Join(root, "releases")), qData), Timeout: time.Minute, Verify: install.StepVerify{Type: "dir_exists", Path: data}},
		{Number: 2, Description: "Fetch Niko1221/Strata " + tag, Command: clone, Timeout: 20 * time.Minute, Verify: install.StepVerify{Type: "command_output", Command: verifyClone, Expected: "ok"}},
	}
	apps, err := install.LoadAppsConfig()
	if err != nil {
		return nil, err
	}
	svc, ok := apps.LookupApp(i.Name)
	if !ok {
		return nil, fmt.Errorf("strata provider configuration is missing")
	}
	var required *config.PythonRequirement
	if svc.Requirements != nil {
		required = svc.Requirements.Python
	}
	choice, err := interpreter.Select(ctx, required, interpreter.Options{})
	if err != nil {
		return nil, fmt.Errorf("strata needs a Python with venv that meets requirements.python: %w", err)
	}
	python := filepath.Join(dir, ".venv", "bin", "python")
	qPython := fsroot.ShellQuote(python)
	build := qPython + " -c " + fsroot.ShellQuote(fmt.Sprintf(`import setup
llama = setup.get_llama_cpp()
cuda_major = %d
setup.build_engine(setup.update_engine_gpu(cuda_major), "none", True, llama)
`, cudaMajor))
	mtp := filepath.Join(data, "mtp")
	rt := filepath.Join(mtp, "rt")
	prepareMTP := qPython + " -c " + fsroot.ShellQuote(fmt.Sprintf(`import os
import subprocess
import sys
from pathlib import Path
import setup
mtp = Path(%q)
rt = mtp / "rt"
if not (rt / "experts.bin").is_file():
    env = dict(os.environ, STRATA_SOURCE=setup.model_source())
    subprocess.run([sys.executable, "tools/mtp_fetch.py", "fetch", "--out", str(mtp)], env=env, check=True)
    subprocess.run([sys.executable, "tools/mtp_pack.py", "--src", str(mtp), "--experts", "q2_0", "--out", str(mtp / "mtp-q2_0.gguf")], check=True)
    subprocess.run([sys.executable, "tools/mtp_rt.py", "--gguf", str(mtp / "mtp-q2_0.gguf"), "--out", str(rt)], check=True)
setup.refresh_draft_vocab(rt)
`, mtp))
	setupEnv, err = install.MergeExecutionEnvironment(setupEnv, map[string]string{"STRATA_GGUF_PY": filepath.Join(dir, "third_party", "llama.cpp", "gguf-py")})
	if err != nil {
		return nil, err
	}
	if err := fsroot.VerifyProviderPath(root, filepath.Join("bin", "strata-launch")); err != nil {
		return nil, err
	}
	qLauncher := fsroot.ShellQuote(launcherPath())
	verifyEngine := fmt.Sprintf("families=$(%s --families) && printf '%%s\\n' \"$families\" | tr '[:space:]' '\\n' | grep -Fx qwen4exp", fsroot.ShellQuote(filepath.Join(dir, "engine", "strata")))
	plan.Steps = append(plan.Steps,
		install.Step{
			Number: 3, Description: "Create private Strata Python environment",
			Command: fmt.Sprintf("%s -m venv %s", fsroot.ShellQuote(choice.Path), fsroot.ShellQuote(filepath.Join(dir, ".venv"))),
			Timeout: 5 * time.Minute, Verify: install.StepVerify{Type: "file_exists", Path: python},
		},
		install.Step{
			Number: 4, Description: "Install Strata build dependencies",
			Command: qPython + " -m pip install -r requirements.txt", WorkingDir: dir,
			ExecutionEnvironment: setupEnv, Timeout: 20 * time.Minute,
		},
		install.Step{
			Number: 5, Description: "Build Strata engine", Command: build, WorkingDir: dir,
			ExecutionEnvironment: setupEnv, Timeout: 30 * time.Minute,
			Verify: install.StepVerify{Type: "file_exists", Path: filepath.Join(dir, "engine", "strata")},
		},
		install.Step{
			Number: 6, Description: "Verify Strata architecture support", Command: verifyEngine,
			WorkingDir: dir, ExecutionEnvironment: setupEnv, Timeout: time.Minute,
			Verify: install.StepVerify{Type: "command_output", Command: verifyEngine, Expected: "qwen4exp"},
		},
		install.Step{
			Number: 7, Description: "Prepare shared MTP draft layer", Command: prepareMTP, WorkingDir: dir,
			ExecutionEnvironment: setupEnv, Timeout: 8 * time.Hour,
			Verify: install.StepVerify{Type: "file_exists", Path: filepath.Join(rt, "experts.bin")},
		},
		install.Step{
			Number: 8, Description: "Write Strata on-demand launcher",
			Command: fmt.Sprintf("mkdir -p %s && printf %%s %s > %s && chmod 755 %s", fsroot.ShellQuote(fsroot.ProviderBinDir(i.Name)), fsroot.ShellQuote(launcher), qLauncher, qLauncher),
			Timeout: time.Minute, Verify: install.StepVerify{Type: "file_exists", Path: launcherPath(), PreVerify: verifyLauncher},
		},
	)

	old := fsroot.ReadInstalledVersion(i.Name)
	if old != "" && old != version {
		if !releaseVersion.MatchString(old) {
			return nil, fmt.Errorf("installed Strata release %q is invalid", old)
		}
	}
	if old != "" && old != version {
		plan.Steps = append(plan.Steps, install.Step{Number: len(plan.Steps) + 1, Description: "Record the previous verified release", Command: fsroot.WriteVersionCommand(old, previousVersionFile()), Verify: install.StepVerify{Type: "file_equals", Path: previousVersionFile(), Expected: old}, Timeout: 5 * time.Second})
	}
	plan.Steps = append(plan.Steps, install.WriteManagedMarkerStep(len(plan.Steps)+1, i.Name))
	plan.Steps = append(plan.Steps, install.Step{Number: len(plan.Steps) + 1, Description: "Activate verified Strata release", Command: fsroot.WriteVersionCommand(version, fsroot.ProviderVersionFile(i.Name)), Verify: install.StepVerify{Type: "file_equals", Path: fsroot.ProviderVersionFile(i.Name), Expected: version, PlanScoped: true}, Timeout: 5 * time.Second})
	for n := range plan.Steps {
		plan.Steps[n].StdoutLine = i.setupProgress(plan.Steps[n].Number, len(plan.Steps))
	}
	return plan, nil
}

// setupEnvironment gives upstream setup the controlled environment managed
// Python installs use plus the provider's declared variables, never zzRouter's
// own process environment. The private venv leads PATH so upstream builds with
// the CMake and Ninja it installs there.
func (i *Installer) setupEnvironment(dir string) ([]string, error) {
	base, err := install.ControlledEnvironment()
	if err != nil {
		return nil, err
	}
	merged, err := install.MergeExecutionEnvironment(base, i.environment())
	if err != nil {
		return nil, fmt.Errorf("strata provider environment: %w", err)
	}
	for n, entry := range merged {
		// An empty PATH would leave setup with no system tools, or the checkout itself as a search directory.
		if path, ok := strings.CutPrefix(entry, "PATH="); ok && path != "" {
			merged[n] = "PATH=" + filepath.Join(dir, ".venv", "bin") + string(os.PathListSeparator) + path
			return merged, nil
		}
	}
	return nil, fmt.Errorf("strata setup needs the service PATH")
}

func (i *Installer) setupProgress(step, total int) func(string) {
	p := i.Progress
	if p == nil {
		return nil
	}
	return func(line string) {
		if line != "" {
			p.SetStep(step, total, security.RedactSensitive(line))
		}
	}
}

// UpgradePlan prepares a replacement release beside the active one.
func (i *Installer) UpgradePlan(ctx context.Context, version string) (*install.Plan, error) {
	plan, err := i.InstallPlan(ctx, version)
	if err != nil {
		return nil, err
	}
	plan.Action = "upgrade"
	return plan, nil
}

// Upgrade does not use BaseInstaller's whole-root backup: model data is
// deliberately shared between releases and must never be moved or removed
// by an unsuccessful code upgrade.
func (i *Installer) Upgrade(ctx context.Context, version string, progress func(string)) error {
	lock := fsroot.NewLockFile(i.Name)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	plan, err := i.UpgradePlan(ctx, version)
	if err != nil {
		return err
	}
	return plan.Execute(ctx, progress)
}

// Rollback selects the previous verified release; the coordinator owns run admission.
func (i *Installer) Rollback(_ context.Context) error {
	lock := fsroot.NewLockFile(i.Name)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	previous, err := os.ReadFile(previousVersionFile())
	if err != nil {
		return fmt.Errorf("no previous Strata release: %w", err)
	}
	v := strings.TrimSpace(string(previous))
	if !releaseVersion.MatchString(v) {
		return fmt.Errorf("invalid previous Strata release %q", v)
	}
	if err := verifyRelease(v); err != nil {
		return fmt.Errorf("previous Strata release is incomplete: %w", err)
	}
	path := fsroot.ProviderVersionFile(i.Name)
	tmp := path + ".rollback.tmp"
	if err := fsroot.VerifySymlinkSafe(path); err != nil {
		return err
	}
	if err := fsroot.VerifySymlinkSafe(tmp); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, []byte(v+"\n"), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// UninstallPlan detaches management while retaining recoverable data.
func (i *Installer) UninstallPlan(_ context.Context) (*install.Plan, error) {
	root := fsroot.ProviderDir(i.Name)
	marker := fsroot.ManagedMarkerPath(i.Name)
	version := fsroot.ProviderVersionFile(i.Name)
	previous := previousVersionFile()
	qMarker, qVersion, qPrevious := fsroot.ShellQuote(marker), fsroot.ShellQuote(version), fsroot.ShellQuote(previous)
	return &install.Plan{Provider: i.Name, Platform: fsroot.CurrentPlatform(), Action: "uninstall", InstallDir: root, Steps: []install.Step{
		{Number: 1, Description: "Detach managed Strata install", Command: fmt.Sprintf("rm -f %s %s %s", qMarker, qVersion, qPrevious), Timeout: 10 * time.Second, Verify: install.StepVerify{Type: "command_output", Command: fmt.Sprintf("test ! -e %s && test ! -e %s && echo detached", qMarker, qVersion), Expected: "detached"}, Notes: "Retains packs, the shared draft layer and releases for recovery."},
	}}, nil
}
