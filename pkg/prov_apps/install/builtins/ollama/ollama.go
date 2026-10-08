// Package ollama installs Ollama across platforms. Unlike llama.cpp (which
// installs into zzRouter's managed provider dir), Ollama installs to system
// locations via its official installer and runs as a persistent service.
// zzRouter tracks the managed state via a marker file in ProviderDir("ollama")
// so it knows whether it performed the install (and therefore whether
// uninstall is allowed).
package ollama

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/download"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/variant"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
)

// Installer manages Ollama installation across platforms.
// ProgressReporter is satisfied through the embedded BaseInstaller's
// SetProgress + Progress field — read i.Progress inside InstallPlan
// to access the coordinator-wired handle.
type Installer struct {
	install.BaseInstaller

	// serviceEnv reports the environment the provider config declares.
	// Ollama is the one built-in whose daemon is started by its own
	// install plan, so the plan is the only place that environment can
	// be applied. May be nil.
	serviceEnv    install.ServiceEnvFunc
	latestRelease func(context.Context, string) (upstream.Release, error)
}

// New creates an Ollama installer. serviceEnv may be nil; the start step
// then carries no environment.
func New(serviceEnv install.ServiceEnvFunc) *Installer {
	inst := &Installer{serviceEnv: serviceEnv, latestRelease: install.LatestUpstreamRelease}
	inst.BaseInstaller = install.NewBaseInstaller("ollama", inst)
	return inst
}

func (i *Installer) SupportedPlatforms() []fsroot.Platform {
	return []fsroot.Platform{
		{OS: "darwin", Arch: "arm64"},
		{OS: "darwin", Arch: "amd64"},
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
		{OS: "windows", Arch: "amd64"},
	}
}

func (i *Installer) IsInstalled() bool {
	if !fsroot.ManagedMarkerExists(i.Name) {
		return false
	}
	return findBinary() != ""
}

// ManagedBinary implements install.BinaryLocator: the binary a managed
// install of Ollama runs from, or "" when this node has no managed install.
//
// Deliberately not findBinary(), which starts at $PATH and would happily
// name an operator's own copy. The marker is what makes an install ours,
// so it gates the answer on every platform; on darwin, where zzRouter
// installs through Homebrew and owns no path of its own, the marker is
// the ONLY thing that makes the Homebrew binary ours, and findBinary is
// then the honest way to locate it.
func (i *Installer) ManagedBinary() string {
	return i.managedBinary(runtime.GOOS)
}

func (i *Installer) managedBinary(platform string) string { //nolint:unparam // The runtime OS differs across builds; tests also select platform-specific ownership paths.
	if !fsroot.ManagedMarkerExists(i.Name) {
		return ""
	}
	return i.daemonBinary(platform)
}

// DaemonCommand implements install.DaemonInstaller.
func (i *Installer) DaemonCommand() (string, []string) {
	return i.daemonBinary(runtime.GOOS), []string{"serve"}
}

func (i *Installer) daemonBinary(platform string) string {
	if platform == "darwin" {
		return findBinary()
	}
	managed := filepath.Join(fsroot.ProviderBinDir(i.Name), binaryName())
	if platform == "windows" {
		managed = filepath.Join(windowsInstallDir(), "ollama.exe")
	}
	if _, err := os.Stat(managed); err != nil {
		return ""
	}
	return managed
}

// InstalledVersion reports the version this node's install recorded.
//
// The recorded file wins over the binary because `ollama --version` reports
// the running daemon's version first (see recordVersionStep). Probing is kept
// only for a managed install predating the version file, and even then it
// reads the client line when one is present.
func (i *Installer) InstalledVersion() (string, error) {
	if !fsroot.ManagedMarkerExists(i.Name) {
		return "", fmt.Errorf("ollama not managed by zzRouter")
	}
	if v := fsroot.ReadInstalledVersion(i.Name); v != "" {
		return v, nil
	}
	binPath := findBinary()
	if binPath == "" {
		return "", fmt.Errorf("ollama not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := host.CommandContext(ctx, binPath, "--version").CombinedOutput()
	if err != nil {
		return "unknown", nil
	}
	if v := clientVersion(string(out)); v != "" {
		return v, nil
	}
	return install.ParseVersionOutput(string(out)), nil
}

// clientVersionRe matches the line ollama prints about its own binary when a
// daemon of a different version is answering. Its absence means the two agree
// (or nothing is serving), so the general parse is right in that case.
var clientVersionRe = regexp.MustCompile(`client version is\s+(\S+)`)

func clientVersion(out string) string {
	if m := clientVersionRe.FindStringSubmatch(out); len(m) > 1 {
		return m[1]
	}
	return ""
}

// findBinary returns the ollama path. Falls back to canonical install
// dirs because a freshly installed binary is not on the Go process's PATH.
func findBinary() string {
	if path, err := exec.LookPath(binaryName()); err == nil {
		return path
	}
	// Platform-specific fallback paths
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		if lad := os.Getenv("LOCALAPPDATA"); lad != "" {
			candidates = append(candidates, filepath.Join(lad, "Programs", "Ollama", "ollama.exe"))
		}
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			candidates = append(candidates, filepath.Join(pf, "Ollama", "ollama.exe"))
		}
		if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
			candidates = append(candidates, filepath.Join(pf86, "Ollama", "ollama.exe"))
		}
	case "darwin":
		candidates = append(candidates,
			"/opt/homebrew/bin/ollama", // Apple Silicon Homebrew
			"/usr/local/bin/ollama",    // Intel Homebrew + manual installs
		)
	case "linux":
		candidates = append(candidates,
			filepath.Join(fsroot.ProviderBinDir("ollama"), "ollama"), // zzrouter-managed install
			"/usr/local/bin/ollama",
			"/usr/bin/ollama",
		)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// windowsInstallDir is where the standalone Windows zip is installed.
// It mirrors findBinary's first candidate, so detection and installation
// cannot disagree about the location, and falls back the same way
// fsroot.ProviderRootDir does when LOCALAPPDATA is unset.
func windowsInstallDir() string {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// Relative is wrong, but so is panicking in a plan builder;
			// the step's own Test-Path verify reports the failure.
			return filepath.Join("Programs", "Ollama")
		}
		localAppData = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(localAppData, "Programs", "Ollama")
}

func (i *Installer) Preflight(ctx context.Context, reqs *config.AppRequirements) *preflight.Report {
	report := &preflight.Report{Provider: i.Name, AllOK: true}
	if reqs == nil {
		reqs = &config.AppRequirements{}
	}

	report.Results = append(report.Results, preflight.DiskAndPermissions(i.Name, reqs.DiskSpace)...)

	platform := fsroot.CurrentPlatform()
	switch platform.OS {
	case "linux":
		report.Results = append(report.Results, preflight.Commands("curl", "tar", "zstd", "env", "setsid", "pkill", "pgrep")...)
	case "darwin":
		report.Results = append(report.Results, preflight.Commands("brew")...)
	case "windows":
		report.Results = append(report.Results, preflight.Commands("powershell")...)
	}

	// Ollama bundles CUDA + ROCm runtimes and falls back to CPU at
	// startup when no driver is present, so the GPU check is purely
	// informational. PolicyAdvisory surfaces the hint but never
	// blocks the install.
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

func (i *Installer) InstallPlan(ctx context.Context, version string) (*install.Plan, error) {
	if err := fsroot.ValidateVersion(version); err != nil {
		return nil, err
	}

	platform := fsroot.CurrentPlatform()
	installDir := fsroot.ProviderDir(i.Name)

	plan := &install.Plan{
		Provider:   i.Name,
		Version:    version,
		Platform:   platform,
		Action:     "install",
		InstallDir: installDir,
	}

	// Normalize on every platform, not only where a URL is built: darwin
	// compares this against Homebrew's bare version, so a request naming the
	// tag form would never match. The download URLs spell the "v" out
	// themselves, so ".../download/v0.32.14/" comes from the bare form and
	// the raw tag would yield ".../download/vv0.32.14/". Which prefix to
	// strip is declared once, in version_source.
	//
	// A provider declaring no version_source is still installable; there is
	// simply no prefix to normalize with, so the caller's string stands.
	requested := version
	if src, srcErr := install.ProviderVersionSource(i.Name); srcErr == nil {
		requested = src.Normalize(version)
	}

	// Linux + Windows both pull versioned archives from github releases so
	// we can verify against sha256sum.txt (also at github). Darwin uses
	// Homebrew and skips version pinning by design.
	var versionTag string
	if platform.OS == "linux" || platform.OS == "windows" {
		versionTag = requested
		if versionTag == "" {
			release, relErr := i.latestRelease(ctx, i.Name)
			if relErr != nil {
				return nil, fmt.Errorf("failed to resolve latest ollama version: %w", relErr)
			}
			// Revalidate: the caller's string was checked above, but this one
			// came off the network. It is interpolated into a URL that a
			// shell command then runs unquoted, and git tag names permit
			// characters a shell reads as syntax.
			if err := fsroot.ValidateVersion(release.Version); err != nil {
				return nil, fmt.Errorf("upstream ollama version %q is unusable: %w", release.Version, err)
			}
			versionTag = release.Version
		}
		plan.Version = versionTag
	}

	switch platform.OS {
	case "linux":
		v, err := i.resolveVariant(ctx, platform)
		if err != nil {
			return nil, fmt.Errorf("ollama variant selection failed: %w", err)
		}
		archiveName := variant.RenderArtifact(v, versionTag)
		downloadURL := renderURL(v.URLPattern, versionTag, archiveName)
		i.buildLinuxSteps(plan, downloadURL, archiveName, versionTag)
	case "darwin":
		i.buildDarwinSteps(plan, requested)
	case "windows":
		// Windows uses install_variants from provider config so download
		// URLs aren't hardcoded in Go (mirrors llama.cpp). The headless
		// standalone CLI zip is selected in preference to the desktop
		// installer (OllamaSetup.exe) so OLLAMA_HOST behaves normally.
		v, err := i.resolveVariant(ctx, platform)
		if err != nil {
			return nil, fmt.Errorf("ollama variant selection failed: %w", err)
		}
		archiveName := variant.RenderArtifact(v, versionTag)
		downloadURL := renderURL(v.URLPattern, versionTag, archiveName)
		i.buildWindowsSteps(plan, downloadURL, archiveName, versionTag)
	default:
		return nil, fmt.Errorf("unsupported platform: %s", platform.OS)
	}

	return plan, nil
}

// resolveVariant loads ollama's install_variants from provider config and
// returns the one matching the given platform and the local host's hardware.
// Fails fast (no hardcoded fallback) — provider config is the sole source of
// truth for artifact filenames.
func (i *Installer) resolveVariant(ctx context.Context, platform fsroot.Platform) (*config.AppInstallVariant, error) {
	cfg, err := install.LoadAppsConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load provider config: %w", err)
	}
	ext := cfg.GetExternal(i.Name)
	if ext == nil {
		return nil, fmt.Errorf("provider config has no 'ollama' entry")
	}
	if len(ext.InstallVariants) == 0 {
		return nil, fmt.Errorf("provider config 'ollama' entry has no install_variants: " +
			"archive filenames must be declared in config, not hardcoded in Go")
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hw := variant.DetectLocalHardware(probeCtx)

	return variant.Select(ext.InstallVariants, platform, hw)
}

// renderURL substitutes {version} and {artifact} in a variant URL pattern.
func renderURL(pattern, version, artifact string) string {
	out := strings.ReplaceAll(pattern, "{version}", version)
	out = strings.ReplaceAll(out, "{artifact}", artifact)
	return out
}

// buildLinuxSteps installs Ollama from the official github release
// tarball into a zzrouter-managed directory. Avoids the official
// install.sh because it requires root/sudo (writes to
// /usr/local/lib/ollama, creates an ollama user, installs a systemd
// unit) which is incompatible with a hardened systemd service running
// under NoNewPrivileges=yes.
//
// Pulls from github releases (not ollama.com/download) so the
// archive's sha256 can be verified against the upstream-published
// sha256sum.txt sibling — ollama.com does not publish a checksum file.
//
// The artifact comes from install_variants in provider config, like the
// Windows path. It used to be built in Go as "ollama-linux-<arch>.tgz",
// which stopped existing when upstream moved to .tar.zst: every install and
// upgrade then failed on a 404 at the download step.
func (i *Installer) buildLinuxSteps(plan *install.Plan, downloadURL, archiveName, versionTag string) {
	markerDir := fsroot.ProviderDir(i.Name)
	binDir := fsroot.ProviderBinDir(i.Name)
	binaryPath := filepath.Join(binDir, "ollama")
	versionFile := fsroot.ProviderVersionFile(i.Name)

	// The archive carries bin/ and lib/, and markerDir also holds the
	// version file, the managed marker and serve.log — records this
	// install must not destroy. So the payload subdirectories are staged
	// and swapped individually rather than the provider root being
	// replaced wholesale.
	stagingRoot := filepath.Join(markerDir, "incoming")
	stagedBinary := filepath.Join(stagingRoot, "bin", "ollama")
	payload := []install.StagedDir{
		{
			Live:     binDir,
			Staging:  filepath.Join(stagingRoot, "bin"),
			Previous: filepath.Join(markerDir, "bin.previous"),
		},
		{
			Live:     filepath.Join(markerDir, "lib"),
			Staging:  filepath.Join(stagingRoot, "lib"),
			Previous: filepath.Join(markerDir, "lib.previous"),
		},
	}

	qMarkerDir := fsroot.ShellQuote(markerDir)
	qBinaryPath := fsroot.ShellQuote(binaryPath)
	qStagedBinary := fsroot.ShellQuote(stagedBinary)
	qStagingRoot := fsroot.ShellQuote(stagingRoot)

	// One step performs both swaps and then removes the emptied staging
	// root, so a half-activated tree is never a state the plan can stop
	// in and later re-enter.
	activate := make([]string, 0, len(payload)+1)
	for _, d := range payload {
		activate = append(activate, d.ActivateUnixCommand())
	}
	activate = append(activate, fmt.Sprintf("rm -rf %s", qStagingRoot))

	// Sibling checksum file at the same release, derived from the artifact
	// URL rather than rebuilt, so both always name the same release.
	checksumsURL := strings.TrimSuffix(downloadURL, archiveName) + "sha256sum.txt"
	archivePath := filepath.Join(os.TempDir(), archiveName)
	qArchive := fsroot.ShellQuote(archivePath)

	plan.Steps = []install.Step{
		{
			Number:      1,
			Description: "Create managed install directory",
			Command:     fmt.Sprintf("mkdir -p %s", qMarkerDir),
			Timeout:     10 * time.Second,
			Verify:      install.StepVerify{Type: "dir_exists", Path: markerDir},
		},
		{
			Number:      2,
			Description: fmt.Sprintf("Download Ollama %s release archive", versionTag),
			// PreExec streams the download via download.Fetch so the progress
			// tracker gets byte-level updates. Command is a copy-pasteable
			// fallback for guided mode; the `[ -f ] ||` guard makes automated
			// mode a no-op because PreExec already placed the file.
			Command: fmt.Sprintf("[ -f %s ] || curl -fsSL -o %s %s", qArchive, qArchive, downloadURL),
			Timeout: 30 * time.Minute,
			Notes:   fmt.Sprintf("Downloads %s (~1.5 GB with bundled CUDA/ROCm runtimes)", downloadURL),
			Verify:  install.StepVerify{Type: "file_exists", Path: archivePath, Transient: true},
			PreExec: i.makeDownloadPreExec(downloadURL, os.TempDir()),
			PostExec: func(ctx context.Context) error {
				return download.VerifyChecksumFromURL(ctx, checksumsURL, archivePath, archiveName)
			},
		},
		{
			Number:      3,
			Description: "Prepare a clean staging directory",
			Command:     install.PrepareUnixCommand(stagingRoot),
			Timeout:     30 * time.Second,
			Verify:      install.StepVerify{Type: "dir_exists", Path: stagingRoot, Transient: true},
		},
		{
			Number:      4,
			Description: "Extract archive into the staging directory",
			// CAREFUL: do NOT delete the archive here. Step 2's verify is
			// file_exists(archivePath), and the step-by-step executor re-runs
			// VerifyAll after every step to decide whether to finalize the
			// onboarding session. Deleting the archive would break step 2's
			// re-verify on subsequent steps, leaving verify.AllOK=false and
			// skipping the FinalizeOnboarding hook — so the provider would
			// install but never become active. The archive is cleaned up by
			// the final step.
			Command: extractCommand(archiveName, qArchive, qStagingRoot),
			Timeout: 5 * time.Minute,
			Notes:   "Extracts bin/ollama + lib/ollama; activation swaps them in",
			Verify:  install.StepVerify{Type: "file_exists", Path: stagedBinary, Transient: true},
			PostExec: func(_ context.Context) error {
				return fsroot.VerifySymlinkSafe(stagedBinary)
			},
		},
		{
			Number:      5,
			Description: "Make ollama binary executable",
			Command:     fmt.Sprintf("chmod 750 %s", qStagedBinary),
			Timeout:     5 * time.Second,
			Verify: install.StepVerify{
				Type:      "command_output",
				Command:   fmt.Sprintf("test -x %s && echo ok", qStagedBinary),
				Expected:  "ok",
				Transient: true,
			},
		},
		// Stopping before the swap is what the staging buys here: the old
		// plan extracted over bin/ and lib/ while the daemon was still
		// running out of them.
		stopServerStep(6),
		{
			Number:      7,
			Description: "Activate the staged install",
			Command:     strings.Join(activate, " && "),
			Timeout:     2 * time.Minute,
			// The one durable check: every step above is scoped to a
			// directory this one consumes. See
			// install.StagedDir.ActivateUnixVerify for why it must also be
			// false while the staging root still exists.
			Verify: install.ActivateUnixVerify(stagingRoot, fmt.Sprintf("test -x %s", qBinaryPath)),
		},
		i.startServerStep(8, install.ServiceStart),
		healthCheckStep(9, versionTag),
		verifyManagedServerStep(10, binaryPath),
		recordVersionStep(11, versionFile, versionTag),
		install.WriteManagedMarkerStep(12, i.Name),
		// Cleanup is optional so a failure here never aborts a successful
		// install. The step has no verify, so VerifyAll treats it as passed.
		{
			Number:      13,
			Description: "Remove downloaded archive from /tmp",
			Command:     fmt.Sprintf("rm -f %s", qArchive),
			Timeout:     5 * time.Second,
			Optional:    true,
		},
	}
}

// buildDarwinSteps uses the Homebrew formula (not the cask) for headless
// server use. The formula supports `brew services start ollama` via launchd,
// while the cask (ollama-app) installs a GUI menu-bar app with a different
// lifecycle model.
func (i *Installer) buildDarwinSteps(plan *install.Plan, requested string) {
	markerDir := fsroot.ProviderDir(i.Name)
	qMarkerDir := fsroot.ShellQuote(markerDir)
	versionFile := fsroot.ProviderVersionFile(i.Name)

	plan.Steps = []install.Step{
		{
			Number:      1,
			Description: "Create managed marker directory",
			Command:     fmt.Sprintf("mkdir -p %s", qMarkerDir),
			Timeout:     10 * time.Second,
			Verify:      install.StepVerify{Type: "dir_exists", Path: markerDir},
		},
		{
			Number:      2,
			Description: "Install or upgrade Ollama via Homebrew",
			// upgrade first: `brew install` on an already-installed formula
			// prints "already installed" and exits 0 without doing anything,
			// so an upgrade silently changed nothing and still reported
			// success. The install fallback covers a first-time install,
			// where there is nothing to upgrade.
			Command: "brew upgrade --formula ollama || brew install ollama",
			Timeout: 10 * time.Minute,
			Notes:   "Installs Ollama CLI via Homebrew formula (headless, supports brew services)",
			Verify: install.StepVerify{
				Type:     "command_output",
				Command:  "command -v ollama",
				Expected: "ollama",
			},
		},
		i.startServerStep(3, install.ServiceRestart),
		healthCheckStep(4, ""),
		recordBrewVersionStep(5, versionFile),
	}
	// Homebrew, not the caller, picks the version. When a specific one was
	// asked for, say plainly whether it was delivered: the formula routinely
	// trails the GitHub release the version check reports, and silently
	// installing something else while reporting success is how "I pressed
	// upgrade and nothing happened" happens.
	if requested != "" {
		plan.Steps = append(plan.Steps, confirmBrewVersionStep(len(plan.Steps)+1, versionFile, requested))
	}
	plan.Steps = append(plan.Steps, install.WriteManagedMarkerStep(len(plan.Steps)+1, i.Name))
}

// confirmBrewVersionStep fails the job when Homebrew could not supply the
// requested version, naming both so the reason is actionable.
func confirmBrewVersionStep(number int, versionFile, requested string) install.Step {
	qVersionFile := fsroot.ShellQuote(versionFile)
	qRequested := fsroot.ShellQuote(requested)
	return install.Step{
		Number:      number,
		Description: "Confirm Homebrew installed " + requested,
		Command: fmt.Sprintf(
			`v=$(cat %s); if [ "$v" != %s ]; then `+
				`echo "Homebrew installed $v. The ollama formula does not carry %s yet: `+
				`on macOS the formula decides the version, and it trails the GitHub release."; exit 1; fi`,
			qVersionFile, qRequested, requested),
		Timeout: 10 * time.Second,
		Notes:   "Homebrew's formula trails upstream releases; this reports that rather than hiding it",
	}
}

// buildWindowsSteps downloads the standalone Ollama CLI zip (per
// install_variants in provider config), extracts it to %LOCALAPPDATA%\Programs\Ollama,
// and starts `ollama.exe serve` headless. Avoids OllamaSetup.exe — the
// desktop installer ships a tray-app wrapper that breaks OLLAMA_HOST
// behavior for clients (see clean.ps1 + the YAML's install_variants comment).
//
// The headless install gives the same on-disk layout (ollama.exe + lib/)
// without the GUI tray, so existing PATH-based detection and findBinary()
// continue to work.
func (i *Installer) buildWindowsSteps(plan *install.Plan, downloadURL, archiveName, versionTag string) {
	markerDir := fsroot.ProviderDir(i.Name)
	versionFile := fsroot.ProviderVersionFile(i.Name)
	tempZip := filepath.Join(os.TempDir(), archiveName)
	psTempZip := fsroot.PowerShellQuote(tempZip)
	psMarkerDir := fsroot.PowerShellQuote(markerDir)
	psDownloadURL := fsroot.PowerShellQuote(downloadURL)

	// Sibling checksum file at the same release. Upstream publishes
	// `sha256sum.txt` alongside the archives. Missing/404 degrades to a
	// warning (matches llamacpp); a real mismatch is a hard error.
	checksumsURL := strings.TrimSuffix(downloadURL, archiveName) + "sha256sum.txt"

	// Per-user install location — matches findBinary's first
	// candidate so PATH-based detection keeps working without additional
	// PATH edits. Bundled-installer history also pointed here, so users
	// upgrading from the GUI install land in the familiar place.
	//
	// Resolved in Go rather than as a PowerShell $env: expression, so the
	// location has ONE spelling. findBinary already resolves it this way,
	// and detection and installation disagreeing about where ollama lives
	// is the failure this avoids — the same environment the daemon is
	// started from decides both.
	winInstallDir := windowsInstallDir()
	ollamaExe := fsroot.PowerShellQuote(filepath.Join(winInstallDir, "ollama.exe"))

	// The zip has a flat layout, so the whole install directory is the
	// payload and can be swapped as one.
	staged := install.StagedDir{
		Live:     winInstallDir,
		Staging:  winInstallDir + ".incoming",
		Previous: winInstallDir + ".previous",
	}

	plan.Steps = []install.Step{
		{
			Number:      1,
			Description: "Create managed marker directory",
			Command:     fmt.Sprintf("powershell -NoProfile -Command \"New-Item -ItemType Directory -Force -Path %s\"", psMarkerDir),
			Timeout:     10 * time.Second,
			Verify:      install.StepVerify{Type: "dir_exists", Path: markerDir},
		},
		{
			Number:      2,
			Description: fmt.Sprintf("Download standalone Ollama CLI (%s, ~2 GB for full bundle / ~350 MB for rocm)", archiveName),
			// Size check prevents accepting a truncated download from a killed process.
			Command: fmt.Sprintf("powershell -NoProfile -Command \""+
				"if ((Test-Path %s) -and ((Get-Item %s).Length -gt 200000000)) { "+
				"Write-Output 'already-downloaded' "+
				"} else { "+
				"Invoke-WebRequest -Uri %s -OutFile %s "+
				"}\"", psTempZip, psTempZip, psDownloadURL, psTempZip),
			Timeout: 30 * time.Minute,
			Verify:  install.StepVerify{Type: "file_exists", Path: tempZip, Transient: true},
			PreExec: i.makeDownloadPreExec(downloadURL, os.TempDir()),
			PostExec: func(ctx context.Context) error {
				return download.VerifyChecksumFromURL(ctx, checksumsURL, tempZip, archiveName)
			},
		},
		{
			Number:      3,
			Description: "Prepare a clean staging directory",
			Command:     install.PrepareWindowsCommand(staged.Staging),
			Timeout:     30 * time.Second,
			Verify:      install.StepVerify{Type: "dir_exists", Path: staged.Staging, Transient: true},
		},
		{
			Number:      4,
			Description: "Extract Ollama CLI into the staging directory",
			// This used to remove the install directory and then extract
			// into it, so a failed or truncated extract left the node with
			// no ollama at all. Staging inverts that: the live directory
			// is untouched until a complete tree exists to replace it.
			Command: fmt.Sprintf("powershell -NoProfile -Command \"Expand-Archive -Path %s -DestinationPath %s -Force; Write-Output 'extracted'\"",
				psTempZip, fsroot.PowerShellQuote(staged.Staging)),
			Timeout: 10 * time.Minute,
			Notes:   "Standalone zip extracts to a flat layout: ollama.exe + lib/. No Inno Setup, no GUI.",
			Verify: install.StepVerify{
				Type:      "command_output",
				Command:   fmt.Sprintf("powershell -NoProfile -Command \"if (Test-Path %s) { Write-Output 'ok' }\"", fsroot.PowerShellQuote(filepath.Join(staged.Staging, "ollama.exe"))),
				Expected:  "ok",
				Transient: true,
			},
		},
		stopServerStep(5),
		{
			Number:      6,
			Description: "Activate the staged install",
			Command:     staged.ActivateWindowsCommand(),
			Timeout:     2 * time.Minute,
			// The one durable check; see install.StagedDir.ActivateWindowsVerify.
			Verify: staged.ActivateWindowsVerify(fmt.Sprintf("if (-not (Test-Path %s)) { exit 1 }", ollamaExe)),
		},
		{
			Number:      7,
			Description: "Clean up downloaded zip",
			Command:     fmt.Sprintf("powershell -NoProfile -Command \"Remove-Item -Path %s -Force -ErrorAction SilentlyContinue\"", psTempZip),
			Timeout:     10 * time.Second,
			Optional:    true,
		},
		i.startServerStep(8, install.ServiceStart),
		healthCheckStep(9, versionTag),
		verifyManagedServerStepWindows(10, ollamaExe),
		recordVersionStep(11, versionFile, versionTag),
		install.WriteManagedMarkerStep(12, i.Name),
	}
}

// Upgrade overrides BaseInstaller.Upgrade, which rolls back on failure.
//
// That rationale held while every platform installed to a system location
// via the official installer. It no longer does on Linux, which extracts a
// tarball into the managed provider dir, so a failure mid-extraction now
// leaves a half-replaced tree there. Restoring is not the fix on its own:
// UpgradePlan does not add install.BackupStep, so there is nothing to
// restore, and backing up first means copying ~1.5 GB on every upgrade.
// Deciding that tradeoff is open work; until then the failure is at least
// loud, because the extraction reports a nonzero status and the health check
// asserts the version actually serving.
func (i *Installer) Upgrade(ctx context.Context, toVersion string, progress func(string)) error {
	lock := fsroot.NewLockFile(i.Name)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()

	plan, err := i.UpgradePlan(ctx, toVersion)
	if err != nil {
		return err
	}
	if err := plan.Execute(ctx, progress); err != nil {
		return fmt.Errorf("ollama upgrade failed: %w", err)
	}
	return nil
}

func (i *Installer) UpgradePlan(ctx context.Context, toVersion string) (*install.Plan, error) {
	plan, err := i.InstallPlan(ctx, toVersion)
	if err != nil {
		return nil, err
	}
	plan.Action = "upgrade"
	return plan, nil
}

func (i *Installer) UninstallPlan(_ context.Context) (*install.Plan, error) {
	platform := fsroot.CurrentPlatform()
	markerDir := fsroot.ProviderDir(i.Name)

	plan := &install.Plan{
		Provider:   i.Name,
		Platform:   platform,
		Action:     "uninstall",
		InstallDir: markerDir,
	}

	switch platform.OS {
	case "linux":
		i.buildLinuxUninstallSteps(plan, markerDir)
	case "darwin":
		i.buildDarwinUninstallSteps(plan, markerDir)
	case "windows":
		i.buildWindowsUninstallSteps(plan, markerDir)
	default:
		return nil, fmt.Errorf("unsupported platform: %s", platform.OS)
	}

	return plan, nil
}

func (i *Installer) buildLinuxUninstallSteps(plan *install.Plan, markerDir string) {
	qMarkerDir := fsroot.ShellQuote(markerDir)

	plan.Steps = []install.Step{
		stopServerStep(1),
		modelCleanupStep(2),
		{
			Number:      3,
			Description: "Remove managed install directory",
			Command:     fmt.Sprintf("rm -rf %s", qMarkerDir),
			Timeout:     30 * time.Second,
			Verify: install.StepVerify{
				Type:     "command_output",
				Command:  fmt.Sprintf("test ! -d %s && echo removed", qMarkerDir),
				Expected: "removed",
			},
		},
	}
}

func (i *Installer) buildDarwinUninstallSteps(plan *install.Plan, markerDir string) {
	qMarkerDir := fsroot.ShellQuote(markerDir)

	plan.Steps = []install.Step{
		stopServerStep(1),
		{
			Number:      2,
			Description: "Uninstall Ollama via Homebrew",
			Command:     "brew uninstall ollama",
			Timeout:     2 * time.Minute,
			Verify: install.StepVerify{
				Type:     "command_output",
				Command:  "which ollama >/dev/null 2>&1 && echo found || echo removed",
				Expected: "removed",
			},
		},
		modelCleanupStep(3),
		{
			Number:      4,
			Description: "Remove managed marker",
			Command:     fmt.Sprintf("rm -rf %s", qMarkerDir),
			Timeout:     10 * time.Second,
			Verify: install.StepVerify{
				Type:     "command_output",
				Command:  fmt.Sprintf("test ! -d %s && echo removed", qMarkerDir),
				Expected: "removed",
			},
		},
	}
}

func (i *Installer) buildWindowsUninstallSteps(plan *install.Plan, markerDir string) {
	psMarkerDir := fsroot.PowerShellQuote(markerDir)

	plan.Steps = []install.Step{
		stopServerStep(1),
		{
			Number:      2,
			Description: "Uninstall Ollama via its uninstaller",
			// Inno Setup numbers uninstallers unins000, unins001, ... on reinstall;
			// only the highest-numbered one is valid. Fall back to rm -rf if missing.
			Command: "powershell -NoProfile -Command \"" +
				"$dir = Join-Path $env:LOCALAPPDATA 'Programs\\Ollama'; " +
				"if (Test-Path $dir) { " +
				"$u = Get-ChildItem $dir -Filter 'unins*.exe' -ErrorAction SilentlyContinue | " +
				"Sort-Object Name -Descending | Select-Object -First 1; " +
				"if ($u) { Start-Process $u.FullName -ArgumentList '/VERYSILENT','/SUPPRESSMSGBOXES','/NORESTART' -Wait }; " +
				"if (Test-Path (Join-Path $dir 'ollama.exe')) { Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue } " +
				"}\"",
			Timeout: 2 * time.Minute,
			Verify: install.StepVerify{
				Type:     "command_output",
				Command:  "powershell -NoProfile -Command \"if (!(Test-Path (Join-Path $env:LOCALAPPDATA 'Programs\\Ollama\\ollama.exe'))) { Write-Output 'removed' }\"",
				Expected: "removed",
			},
		},
		{
			Number:      3,
			Description: "Remove downloaded models (optional, may be large)",
			Command:     "powershell -NoProfile -Command \"$m = Join-Path $env:USERPROFILE '.ollama'; if (Test-Path $m) { $s = (Get-ChildItem $m -Recurse | Measure-Object -Property Length -Sum).Sum / 1MB; Write-Host ('Removing {0:N0} MB of model data from ' + $m) -f $s; Remove-Item $m -Recurse -Force }\"",
			Timeout:     5 * time.Minute,
			Optional:    true,
			Notes:       "Removes ~/.ollama which contains downloaded models: potentially many GB",
		},
		{
			Number:      4,
			Description: "Remove managed marker",
			Command:     fmt.Sprintf("powershell -NoProfile -Command \"Remove-Item -Path %s -Recurse -Force -ErrorAction SilentlyContinue\"", psMarkerDir),
			Timeout:     10 * time.Second,
			Verify: install.StepVerify{
				Type:     "command_output",
				Command:  fmt.Sprintf("powershell -NoProfile -Command \"if (!(Test-Path %s)) { Write-Output 'removed' }\"", psMarkerDir),
				Expected: "removed",
			},
		},
	}
}

// --- Shared steps ---

func stopServerStep(number int) install.Step {
	return install.Step{Number: number, Description: "Stop the running Ollama server", ServiceAction: install.ServiceStop, Timeout: 30 * time.Second, Notes: "Stops only the managed runtime through its lifecycle owner"}
}

func (i *Installer) startServerStep(number int, action install.ServiceAction) install.Step {
	return install.Step{Number: number, Description: "Start Ollama server", ServiceAction: action, Env: i.envMap(), Timeout: 30 * time.Second, Notes: "zzRouter supervises the managed runtime, or controls its existing OS service"}
}

// envMap is the environment shown by the API plan. The lifecycle owner
// resolves it again when the service action executes.
func (i *Installer) envMap() map[string]string {
	if i.serviceEnv == nil {
		return nil
	}
	return maps.Clone(i.serviceEnv(i.Name))
}

// qServePattern is a shell-quoted pkill/pgrep -f pattern matching the
// managed server's command line and nothing else -- in particular not the
// shell running the match. Bracketing the first character of the file name
// is what buys that: the pattern as it appears on the shell's own command
// line no longer matches the regex it spells.
func qServePattern(binaryPath string) string {
	dir, file := path.Split(binaryPath)
	return fsroot.ShellQuote(fmt.Sprintf("%s[%c]%s serve", dir, file[0], file[1:]))
}

// verifyManagedServerStep proves the managed binary is the process serving.
// It is the other half of the assertion healthCheckStep starts, and neither
// half is sufficient alone: the health check says something on 11434 reports
// the expected version, this says the install we just made is running.
//
// A daemon started outside zzRouter answers /api/version with whatever
// version it is -- including the one being installed. That is not
// hypothetical: a foreign Ollama of the same version held 11434 while the
// managed binary lost the bind race and exited, so the upgrade reported
// every step green and every model load afterwards failed against a server
// that could no longer find its own runner. The tell was 17 ms between
// "Start Ollama server" and "Verify Ollama 0.32.15 is serving".
//
// pgrep rather than reading the port's owner: identifying who holds a
// socket owned by another user needs privileges zzRouter does not have,
// while "did the process we started survive" is the actual question and
// needs none. It runs after the health check so a binary that is going to
// exit on "address in use" has had that time to do it.
func verifyManagedServerStep(number int, binaryPath string) install.Step {
	pattern := qServePattern(binaryPath)
	return install.Step{
		Number:      number,
		Description: "Verify the managed Ollama is the one serving",
		Command: fmt.Sprintf("pgrep -f %s >/dev/null || { echo %s; exit 1; }",
			pattern, fsroot.ShellQuote(foreignServerMessage)),
		Timeout: 10 * time.Second,
		Notes:   "Catches another Ollama holding port 11434, which the version check cannot",
		Verify: install.StepVerify{
			Type:     "command_output",
			Command:  fmt.Sprintf("pgrep -f %s >/dev/null && echo managed", pattern),
			Expected: "managed",
		},
	}
}

// verifyManagedServerStepWindows is verifyManagedServerStep for PowerShell.
// It filters on Path exactly as the stop step there does, and needs no
// self-exclusion because Get-Process matches process names rather than
// whole command lines.
//
// A plain pipe, not `^|`. These commands run as `cmd /S /C "<command>"`,
// which strips the outer quote pair and passes the rest through
// verbatim, so the caret is not an escape here: it reaches PowerShell as
// an argument and breaks the pipeline. Worse, it broke it quietly, since
// the exit code came from the statement after the semicolon. Measured on
// a Windows 11 worker through that exact wrapper: `|` returns the PID,
// `^|` fails with a positional-parameter error.
func verifyManagedServerStepWindows(number int, ollamaExe string) install.Step {
	running := fmt.Sprintf("Get-Process ollama -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq %s }", ollamaExe)
	return install.Step{
		Number:      number,
		Description: "Verify the managed Ollama is the one serving",
		Command: fmt.Sprintf("powershell -NoProfile -Command \"if (%s) { Write-Output 'managed' } else { Write-Output '%s'; exit 1 }\"",
			running, foreignServerMessage),
		Timeout: 10 * time.Second,
		Notes:   "Catches another Ollama holding port 11434, which the version check cannot",
		Verify: install.StepVerify{
			Type:     "command_output",
			Command:  fmt.Sprintf("powershell -NoProfile -Command \"if (%s) { Write-Output 'managed' }\"", running),
			Expected: "managed",
		},
	}
}

// foreignServerMessage names the one cause, because every other reason the
// managed process could be missing was already caught by an earlier step.
const foreignServerMessage = "port 11434 is served by an Ollama that is not the managed install"

// healthProbeCommands returns the poll and verify commands for goos.
//
// The literal 127.0.0.1, never "localhost". Ollama binds IPv4 only, while
// "localhost" resolves to ::1 first on Windows, so every probe spends its
// whole timeout on an address nothing is listening on and the step fails
// against a daemon that is serving perfectly. Measured on a Windows 11
// worker: localhost timed out at 1250 ms, 127.0.0.1 answered in 23 ms.
//
// The Windows failure branch reports the last error. A bare `exit 1`
// surfaced as "exit status 1: " with nothing after the colon, which says
// only that the step ran. Single-quoted inside PowerShell so the string
// survives the cmd /S /C wrapper without another layer of escaping.
//
// Split out from healthCheckStep so the platform branch can be asserted
// from any platform; it is unreachable at runtime on the other one.
func healthProbeCommands(goos string) (poll, verify string) {
	if goos == "windows" {
		return "powershell -NoProfile -Command \"$e=$null; 1..20 | ForEach-Object { try { (Invoke-WebRequest 'http://127.0.0.1:11434/api/version' -UseBasicParsing -TimeoutSec 2).Content; exit 0 } catch { $e=$_.Exception.Message; Start-Sleep 1 } }; [Console]::Error.WriteLine('no answer on 127.0.0.1:11434 after 20 tries: ' + $e); exit 1\"",
			"powershell -NoProfile -Command \"(Invoke-WebRequest 'http://127.0.0.1:11434/api/version' -UseBasicParsing -TimeoutSec 3).Content\""
	}
	// curl --retry absorbs the daemon's cold-start window: `brew services
	// start` / `systemctl start` return before the socket is listening, so a
	// single curl hits a connection-refused race. Verify is a single fast
	// probe, since plan.VerifyAll() runs it during the Install-screen
	// pre-probe and a slow one would stall the UI load.
	return "curl -sf --retry 20 --retry-delay 1 --retry-connrefused --retry-all-errors --connect-timeout 2 http://127.0.0.1:11434/api/version",
		"curl -sf --connect-timeout 2 --max-time 5 http://127.0.0.1:11434/api/version"
}

// healthCheckStep polls the Ollama API until it answers.
//
// expectVersion, when known, is what /api/version must report. Asserting the
// literal version is what makes an upgrade provable: a stale daemon still
// listening on 11434 answers the generic "is something there" probe happily,
// so without this the upgrade completes green while the old build keeps
// serving. Empty means "any version" — the Homebrew path does not pin one.
func healthCheckStep(number int, expectVersion string) install.Step {
	pollCmd, verifyCmd := healthProbeCommands(runtime.GOOS)
	expected := "version"
	description := "Verify Ollama is responding"
	if expectVersion != "" {
		expected = expectVersion
		description = "Verify Ollama " + expectVersion + " is serving"
	}
	return install.Step{
		Number:      number,
		Description: description,
		Command:     pollCmd,
		Timeout:     30 * time.Second,
		// Required, unlike the start step above it. Execute only runs a
		// step's Verify when the step is not Optional (plan.go), and this
		// step's Verify IS the proof — that something answers on 11434 and,
		// where the version is known, that it is the one just installed.
		// Left optional, the assertion never ran and a stale daemon kept the
		// job green, which is the failure this step exists to catch.
		Notes: "Polls the Ollama API health endpoint for up to 20s",
		Verify: install.StepVerify{
			Type:     "command_output",
			Command:  verifyCmd,
			Expected: expected,
		},
	}
}

// extractCommand returns the untar for an artifact, matching its compression.
//
// tar drives zstd itself rather than being fed by a pipe. The shell runs
// steps as `sh -c` with no pipefail, so a pipeline reports only the exit
// status of tar: a truncated archive lets zstd emit valid data and then fail,
// tar extracts the partial content and exits 0, and a half-replaced install
// reports success. Measured on the worker (GNU tar 1.34): piped truncated
// archive exits 0, --use-compress-program exits 2.
//
// --use-compress-program rather than --zstd because the latter needs GNU tar
// 1.31+; this one has been available far longer and costs nothing. Both are
// GNU spellings, which is fine — this path is linux-only, and preflight
// already requires the zstd binary there.
func extractCommand(archiveName, qArchive, qDestDir string) string {
	if strings.HasSuffix(archiveName, ".zst") {
		return fmt.Sprintf("tar --use-compress-program=zstd -xf %s -C %s", qArchive, qDestDir)
	}
	return fmt.Sprintf("tar -xzf %s -C %s", qArchive, qDestDir)
}

// recordVersionStep writes the version this install placed to versionFile.
//
// It writes the release the plan resolved rather than asking the binary,
// because `ollama --version` answers about the *running daemon*:
//
//	ollama version is 0.21.2            <- the daemon that is already serving
//	Warning: client version is 0.32.14  <- the binary we just installed
//
// Taking the first line therefore records the version the upgrade was meant
// to replace, and the node reads as up to date forever after.
func recordVersionStep(number int, versionFile, version string) install.Step {
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = fmt.Sprintf("powershell -NoProfile -Command \"Set-Content -Path %s -Value %s\"",
			fsroot.PowerShellQuote(versionFile), fsroot.PowerShellQuote(version))
	} else {
		cmd = fmt.Sprintf("printf '%%s\\n' %s > %s",
			fsroot.ShellQuote(version), fsroot.ShellQuote(versionFile))
	}
	return install.Step{
		Number:      number,
		Description: "Record installed version",
		Command:     cmd,
		Timeout:     10 * time.Second,
		// The version is a literal this plan already resolved, so the check
		// can be that the file holds it. recordBrewVersionStep below cannot
		// do the same — brew picks the version, so there is nothing to
		// compare against until the step has run.
		Verify: install.StepVerify{Type: "file_equals", Path: versionFile, Expected: version, PlanScoped: true},
	}
}

// recordBrewVersionStep records what Homebrew actually installed.
//
// The macOS path deliberately does not pin a version, so there is nothing to
// write ahead of time. `brew list --versions` names the installed formula
// ("ollama 0.32.14") and, unlike the CLI's own banner, cannot report a
// running daemon instead.
func recordBrewVersionStep(number int, versionFile string) install.Step {
	qVersionFile := fsroot.ShellQuote(versionFile)
	return install.Step{
		Number:      number,
		Description: "Record installed version",
		// Assign before redirecting. A pipeline reports only the last
		// command's status, so `brew list | awk > file` records an empty
		// version on a brew failure and still exits 0 — the same masking
		// just removed from the extract step.
		Command: fmt.Sprintf("v=$(brew list --versions ollama) && "+
			"printf '%%s\n' \"${v##* }\" > %s", qVersionFile),
		Timeout: 30 * time.Second,
		Verify:  install.StepVerify{Type: "file_exists", Path: versionFile},
	}
}

// modelCleanupStep returns an optional step that removes ~/.ollama
// (downloaded models). This is marked optional because the data may be large
// and the user might want to keep it.
func modelCleanupStep(number int) install.Step {
	return install.Step{
		Number:      number,
		Description: "Remove downloaded models (optional, may be large)",
		Command:     "echo \"Model data in ~/.ollama will not be removed automatically.\"; echo \"To remove: rm -rf ~/.ollama\"; ls -lhd ~/.ollama 2>/dev/null || true",
		Timeout:     10 * time.Second,
		Optional:    true,
		Notes:       "~/.ollama contains downloaded models: potentially many GB. Remove manually if desired.",
	}
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "ollama.exe"
	}
	return "ollama"
}

// makeDownloadPreExec snapshots i.Progress at plan-build time to avoid racing
// with the deferred SetProgress(nil) cleanup in InstallCoordinator.Provider.
func (i *Installer) makeDownloadPreExec(url, destDir string) func(ctx context.Context) error {
	p := i.Progress
	var progressFn func(downloaded, total int64, pct int)
	if p != nil {
		progressFn = p.SetDownloadProgress
	}
	provider := i.Name
	return func(ctx context.Context) error {
		_, err := download.FetchForProvider(ctx, provider, url, destDir, progressFn)
		return err
	}
}
