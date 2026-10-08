package install

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// nameNormalizer strips dots and hyphens so that "llamacpp" matches "llama.cpp".
var nameNormalizer = strings.NewReplacer(".", "", "-", "")

// ProviderInstaller defines the install/upgrade/uninstall contract per provider.
// Each provider (llama.cpp, vLLM, MLX) implements this interface.
type ProviderInstaller interface {
	// Automated — zzrouter executes everything
	Install(ctx context.Context, version string, progress func(string)) error
	Upgrade(ctx context.Context, toVersion string, progress func(string)) error
	Uninstall(ctx context.Context) error
	Rollback(ctx context.Context) error

	// Guided — returns structured steps for manual execution.
	// ctx bounds any I/O the plan builder performs (GitHub version
	// resolution, hardware probes for variant selection) and, when
	// wrapped with gpu.WithCachedInventory, lets preflight and plan-
	// building share a single hardware probe.
	InstallPlan(ctx context.Context, version string) (*Plan, error)
	UpgradePlan(ctx context.Context, toVersion string) (*Plan, error)
	UninstallPlan(ctx context.Context) (*Plan, error)

	// Queries.
	//
	// IsInstalled MUST be scoped to zzrouter-managed state — e.g., the
	// version file under ProviderDir(name), a .managed marker, or the
	// install manifest — and MUST NOT return true for system-level
	// installs that happen to be on $PATH. It is the answer to "did we
	// put this here", which is what decides whether removing it is ours
	// to do: uninstalling an install we merely found would run
	// `brew uninstall` or delete a model directory the operator owns.
	//
	// That is enforced, not merely documented — UninstallAsync and
	// UpgradeAsync both refuse with ErrProviderNotManaged when this
	// returns false and a version was nonetheless detected. Providers
	// found but not managed DO surface as Dormant so the UI can show
	// them; being visible is not being removable.
	//
	// InstalledVersion is allowed to probe the installed package (pip
	// importlib.metadata for pythonvenv, llama-server --version for
	// llamacpp) because it only runs when IsInstalled has already
	// returned true, i.e. when the caller has proven the install is
	// ours. If you're writing a new installer, gate IsInstalled on
	// something zzrouter writes, not on something the user might.
	InstalledVersion() (string, error)
	IsInstalled() bool
	ProviderName() string
	SupportedPlatforms() []fsroot.Platform

	// Preflight checks system prerequisites (disk, permissions, python, GPU, tools).
	// Requirements come from provider config; pass nil to use built-in defaults.
	// ctx bounds GPU probes; callers that will also invoke InstallPlan should
	// pre-wrap with gpu.WithCachedInventory so both paths share one probe.
	Preflight(ctx context.Context, reqs *config.AppRequirements) *PreflightReport
}

// BinaryLocator is an optional interface implemented by installers whose
// provider runs as a long-lived process from a path this node controls.
//
// It exists so a caller can ask who is actually running, which is the one
// question a version comparison cannot answer: a daemon started outside
// zzRouter reports whatever version it is, including the one we installed.
//
// ManagedBinary returns "" when there is no managed install to name, and
// callers must treat that as "cannot tell" rather than "nothing running".
// Implementations MUST return the binary the MANAGED install runs from --
// not merely the first one on $PATH, which is how an operator's own copy
// would end up being reported as ours.
type BinaryLocator interface {
	ManagedBinary() string
}

// DaemonInstaller describes a managed, long-lived runtime. Arguments are fixed
// by the installer; neither the API nor provider config supplies an executable.
type DaemonInstaller interface {
	BinaryLocator
	DaemonCommand() (binary string, args []string)
}

// ProgressReporter is an optional interface implemented by installers that
// expose byte-level download progress via an InstallProgress handle.
type ProgressReporter interface {
	SetProgress(*InstallProgress)
}

// ServiceEnvFunc reports the environment a provider's config declares for
// the long-lived process it starts, and is wired into the installers that
// start one (see builtins.Register).
//
// Read when a plan is BUILT rather than when the installer is registered,
// so an edited config lands on the next start without re-wiring anything.
// A provider whose daemon is started by its install plan has no other way
// to receive it: a service manager is what applies env to an external
// service, and a managed install creates no service unit.
//
// A nil func, or a nil map, means "nothing configured" — the daemon then
// inherits only what the install step itself runs with.
type ServiceEnvFunc func(provider string) map[string]string

// PlanProvider is the subset of ProviderInstaller that each provider must implement.
// The shared BaseInstaller handles the boilerplate Install/Upgrade/Uninstall/Rollback.
type PlanProvider interface {
	InstallPlan(ctx context.Context, version string) (*Plan, error)
	UpgradePlan(ctx context.Context, toVersion string) (*Plan, error)
	UninstallPlan(ctx context.Context) (*Plan, error)
}

// BaseInstaller provides shared Install/Upgrade/Uninstall/Rollback logic.
// Concrete installers embed this and implement only the *Plan methods +
// queries. `pp` is unexported; subpackages set it via NewBaseInstaller.
//
// The embedded Progress field + SetProgress method together satisfy
// ProgressReporter so every concrete installer inherits the wiring for
// free — installers that care about byte-level download telemetry just
// read b.Progress inside their InstallPlan. SetProgress is called by
// the InstallCoordinator before Install runs and cleared (nil) via
// deferred cleanup.
type BaseInstaller struct {
	Name     string
	pp       PlanProvider
	Progress *InstallProgress
}

// SetProgress satisfies ProgressReporter. Embedded in every concrete
// installer via BaseInstaller so individual builtins don't need to
// re-declare it.
func (b *BaseInstaller) SetProgress(p *InstallProgress) {
	b.Progress = p
}

// NewBaseInstaller wires a per-provider PlanProvider into the shared
// Install/Upgrade/Uninstall/Rollback runtime. Subpackage installers call
// this in their New() constructor and embed the returned value.
func NewBaseInstaller(name string, pp PlanProvider) BaseInstaller {
	return BaseInstaller{Name: name, pp: pp}
}

func (b *BaseInstaller) Install(ctx context.Context, version string, progress func(string)) error {
	if err := fsroot.EnsureProviderRoot(); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	lock := fsroot.NewLockFile(b.Name)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()

	plan, err := b.pp.InstallPlan(ctx, version)
	if err != nil {
		return err
	}
	return plan.Execute(ctx, progress)
}

func (b *BaseInstaller) Upgrade(ctx context.Context, toVersion string, progress func(string)) error {
	lock := fsroot.NewLockFile(b.Name)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()

	plan, err := b.pp.UpgradePlan(ctx, toVersion)
	if err != nil {
		return err
	}
	if err := plan.Execute(ctx, progress); err != nil {
		_ = fsroot.Restore(b.Name)
		return fmt.Errorf("upgrade failed, rolled back: %w", err)
	}

	if err := fsroot.CleanupBackup(b.Name); err != nil {
		slog.Warn("Failed to clean up backup (will be overwritten on next upgrade)",
			"provider", b.Name, "error", err)
	}
	return nil
}

func (b *BaseInstaller) Uninstall(ctx context.Context) error {
	plan, err := b.pp.UninstallPlan(ctx)
	if err != nil {
		return err
	}
	return plan.Execute(ctx, nil)
}

func (b *BaseInstaller) Rollback(_ context.Context) error {
	return fsroot.Restore(b.Name)
}

func (b *BaseInstaller) ProviderName() string {
	return b.Name
}

// Dispatcher routes install requests to the correct per-provider installer.
type Dispatcher struct {
	installers map[string]ProviderInstaller
	mu         sync.RWMutex
}

// NewDispatcher creates an empty install dispatcher. The built-in provider
// installers live in pkg/prov_apps/install/builtins; call
// builtins.NewDispatcher to get a pre-wired dispatcher, or this
// constructor + builtins.Register to populate an existing one. Keeping
// the two apart means install root has no knowledge of concrete providers.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		installers: make(map[string]ProviderInstaller),
	}
}

// Register adds a provider installer.
func (d *Dispatcher) Register(name string, installer ProviderInstaller) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.installers[name] = installer
}

// Get returns the installer for a provider.
// Tries an exact match first, then falls back to normalized matching
// (e.g., "llamacpp" resolves to the "llama.cpp" installer).
func (d *Dispatcher) Get(name string) (ProviderInstaller, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if inst, ok := d.installers[name]; ok {
		return inst, nil
	}
	// Fuzzy match: normalize both the query and registered keys.
	norm := nameNormalizer.Replace(name)
	for key, inst := range d.installers {
		if nameNormalizer.Replace(key) == norm {
			return inst, nil
		}
	}
	return nil, fmt.Errorf("no installer for provider %q", name)
}

// List returns all registered provider names.
func (d *Dispatcher) List() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	names := make([]string, 0, len(d.installers))
	for name := range d.installers {
		names = append(names, name)
	}
	return names
}

// IsInstalled checks if a provider is installed via its installer.
// Returns false if no installer is registered for the provider.
func (d *Dispatcher) IsInstalled(name string) bool {
	inst, err := d.Get(name)
	if err != nil {
		return false
	}
	return inst.IsInstalled()
}

// BuildUpgradePlan wraps an install plan with backup steps for the upgrade path.
// If makeVenvWritable is true, a chmod step is prepended to unlock a hardened venv.
func BuildUpgradePlan(name string, installPlan *Plan, makeVenvWritable bool) *Plan {
	installPlan.Action = "upgrade"

	var prepend []Step

	if makeVenvWritable {
		venvDir := fsroot.ProviderVenvDir(name)
		prepend = append(prepend, Step{
			Description: "Make venv writable for upgrade",
			Command:     fmt.Sprintf("chmod -R u+w %s", fsroot.ShellQuote(venvDir)),
			Verify:      StepVerify{Type: "dir_exists", Path: venvDir},
		})
	}

	provDir := fsroot.ProviderDir(name)
	backupDir := fsroot.ProviderBackupDir(name)

	if fsroot.CurrentPlatform().OS == "windows" {
		prepend = append(prepend, Step{
			Description: "Backup current installation",
			Command:     fmt.Sprintf("powershell -Command \"Copy-Item -Path '%s' -Destination '%s' -Recurse -Force\"", provDir, backupDir),
			Notes:       "Creates a backup for rollback in case the upgrade fails",
			Verify:      StepVerify{Type: "dir_exists", Path: backupDir},
		})
	} else {
		prepend = append(prepend, Step{
			Description: "Backup current installation",
			Command:     fmt.Sprintf("cp -r %s %s", fsroot.ShellQuote(provDir), fsroot.ShellQuote(backupDir)),
			Notes:       "Creates a backup for rollback in case the upgrade fails",
			Verify:      StepVerify{Type: "dir_exists", Path: backupDir},
		})
	}

	installPlan.Steps = append(prepend, installPlan.Steps...)
	for idx := range installPlan.Steps {
		installPlan.Steps[idx].Number = idx + 1
	}
	return installPlan
}

// --- Version parsing (shared by all installers) ---

// versionLineRe matches "version: <token>" in multi-line output (llama.cpp format).
var versionLineRe = regexp.MustCompile(`(?m)^version:\s*(\S+)`)

// semverRe matches semver-like patterns (e.g., "0.11.0", "1.2.3-beta").
var semverRe = regexp.MustCompile(`\b\d+\.\d+\.\d+(?:-[\w.]+)?\b`)

// ParseVersionOutput extracts a version string from command output.
// Handles noisy output (e.g., llama-server prints GPU init lines before the version).
// Strategy:
//  1. Look for "version: <token>" line (llama.cpp format)
//  2. Look for a semver-like pattern anywhere in the output
//  3. Fall back to the last non-empty line (pip-based providers)
func ParseVersionOutput(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return "unknown"
	}

	// 1. Explicit "version: ..." line
	if m := versionLineRe.FindStringSubmatch(output); len(m) > 1 {
		return m[1]
	}

	// 2. Semver pattern anywhere
	if m := semverRe.FindString(output); m != "" {
		return m
	}

	// 3. Last non-empty line (works for pip-based providers like vLLM/MLX).
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}

	return "unknown"
}
