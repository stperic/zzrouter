// Package llamacpp installs llama.cpp's pre-built binaries from GitHub
// Releases. Artifact filenames come from provider config install_variants
// (GPU/platform-aware selection); there is no hardcoded fallback.
package llamacpp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/download"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/variant"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
	"github.com/stperic/zzrouter/pkg/security"
)

// Installer manages llama.cpp installation.
// Downloads pre-built binaries from GitHub Releases for all platforms.
// ProgressReporter is satisfied through the embedded BaseInstaller —
// read i.Progress inside InstallPlan for the coord-wired handle.
type Installer struct {
	install.BaseInstaller
	detectHardware func(context.Context) *hardware.HardwareInfo
}

// New creates a llama.cpp installer.
func New() *Installer {
	inst := &Installer{}
	// Name must match the provider config key: the spawn-time resolver
	// looks the binary up under ProviderBinDir(configKey), so an
	// installer writing anywhere else is invisible to it.
	inst.BaseInstaller = install.NewBaseInstaller("llamacpp", inst)
	return inst
}

// makeDownloadPreExec snapshots i.Progress at plan-build time so the
// actual fetch (invoked inside the step's PreExec) routes byte-level
// progress onto the jobs stream. Shape mirrors
// ollama.Installer.makeDownloadPreExec.
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

func (i *Installer) SupportedPlatforms() []fsroot.Platform {
	return []fsroot.Platform{
		{OS: "darwin", Arch: "arm64"},
		{OS: "darwin", Arch: "amd64"},
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
		{OS: "windows", Arch: "amd64"},
		{OS: "windows", Arch: "arm64"},
	}
}

// IsInstalled gates on the version file (written as a late install step).
// Using the binary as the marker would let an aborted extraction look
// installed. PATH-based detection of non-managed binaries lives in the
// detect package and is deliberately excluded here.
func (i *Installer) IsInstalled() bool {
	return fsroot.ReadInstalledVersion(i.Name) != ""
}

// InstalledVersion reports the release the install recorded, falling back to
// the binary's own banner only for a llama-server we did not place.
//
// The file is preferred because it holds the release tag ("b10502") that the
// download URLs and the upstream comparison are both keyed on, while the
// banner prints a bare build number in a different shape entirely. Reading
// the same source IsInstalled gates on also keeps one answer per install.
func (i *Installer) InstalledVersion() (string, error) {
	if v := fsroot.ReadInstalledVersion(i.Name); v != "" {
		return v, nil
	}

	binPath := filepath.Join(fsroot.ProviderBinDir(i.Name), serverBinary())
	if _, err := os.Stat(binPath); err != nil {
		// Try system PATH
		var lookErr error
		binPath, lookErr = exec.LookPath(serverBinary())
		if lookErr != nil {
			return "", fmt.Errorf("llama-server not found")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := host.CommandContext(ctx, binPath, "--version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown", nil
	}
	return install.ParseVersionOutput(string(output)), nil
}

// Preflight checks for required download tools (curl, tar on Unix; PowerShell on Windows).
func (i *Installer) Preflight(ctx context.Context, reqs *config.AppRequirements) *preflight.Report {
	report := &preflight.Report{Provider: i.Name, AllOK: true}
	if reqs == nil {
		reqs = &config.AppRequirements{}
	}

	// Disk space + write permissions
	report.Results = append(report.Results, preflight.DiskAndPermissions(i.Name, reqs.DiskSpace)...)

	// Download tools
	if fsroot.CurrentPlatform().OS == "windows" {
		report.Results = append(report.Results, preflight.Commands("powershell")...)
		// llama.cpp's pre-built Windows binaries are linked against
		// MSVC 14.30+ runtime DLLs. Bare Windows images (Vultr, Server
		// 2022/2025) ship MSVCP140 14.22 from the 2019 redist — too
		// old. The binary loads but crashes immediately at startup
		// with NTSTATUS 0xC0000005 ACCESS_VIOLATION, a 250 MB download
		// followed by an opaque verify-step failure. Catch this at
		// preflight so the user sees an actionable hint and never
		// burns the bandwidth.
		report.Results = append(report.Results, preflight.MSVCRedist())
	} else {
		report.Results = append(report.Results, preflight.Commands("curl", "tar")...)
	}

	// llama.cpp ships CPU + CUDA + ROCm variants and the variant
	// selector picks the right build at install time. The GPU check
	// is advisory so the CPU variant still succeeds on nodes
	// without a driver — a hard gate would break every CPU-only
	// install. When the user explicitly declares the CUDA variant
	// in provider config they can raise the policy to required.
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
	layout := binLayout{
		StagedDir: install.StagedDir{
			Live:     fsroot.ProviderBinDir(i.Name),
			Staging:  fsroot.ProviderBinStagingDir(i.Name),
			Previous: fsroot.ProviderBinPreviousDir(i.Name),
		},
		binary: serverBinary(),
	}

	// The release feed and the download host both come from the provider's
	// version_source, so no upstream URL is spelled out in Go.
	src, err := install.ProviderVersionSource(i.Name)
	if err != nil {
		return nil, err
	}
	// The artifact URLs below are GitHub release URLs built from src.Repo,
	// so a source of any other type would render a malformed host.
	if src.Type != config.VersionSourceGitHubRelease {
		return nil, fmt.Errorf("llama.cpp version_source must be type %q, got %q",
			config.VersionSourceGitHubRelease, src.Type)
	}

	// Coerce rather than trust: the versions endpoint reports "10502" as
	// latest and "b10502" as latest_tag, and the release URLs below only
	// accept the tag. Callers that read the other field would 404.
	versionTag := src.Tag(version)
	if versionTag == "" {
		// The raw tag, not the normalized version: llama.cpp's release URLs
		// and its version file are both keyed on "b10502".
		release, resolveErr := upstream.Latest(ctx, src)
		if resolveErr != nil {
			return nil, fmt.Errorf("failed to resolve latest llama.cpp version: %w", resolveErr)
		}
		versionTag = release.Tag
	}

	plan := &install.Plan{
		Provider:   i.Name,
		Version:    versionTag,
		Platform:   platform,
		Action:     "install",
		InstallDir: installDir,
	}

	// Select the archive by resolving install_variants from provider config.
	// The variants list is the single source of truth for asset filenames —
	// no hardcoded fallback in Go (enforces config-driven provider data).
	v, err := i.resolveVariant(ctx, platform)
	if err != nil {
		return nil, fmt.Errorf("llama.cpp variant selection failed: %w", err)
	}
	rendered := variant.RenderArtifacts(v, versionTag)
	if len(rendered) == 0 {
		return nil, fmt.Errorf("llama.cpp variant %q has no artifacts declared", v.ID)
	}
	artifacts := make([]artifactRef, 0, len(rendered))
	for _, r := range rendered {
		artifacts = append(artifacts, artifactRef{
			Filename: r.Filename,
			URL: fmt.Sprintf(
				"https://github.com/%s/releases/download/%s/%s",
				src.Repo, versionTag, r.Filename,
			),
		})
	}
	names := make([]string, len(artifacts))
	for i, a := range artifacts {
		names[i] = a.Filename
	}
	slog.Info("llama.cpp variant selected",
		"variant", v.ID,
		"artifacts", names,
		"platform", fmt.Sprintf("%s/%s", platform.OS, platform.Arch),
	)

	if platform.OS == "windows" {
		i.buildWindowsSteps(plan, src, layout, artifacts)
	} else {
		i.buildUnixSteps(plan, src, layout, artifacts)
	}

	return plan, nil
}

// binLayout is llama.cpp's payload: a single bin/ directory, plus the
// name of the server binary inside it. The staging discipline itself
// lives in install.StagedDir — see it for why extraction never writes
// into the live tree.
type binLayout struct {
	install.StagedDir
	binary string // server binary name, e.g. llama-server(.exe)
}

func (l binLayout) stagedBinary() string { return filepath.Join(l.Staging, l.binary) }
func (l binLayout) liveBinary() string   { return filepath.Join(l.Live, l.binary) }

// artifactRef is one rendered (filename, url) pair the installer will
// download + extract. Multi-artifact variants emit one ref per peer asset
// (e.g. llama-bin-*.zip + cudart-llama-bin-*.zip).
type artifactRef struct {
	Filename string
	URL      string
}

// buildUnixSteps adds install steps for macOS and Linux. Emits a
// download → checksum → extract triplet per artifact into the staging
// tree, then chmod + verify + activate + record-version. Multi-artifact
// variants unpack into the same staging directory; archive contents
// must not collide.
//
// Everything before activation is scoped to staging and is marked
// Transient, because the staged tree is consumed by the swap: VerifyAll
// asks "is this install intact?", and a staging directory that is gone
// is the successful case, not damage. The durable integrity check is
// the activate step's, which runs the binary that is actually live.
func (i *Installer) buildUnixSteps(plan *install.Plan, src *config.VersionSource, layout binLayout, artifacts []artifactRef) {
	qStageDir := fsroot.ShellQuote(layout.Staging)
	qStagedBinary := fsroot.ShellQuote(layout.stagedBinary())
	qLiveBinary := fsroot.ShellQuote(layout.liveBinary())

	steps := []install.Step{
		{
			Number:      1,
			Description: "Prepare a clean staging directory",
			// Clearing staging is safe in a way that clearing the live
			// directory is not: nothing resolves against it.
			Command: install.PrepareUnixCommand(layout.Staging),
			Timeout: 30 * time.Second,
			Verify:  install.StepVerify{Type: "dir_exists", Path: layout.Staging, Transient: true},
		},
	}
	step := 2
	for idx, a := range artifacts {
		// Per-artifact temp file. Index suffix avoids collision when two
		// artifacts of one variant share an extension.
		archivePath := filepath.Join(os.TempDir(), fmt.Sprintf("zzrouter-llamacpp-download-%d-%s", idx, a.Filename))
		qArchive := fsroot.ShellQuote(archivePath)
		url := a.URL
		fname := a.Filename

		steps = append(steps,
			install.Step{
				Number:      step,
				Description: fmt.Sprintf("Download %s", a.Filename),
				Command:     fmt.Sprintf("[ -f %s ] || curl -fsSL -o %s %s", qArchive, qArchive, url),
				Timeout:     30 * time.Minute,
				Verify:      install.StepVerify{Type: "file_exists", Path: archivePath, Transient: true},
				PreExec:     i.makeDownloadPreExec(url, os.TempDir()),
			},
			install.Step{
				Number:      step + 1,
				Description: fmt.Sprintf("Verify checksum of %s", a.Filename),
				// Prints the archive's own hash. The comparison happens in
				// PostExec against the digest GitHub recorded for this asset,
				// so a hand-run of this command shows the operator the value
				// to check rather than pretending to check it.
				Command: fmt.Sprintf("shasum -a 256 %s 2>/dev/null || sha256sum %s", qArchive, qArchive),
				// 30s was the budget for fetching a checksum file. This step
				// now hashes the archive twice (once for the operator in the
				// command, once in PostExec for the comparison) around a
				// GitHub round trip that has its own 30s ceiling, and
				// executeStep wraps all of it in this one timeout. A large
				// archive on a cold cache would cancel a good download, and
				// the step is no longer Optional. Matches the Windows budget.
				Timeout: 5 * time.Minute,
				Notes:   "Compares the download against the SHA-256 GitHub published for this release asset",
				// Deliberately no Verify. The check this step performs leaves no
				// artifact behind, and the obvious stand-in — file_exists on the
				// archive — is satisfied by exactly the corrupt download it exists
				// to reject. Worse, runInstallStep skips any step whose Verify
				// already passes, so a present-but-bad archive made the
				// single-step path report "done" without ever hashing it. The
				// download step already attests that the archive landed.
				PostExec: i.makeDigestVerifyPostExec(src, plan.Version, archivePath, fname),
			},
			install.Step{
				Number:      step + 2,
				Description: fmt.Sprintf("Extract %s", a.Filename),
				Command:     fmt.Sprintf("tar xzf %s --strip-components=1 -C %s && rm -f %s", qArchive, qStageDir, qArchive),
				Timeout:     5 * time.Minute,
				// dir_exists not file_exists: the binary is only in one of
				// the peer artifacts. The post-extract sanity is just that
				// staging wasn't blown away. The "verify llama-server" step
				// further down attests the actual binary survived.
				Verify: install.StepVerify{Type: "dir_exists", Path: layout.Staging, Transient: true},
			},
		)
		step += 3
	}

	steps = append(steps,
		install.Step{
			Number:      step,
			Description: "Make binary executable",
			Command:     fmt.Sprintf("chmod 750 %s", qStagedBinary),
			Timeout:     10 * time.Second,
			Verify: install.StepVerify{
				Type:      "command_output",
				Command:   fmt.Sprintf("test -x %s && echo ok", qStagedBinary),
				Expected:  "ok",
				Transient: true,
			},
			PostExec: func(_ context.Context) error {
				return fsroot.VerifySymlinkSafe(layout.stagedBinary())
			},
		},
		install.Step{
			Number:      step + 1,
			Description: "Verify the staged llama-server works",
			// This is the gate that makes staging worth doing: a tree
			// that cannot run never gets to replace one that can.
			Command: fmt.Sprintf("%s --version", qStagedBinary),
			Timeout: 30 * time.Second,
			Verify: install.StepVerify{
				Type:      "command_output",
				Command:   fmt.Sprintf("%s --version", qStagedBinary),
				Expected:  "version",
				Transient: true,
			},
			PostExec: func(_ context.Context) error {
				hash, err := security.ComputeSHA256(layout.stagedBinary())
				if err == nil {
					slog.Info("Provider binary SHA256", "provider", "llama.cpp", "path", layout.stagedBinary(), "sha256", hash)
				}
				return nil
			},
		},
		install.Step{
			Number:      step + 2,
			Description: "Activate the staged install",
			Command:     layout.ActivateUnixCommand(),
			Timeout:     2 * time.Minute,
			// Durable, and false while staging exists; both properties
			// are explained on install.StagedDir.ActivateUnixVerify.
			Verify: layout.ActivateUnixVerify(fmt.Sprintf("%s --version 2>&1 | grep -qi version", qLiveBinary)),
		},
		// Record-version MUST be the terminal step — IsInstalled gates on the
		// version file, so writing it earlier would let a partial install look
		// complete if a later required step fails.
		install.Step{
			Number:      step + 3,
			Description: "Record installed version",
			Command:     fsroot.WriteVersionCommand(plan.Version, fsroot.ProviderVersionFile(i.Name)),
			Timeout:     5 * time.Second,
			Verify: install.StepVerify{
				Type:       "file_equals",
				Path:       fsroot.ProviderVersionFile(i.Name),
				Expected:   plan.Version,
				PlanScoped: true,
			},
		},
	)
	plan.Steps = steps
}

// buildWindowsSteps adds install steps for Windows using PowerShell.
// Emits a download → extract → cleanup triplet per artifact into the
// staging tree, then verify + activate + record-version. Both peer
// artifacts (binary zip + cudart runtime zip) extract into the same
// staging directory; the flatten step pulls files out of any nested
// top-level dir, so flat-root zips like cudart-* are no-ops.
//
// Staging rationale and the Transient marking are the same as
// buildUnixSteps; see it.
func (i *Installer) buildWindowsSteps(plan *install.Plan, src *config.VersionSource, layout binLayout, artifacts []artifactRef) {
	qStagedBinary := fsroot.ShellQuote(layout.stagedBinary())
	qLiveBinary := fsroot.ShellQuote(layout.liveBinary())
	psStageDir := fsroot.PowerShellQuote(layout.Staging)

	steps := []install.Step{
		{
			Number:      1,
			Description: "Prepare a clean staging directory",
			// Removing first, rather than relying on New-Item -Force,
			// because -Force leaves existing contents in place — which
			// is the accumulation this staging exists to stop.
			Command: install.PrepareWindowsCommand(layout.Staging),
			Timeout: 30 * time.Second,
			Verify:  install.StepVerify{Type: "dir_exists", Path: layout.Staging, Transient: true},
		},
	}
	step := 2
	for _, a := range artifacts {
		tempZip := filepath.Join(os.TempDir(), a.Filename)
		qTempZip := fsroot.ShellQuote(tempZip)
		psTempZip := fsroot.PowerShellQuote(tempZip)
		url := a.URL

		steps = append(steps,
			install.Step{
				Number:      step,
				Description: fmt.Sprintf("Download %s", a.Filename),
				Command: fmt.Sprintf("powershell -NoProfile -Command \"if (-not (Test-Path %s)) { Invoke-WebRequest -Uri %s -OutFile %s }\"",
					psTempZip, fsroot.PowerShellQuote(url), psTempZip),
				Timeout: 30 * time.Minute,
				Verify:  install.StepVerify{Type: "file_exists", Path: tempZip, Transient: true},
				PreExec: i.makeDownloadPreExec(url, os.TempDir()),
			},
			install.Step{
				Number:      step + 1,
				Description: fmt.Sprintf("Verify checksum of %s", a.Filename),
				// Prints the archive's own hash; PostExec does the comparison.
				// Windows had no integrity step at all, so its downloads were
				// unverified outright rather than verified by a step that
				// silently no-opped, as on Unix.
				Command: fmt.Sprintf("powershell -NoProfile -Command \"(Get-FileHash -Algorithm SHA256 %s).Hash\"", psTempZip),
				Timeout: 5 * time.Minute,
				Notes:   "Compares the download against the SHA-256 GitHub published for this release asset",
				// No Verify, for the reason spelled out on the unix counterpart.
				PostExec: i.makeDigestVerifyPostExec(src, plan.Version, tempZip, a.Filename),
			},
			install.Step{
				Number:      step + 2,
				Description: fmt.Sprintf("Extract %s", a.Filename),
				Command:     fmt.Sprintf("powershell -NoProfile -Command \"Expand-Archive -Path %s -DestinationPath %s -Force; Get-ChildItem %s -Directory | ForEach-Object { Move-Item $_.FullName\\* %s -Force }\"", psTempZip, psStageDir, psStageDir, psStageDir),
				Timeout:     5 * time.Minute,
				Notes:       "Extracts and flattens (no-op flatten when the zip has no top-level directory, e.g. cudart-*)",
				// See unix counterpart: per-extract verify is dir_exists
				// because the binary is only in one of the peer artifacts.
				Verify: install.StepVerify{Type: "dir_exists", Path: layout.Staging, Transient: true},
			},
			install.Step{
				Number:      step + 3,
				Description: fmt.Sprintf("Clean up %s", a.Filename),
				Command:     fmt.Sprintf("del %s", qTempZip),
				Timeout:     10 * time.Second,
				Verify: install.StepVerify{
					Type:     "command_output",
					Command:  fmt.Sprintf("powershell -NoProfile -Command \"if (!(Test-Path %s)) { Write-Output 'ok' }\"", psTempZip),
					Expected: "ok",
				},
			},
		)
		step += 4
	}

	steps = append(steps,
		install.Step{
			Number:      step,
			Description: "Verify the staged llama-server works",
			// The gate: a tree that cannot run never replaces one that can.
			Command: fmt.Sprintf("%s --version", qStagedBinary),
			Verify: install.StepVerify{
				Type:      "command_output",
				Command:   fmt.Sprintf("%s --version", qStagedBinary),
				Expected:  "version",
				Transient: true,
			},
			PostExec: func(_ context.Context) error {
				return fsroot.VerifySymlinkSafe(layout.stagedBinary())
			},
		},
		install.Step{
			Number:      step + 1,
			Description: "Activate the staged install",
			Command:     layout.ActivateWindowsCommand(),
			Timeout:     2 * time.Minute,
			// Durable, and false while staging exists; see
			// install.StagedDir.ActivateWindowsVerify.
			Verify: layout.ActivateWindowsVerify(fmt.Sprintf("$v = & %s --version 2>&1 | Out-String; if ($v -notmatch 'version') { exit 1 }", qLiveBinary)),
		},
		// Record-version MUST be the terminal step — see buildUnixSteps for rationale.
		install.Step{
			Number:      step + 2,
			Description: "Record installed version",
			Command:     fsroot.WriteVersionCommand(plan.Version, fsroot.ProviderVersionFile(i.Name)),
			Timeout:     5 * time.Second,
			Verify: install.StepVerify{
				Type:       "file_equals",
				Path:       fsroot.ProviderVersionFile(i.Name),
				Expected:   plan.Version,
				PlanScoped: true,
			},
		},
	)
	plan.Steps = steps
}

func (i *Installer) UpgradePlan(ctx context.Context, toVersion string) (*install.Plan, error) {
	plan, err := i.InstallPlan(ctx, toVersion)
	if err != nil {
		return nil, err
	}
	return install.BuildUpgradePlan(i.Name, plan, false), nil
}

func (i *Installer) UninstallPlan(_ context.Context) (*install.Plan, error) {
	installDir := fsroot.ProviderDir(i.Name)
	qDir := fsroot.ShellQuote(installDir)

	var removeStep install.Step
	if fsroot.CurrentPlatform().OS == "windows" {
		removeStep = install.Step{
			Number:      1,
			Description: "Remove llama.cpp installation",
			Command:     fmt.Sprintf("powershell -Command \"Remove-Item -Path '%s' -Recurse -Force\"", installDir),
			Verify: install.StepVerify{
				Type:     "command_output",
				Command:  fmt.Sprintf("powershell -Command \"if (!(Test-Path '%s')) { Write-Output 'removed' }\"", installDir),
				Expected: "removed",
			},
		}
	} else {
		removeStep = install.Step{
			Number:      1,
			Description: "Remove llama.cpp installation",
			Command:     fmt.Sprintf("rm -rf %s", qDir),
			Verify: install.StepVerify{
				Type:     "command_output",
				Command:  fmt.Sprintf("test ! -d %s && echo removed", qDir),
				Expected: "removed",
			},
		}
	}

	return &install.Plan{
		Provider:   i.Name,
		Platform:   fsroot.CurrentPlatform(),
		Action:     "uninstall",
		InstallDir: installDir,
		Steps:      []install.Step{removeStep},
	}, nil
}

// serverBinary returns the llama-server binary name for the current OS.
func serverBinary() string {
	if runtime.GOOS == "windows" {
		return "llama-server.exe"
	}
	return "llama-server"
}

// makeDigestVerifyPostExec returns the hook that checks a downloaded artifact
// against the SHA-256 GitHub recorded for that release asset.
//
// llama.cpp publishes no checksum file, so the SHA256SUMS URL this step used
// to fetch had never resolved — it 404'd on every release, and the step being
// Optional meant the failure was logged and swallowed. Every llama.cpp binary
// this installer has ever placed was therefore unverified, while install/verify
// reported the step green. GitHub's own per-asset digest is the checksum
// llama.cpp does publish, by way of the platform rather than the project.
//
// An unreachable or rate-limited API degrades to a warning: refusing to install
// because a digest could not be fetched would trade a real capability for a
// check that was absent a moment ago. A digest that IS fetched and does NOT
// match is a hard error, which is why the step is no longer Optional — an
// Optional step swallows this hook's error too.
func (i *Installer) makeDigestVerifyPostExec(src *config.VersionSource, tag, archivePath, archiveName string) func(context.Context) error {
	return func(ctx context.Context) error {
		digest, err := upstream.AssetDigest(ctx, src, tag, archiveName)
		if err != nil {
			slog.Warn("release asset digest unavailable, skipping integrity check",
				"provider", i.Name, "archive", archiveName, "tag", tag, "error", err)
			return nil
		}
		if digest == "" {
			slog.Warn("release publishes no SHA-256 for this asset, skipping integrity check",
				"provider", i.Name, "archive", archiveName, "tag", tag)
			return nil
		}
		if err := security.VerifySHA256(archivePath, digest); err != nil {
			return fmt.Errorf("archive integrity check FAILED for %s: %w", archiveName, err)
		}
		slog.Info("archive digest verified",
			"provider", i.Name, "archive", archiveName, "sha256", digest)
		return nil
	}
}

// resolveVariant loads llama.cpp's install_variants from provider config and
// returns the one matching the given platform and the local host's hardware.
// Fails fast (no hardcoded fallback) if the config is missing or nothing
// matches — enforcing the rule that provider config is the sole source of
// truth for artifact filenames.
//
// NOTE: `platform` and hardware detection use the LOCAL host. That is correct
// because llama.cpp installs execute on the target worker (via the internal
// /providers/install executor), so CurrentPlatform IS the target platform at
// the point this function runs. For remote installs initiated from a
// coordinator of a different OS, the coordinator forwards the request and
// this function runs on the target worker.
func (i *Installer) resolveVariant(ctx context.Context, platform fsroot.Platform) (*config.AppInstallVariant, error) {
	cfg, err := install.LoadAppsConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load provider config: %w", err)
	}
	od := cfg.GetOnDemand("llamacpp")
	if od == nil {
		return nil, fmt.Errorf("provider config has no 'llamacpp' entry")
	}
	if len(od.InstallVariants) == 0 {
		return nil, fmt.Errorf("provider config 'llamacpp' entry has no install_variants: " +
			"archive filenames must be declared in config, not hardcoded in Go")
	}

	// 10s is an upper bound; WithTimeout inherits any shorter
	// deadline already on ctx. When ctx carries a cached inventory
	// (preflight-then-install flow) this is a cache lookup and the
	// timeout is never exercised.
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	detect := i.detectHardware
	if detect == nil {
		detect = variant.DetectLocalHardware
	}
	hw := detect(probeCtx)

	return variant.Select(od.InstallVariants, platform, hw)
}
