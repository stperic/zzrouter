// Package strata installs the Niko1221/Strata inference engine in a
// zzRouter-owned directory. The upstream setup owns model preparation.
package strata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/interpreter"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/security"
)

const repository = "Niko1221/Strata"
const repositoryURL = "https://github.com/" + repository + ".git"

var releaseVersion = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){2,3}$`)

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

func (i *Installer) SupportedPlatforms() []fsroot.Platform {
	return []fsroot.Platform{{OS: "linux", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"}}
}

func releaseDir(version string) string {
	return filepath.Join(fsroot.ProviderDir("strata"), "releases", version)
}

func previousVersionFile() string {
	return filepath.Join(fsroot.ProviderDir("strata"), "previous-version")
}

func (i *Installer) IsInstalled() bool {
	if !fsroot.ManagedMarkerExists(i.Name) {
		return false
	}
	v := fsroot.ReadInstalledVersion(i.Name)
	if !releaseVersion.MatchString(v) {
		return false
	}
	_, err := installedConfig(releaseDir(v))
	return err == nil
}

func (i *Installer) InstalledVersion() (string, error) {
	if !i.IsInstalled() {
		return "", fmt.Errorf("strata is not managed by zzRouter on this node")
	}
	return fsroot.ReadInstalledVersion(i.Name), nil
}

type modelConfig struct {
	Exe  string `json:"exe"`
	Port int    `json:"port"`
}

type managedModel struct {
	configPath string
	engine     string
	port       int
}

func installedConfig(dir string) (managedModel, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "strata-*.json"))
	if err != nil {
		return managedModel{}, err
	}
	var configs []string
	for _, path := range paths {
		if !strings.HasSuffix(path, ".shared-settings.json") {
			configs = append(configs, path)
		}
	}
	if len(configs) != 1 {
		return managedModel{}, fmt.Errorf("expected one Strata model config in %s, found %d", dir, len(configs))
	}
	if _, err := os.Stat(filepath.Join(dir, "serve", "server.py")); err != nil {
		return managedModel{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, ".venv", "bin", "python")); err != nil {
		return managedModel{}, err
	}
	data, err := os.ReadFile(configs[0])
	if err != nil {
		return managedModel{}, err
	}
	var cfg modelConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return managedModel{}, fmt.Errorf("invalid Strata model config: %w", err)
	}
	if cfg.Port < 1 || cfg.Port > 65535 || cfg.Exe == "" {
		return managedModel{}, fmt.Errorf("strata model config lacks an engine or valid port")
	}
	if !filepath.IsAbs(cfg.Exe) {
		return managedModel{}, fmt.Errorf("strata engine path is not absolute")
	}
	engine, err := filepath.EvalSymlinks(cfg.Exe)
	if err != nil {
		return managedModel{}, err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return managedModel{}, err
	}
	rel, err := filepath.Rel(realDir, engine)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return managedModel{}, fmt.Errorf("strata engine is outside the managed release")
	}
	if _, err := os.Stat(engine); err != nil {
		return managedModel{}, err
	}
	return managedModel{configPath: configs[0], engine: cfg.Exe, port: cfg.Port}, nil
}

// ManagedBinary names the native engine, which is unique to this managed
// checkout. The HTTP service itself is Python and shares its executable
// with unrelated processes, so it is unsuitable for binary-only presence.
func (i *Installer) ManagedBinary() string {
	if !i.IsInstalled() {
		return ""
	}
	dir := releaseDir(fsroot.ReadInstalledVersion(i.Name))
	model, err := installedConfig(dir)
	if err == nil {
		return model.engine
	}
	return ""
}

func (i *Installer) DaemonCommand() (string, []string) {
	if !i.IsInstalled() {
		return "", nil
	}
	dir := releaseDir(fsroot.ReadInstalledVersion(i.Name))
	model, err := installedConfig(dir)
	if err != nil {
		return "", nil
	}
	return filepath.Join(dir, ".venv", "bin", "python"), []string{
		filepath.Join(dir, "serve", "server.py"), "--engine", "strata",
		"--config", model.configPath, "--port", strconv.Itoa(model.port),
	}
}

func (i *Installer) Preflight(ctx context.Context, reqs *config.AppRequirements) *preflight.Report {
	if reqs == nil {
		reqs = &config.AppRequirements{DiskSpace: "150GB"}
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
	_, _, port, err := i.providerSettings()
	if err != nil {
		results = append(results, preflight.Result{Check: "provider configuration", Message: err.Error()})
	} else if !i.IsInstalled() {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			_ = conn.Close()
			results = append(results, preflight.Result{Check: "endpoint", Message: fmt.Sprintf("port %d is already in use", port), Hint: "Free the configured Strata port or use a different loopback endpoint"})
		} else if probeCtx.Err() != nil {
			results = append(results, preflight.Result{Check: "endpoint", Message: probeCtx.Err().Error()})
		} else if errors.Is(err, syscall.ECONNREFUSED) {
			results = append(results, preflight.Result{Check: "endpoint", Passed: true, Message: fmt.Sprintf("port %d is available", port)})
		} else {
			results = append(results, preflight.Result{Check: "endpoint", Message: err.Error()})
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

func (i *Installer) providerSettings() (*config.VersionSource, *config.ExternalProvider, int, error) {
	src, err := install.ProviderVersionSource(i.Name)
	if err != nil {
		return nil, nil, 0, err
	}
	if src.Type != config.VersionSourceGitHubRelease || src.Repo != repository {
		return nil, nil, 0, fmt.Errorf("strata version_source must be github_release %s", repository)
	}
	apps, err := install.LoadAppsConfig()
	if err != nil {
		return nil, nil, 0, err
	}
	p := apps.GetExternal(i.Name)
	if p == nil {
		return nil, nil, 0, fmt.Errorf("strata external provider config is missing")
	}
	u, err := url.Parse(p.Runtime.Endpoint)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, nil, 0, fmt.Errorf("managed Strata requires a loopback HTTP root endpoint")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, nil, 0, fmt.Errorf("managed Strata requires an explicit endpoint port")
	}
	if p.Runtime.API == nil || p.Runtime.API.AuthType != config.AuthTypeNone {
		return nil, nil, 0, fmt.Errorf("managed Strata requires keyless loopback auth")
	}
	return src, p, port, nil
}

func (i *Installer) InstallPlan(ctx context.Context, requested string) (*install.Plan, error) {
	src, p, port, err := i.providerSettings()
	if err != nil {
		return nil, err
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
	// Setup's --yes picks its recommended model family and size for this node.
	// It installs the image encoder only when the template declares vision, so
	// the catalog never advertises images the install cannot read.
	vision := "no"
	if _, ok := p.Features["vision"]; ok {
		vision = "yes"
	}
	setup := fmt.Sprintf("./setup.sh --yes --vision %s --no-start --no-browser --host 127.0.0.1 --port %d --data-dir %s", vision, port, qData)
	setupEnv, err := i.setupEnvironment(dir)
	if err != nil {
		return nil, err
	}
	plan := &install.Plan{
		Provider: i.Name, Version: version, Platform: fsroot.CurrentPlatform(),
		Action: "install", InstallDir: root,
		SupplyChainNotice: "Runs Niko1221/Strata setup.sh from the selected release; setup installs Python packages and downloads a large model.",
	}
	plan.Steps = []install.Step{
		{Number: 1, Description: "Create Strata install and data directories", Command: fmt.Sprintf("mkdir -p %s %s %s", qRoot, fsroot.ShellQuote(filepath.Join(root, "releases")), qData), Timeout: time.Minute, Verify: install.StepVerify{Type: "dir_exists", Path: data}},
		{Number: 2, Description: "Fetch Niko1221/Strata " + tag, Command: clone, Timeout: 20 * time.Minute, Verify: install.StepVerify{Type: "command_output", Command: verifyClone, Expected: "ok"}},
	}
	if runtime.GOOS == "linux" {
		var required *config.PythonRequirement
		if p.Requirements != nil {
			required = p.Requirements.Python
		}
		choice, err := interpreter.Select(ctx, required, interpreter.Options{})
		if err != nil {
			return nil, fmt.Errorf("strata needs a Python with venv that meets requirements.python: %w", err)
		}
		python := filepath.Join(dir, ".venv", "bin", "python")
		plan.Steps = append(plan.Steps, install.Step{
			Number: len(plan.Steps) + 1, Description: "Create private Strata Python environment",
			Command: fmt.Sprintf("%s -m venv %s", fsroot.ShellQuote(choice.Path), fsroot.ShellQuote(filepath.Join(dir, ".venv"))),
			Timeout: 5 * time.Minute, Verify: install.StepVerify{Type: "file_exists", Path: python},
		})
	}
	setupIndex := len(plan.Steps)
	plan.Steps = append(plan.Steps, install.Step{
		Number: len(plan.Steps) + 1, Description: "Run Strata setup and download its recommended model",
		Command: setup, WorkingDir: dir, Timeout: 8 * time.Hour, ExecutionEnvironment: setupEnv,
		Verify: install.StepVerify{Type: "file_exists", Path: filepath.Join(dir, ".venv", "bin", "python"), PreVerify: func(context.Context) error { _, err := installedConfig(dir); return err }},
		Notes:  "Upstream setup may download over 100 GB. Repeating the step resumes its model download.",
	})
	old := fsroot.ReadInstalledVersion(i.Name)
	if old != "" && old != version {
		if !releaseVersion.MatchString(old) {
			return nil, fmt.Errorf("installed Strata release %q is invalid", old)
		}
	}
	plan.Steps = append(plan.Steps, install.Step{Number: len(plan.Steps) + 1, Description: "Stop managed Strata before activation", ServiceAction: install.ServiceStop, Timeout: 30 * time.Second})
	if old != "" && old != version {
		plan.Steps = append(plan.Steps, install.Step{Number: len(plan.Steps) + 1, Description: "Record the previous verified release", Command: fsroot.WriteVersionCommand(old, previousVersionFile()), Verify: install.StepVerify{Type: "file_equals", Path: previousVersionFile(), Expected: old}, Timeout: 5 * time.Second})
	}
	plan.Steps = append(plan.Steps, install.WriteManagedMarkerStep(len(plan.Steps)+1, i.Name))
	plan.Steps = append(plan.Steps, install.Step{Number: len(plan.Steps) + 1, Description: "Activate verified Strata release", Command: fsroot.WriteVersionCommand(version, fsroot.ProviderVersionFile(i.Name)), Verify: install.StepVerify{Type: "file_equals", Path: fsroot.ProviderVersionFile(i.Name), Expected: version, PlanScoped: true}, Timeout: 5 * time.Second})
	plan.Steps = append(plan.Steps, install.Step{Number: len(plan.Steps) + 1, Description: "Start managed Strata service", ServiceAction: install.ServiceStart, Timeout: 30 * time.Second})
	plan.Steps[setupIndex].StdoutLine = i.setupProgress(plan.Steps[setupIndex].Number, len(plan.Steps))
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

func (i *Installer) Rollback(ctx context.Context) error {
	lock := fsroot.NewLockFile(i.Name)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	binary, args := i.DaemonCommand()
	if binary != "" {
		pids, err := process.DaemonPIDs(ctx, binary, args)
		if err != nil {
			return fmt.Errorf("check Strata service before rollback: %w", err)
		}
		if len(pids) != 0 {
			return fmt.Errorf("stop the Strata service before rollback")
		}
	}
	previous, err := os.ReadFile(previousVersionFile())
	if err != nil {
		return fmt.Errorf("no previous Strata release: %w", err)
	}
	v := strings.TrimSpace(string(previous))
	if !releaseVersion.MatchString(v) {
		return fmt.Errorf("invalid previous Strata release %q", v)
	}
	if _, err := installedConfig(releaseDir(v)); err != nil {
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

func (i *Installer) UninstallPlan(_ context.Context) (*install.Plan, error) {
	root := fsroot.ProviderDir(i.Name)
	marker := fsroot.ManagedMarkerPath(i.Name)
	version := fsroot.ProviderVersionFile(i.Name)
	previous := previousVersionFile()
	qMarker, qVersion, qPrevious := fsroot.ShellQuote(marker), fsroot.ShellQuote(version), fsroot.ShellQuote(previous)
	return &install.Plan{Provider: i.Name, Platform: fsroot.CurrentPlatform(), Action: "uninstall", InstallDir: root, Steps: []install.Step{
		{Number: 1, Description: "Stop managed Strata service", ServiceAction: install.ServiceStop, Timeout: 30 * time.Second},
		{Number: 2, Description: "Detach managed Strata install", Command: fmt.Sprintf("rm -f %s %s %s", qMarker, qVersion, qPrevious), Timeout: 10 * time.Second, Verify: install.StepVerify{Type: "command_output", Command: fmt.Sprintf("test ! -e %s && test ! -e %s && echo detached", qMarker, qVersion), Expected: "detached"}, Notes: "Retains downloaded model data and release files for recovery."},
	}}, nil
}
