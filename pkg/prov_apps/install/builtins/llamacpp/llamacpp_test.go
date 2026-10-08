package llamacpp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withTestVariants swaps install.LoadAppsConfig with a fixture that
// carries a full set of llama.cpp install_variants. InstallPlan reads
// archive filenames from provider config rather than hardcoded Go, so
// tests must inject a config with the variants block populated.
func withTestVariants(t *testing.T) {
	t.Helper()
	llamacpp := config.ServiceConfig{
		Name:          "llama.cpp",
		Protocol:      config.ProtocolOpenAI,
		Mode:          "on-demand",
		PinnedVersion: "b8705",
		InstallVariants: []config.AppInstallVariant{
			{
				ID:        "win-amd64-cuda-12.4",
				Platforms: []config.InstallPlatform{{OS: "windows", Arch: "amd64"}},
				Artifact:  "llama-{version}-bin-win-cuda-12.4-x64.zip",
				GPU:       &config.VariantGPURequirement{Vendor: "nvidia", CUDAMin: "12.0"},
			},
			{
				ID:        "win-amd64-cpu",
				Platforms: []config.InstallPlatform{{OS: "windows", Arch: "amd64"}},
				Artifact:  "llama-{version}-bin-win-cpu-x64.zip",
			},
			{
				ID:        "win-arm64-cpu",
				Platforms: []config.InstallPlatform{{OS: "windows", Arch: "arm64"}},
				Artifact:  "llama-{version}-bin-win-cpu-arm64.zip",
			},
			{
				ID:        "linux-amd64",
				Platforms: []config.InstallPlatform{{OS: "linux", Arch: "amd64"}},
				Artifact:  "llama-{version}-bin-ubuntu-x64.tar.gz",
			},
			{
				ID:        "linux-arm64",
				Platforms: []config.InstallPlatform{{OS: "linux", Arch: "arm64"}},
				Artifact:  "llama-{version}-bin-ubuntu-arm64.tar.gz",
			},
			{
				ID:        "darwin-arm64",
				Platforms: []config.InstallPlatform{{OS: "darwin", Arch: "arm64"}},
				Artifact:  "llama-{version}-bin-macos-arm64.tar.gz",
			},
			{
				ID:        "darwin-amd64",
				Platforms: []config.InstallPlatform{{OS: "darwin", Arch: "amd64"}},
				Artifact:  "llama-{version}-bin-macos-x64.tar.gz",
			},
		},
		// Minimal Runtime so OnDemandProvider.Validate accepts the fixture
		// during AddApp (port pool + execution are required even though
		// InstallPlan only reads InstallVariants).
		Runtime: &config.AppRuntimeConfig{
			BasePort:  8080,
			PortRange: []int{8080, 8089},
			Execution: config.ExecutionConfig{Type: "cli", Command: "llama-server"},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		// InstallPlan reads version_source for the download host, so the
		// fixture must carry it the same way the shipped template does.
		VersionSource: &config.VersionSource{
			Type:        config.VersionSourceGitHubRelease,
			Repo:        "ggml-org/llama.cpp",
			StripPrefix: "b",
			Compare:     config.CompareBuildNumber,
		},
	}
	fixture := &config.AppsConfig{}
	require.NoError(t, fixture.AddApp("llamacpp", llamacpp))
	old := install.LoadAppsConfig
	install.LoadAppsConfig = func() (*config.AppsConfig, error) { return fixture, nil }
	t.Cleanup(func() { install.LoadAppsConfig = old })
}

func TestInstaller_InstallPlan(t *testing.T) {
	withTestVariants(t)

	inst := New()

	plan, err := inst.InstallPlan(context.Background(), "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, "llamacpp", plan.Provider)
	assert.Equal(t, "v1.0.0", plan.Version)
	assert.Equal(t, "install", plan.Action)
	assert.NotEmpty(t, plan.Steps)

	// Every step should have a verification, except the checksum step: its
	// check leaves no artifact, and any Verify it could carry would be
	// satisfied by the bad download it exists to reject (see the comment on
	// the step, and TestChecksumStepCannotBeSkipped below).
	for _, step := range plan.Steps {
		if strings.HasPrefix(step.Description, "Verify checksum of ") {
			continue
		}
		assert.NotEmpty(t, step.Verify.Type, "step %d missing verification", step.Number)
	}
}

// TestInstaller_InstallPlan_MultiArtifact installs a synthetic Windows
// CUDA variant declaring two peer artifacts (binary zip + cudart zip)
// and asserts the install plan emits two download steps and two extract
// steps, in YAML order. Mirrors the real llama.cpp + cudart shape.
func TestInstaller_InstallPlan_MultiArtifact(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("multi-artifact path is only exercised when CurrentPlatform()==windows")
	}
	t.Helper()
	llamacpp := config.ServiceConfig{
		Name:          "llama.cpp",
		Protocol:      config.ProtocolOpenAI,
		Mode:          "on-demand",
		PinnedVersion: "b8963",
		VersionSource: &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", StripPrefix: "b", Compare: config.CompareBuildNumber},
		InstallVariants: []config.AppInstallVariant{
			{
				ID:        "win-amd64-cuda-13.1",
				Platforms: []config.InstallPlatform{{OS: "windows", Arch: "amd64"}},
				Artifacts: []config.VariantArtifact{
					{Filename: "llama-{version}-bin-win-cuda-13.1-x64.zip"},
					{Filename: "cudart-llama-bin-win-cuda-13.1-x64.zip"},
				},
				GPU: &config.VariantGPURequirement{Vendor: "nvidia", CUDAMin: "13.0"},
			},
		},
		Runtime: &config.AppRuntimeConfig{
			BasePort:  8080,
			PortRange: []int{8080, 8089},
			Execution: config.ExecutionConfig{Type: "cli", Command: "llama-server"},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}
	fixture := &config.AppsConfig{}
	require.NoError(t, fixture.AddApp("llamacpp", llamacpp))
	old := install.LoadAppsConfig
	install.LoadAppsConfig = func() (*config.AppsConfig, error) { return fixture, nil }
	t.Cleanup(func() { install.LoadAppsConfig = old })

	inst := New()
	inst.detectHardware = func(context.Context) *hardware.HardwareInfo {
		return &hardware.HardwareInfo{GPUType: "nvidia", GPUs: []hardware.GPUInfo{{CUDAVersion: "13.1"}}}
	}
	plan, err := inst.InstallPlan(context.Background(), "b8963")
	require.NoError(t, err)

	var downloadSteps, extractSteps int
	for _, step := range plan.Steps {
		if strings.HasPrefix(step.Description, "Download ") {
			downloadSteps++
		}
		if strings.HasPrefix(step.Description, "Extract ") {
			extractSteps++
		}
	}
	assert.Equal(t, 2, downloadSteps, "expected one download step per peer artifact")
	assert.Equal(t, 2, extractSteps, "expected one extract step per peer artifact")
	for _, step := range plan.Steps {
		if strings.HasPrefix(step.Description, "Verify checksum of ") {
			assert.Empty(t, step.Verify.Type, "checksum steps must always execute")
			assert.NotNil(t, step.PostExec, "checksum comparison must be enforced")
			assert.False(t, step.Optional, "checksum mismatches must fail the install")
			continue
		}
		assert.NotEmpty(t, step.Verify.Type, "step %d (%s) missing verification", step.Number, step.Description)
	}
}

func TestInstallerResolveVariantUsesInjectedGPUFacts(t *testing.T) {
	withTestVariants(t)
	for _, tc := range []struct {
		name, vendor, cuda, variant string
	}{
		{"no GPU", "none", "", "win-amd64-cpu"},
		{"old CUDA", "nvidia", "11.0", "win-amd64-cpu"},
		{"supported CUDA", "nvidia", "13.1", "win-amd64-cuda-12.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := New()
			inst.detectHardware = func(context.Context) *hardware.HardwareInfo {
				return &hardware.HardwareInfo{GPUType: tc.vendor, GPUs: []hardware.GPUInfo{{CUDAVersion: tc.cuda}}}
			}
			selected, err := inst.resolveVariant(t.Context(), fsroot.Platform{OS: "windows", Arch: "amd64"})
			require.NoError(t, err)
			assert.Equal(t, tc.variant, selected.ID)
		})
	}
}

func TestInstaller_UninstallPlan(t *testing.T) {
	inst := New()
	plan, err := inst.UninstallPlan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "uninstall", plan.Action)
	assert.NotEmpty(t, plan.Steps)
}

func TestInstaller_SupportedPlatforms(t *testing.T) {
	inst := New()
	platforms := inst.SupportedPlatforms()
	assert.NotEmpty(t, platforms)
}

func TestInstaller_Preflight(t *testing.T) {
	previousRoot := fsroot.ProviderRootDir()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previousRoot) })

	inst := New()
	report := inst.Preflight(context.Background(), nil)
	assert.NotEmpty(t, report.Results)
	// curl and tar should be available on dev machines
	if runtime.GOOS != "windows" {
		assert.True(t, report.AllOK, "curl and tar should be available")
	}
}

// InstalledVersion reads the release the install recorded rather than the
// binary's banner. The two are not interchangeable: the file holds the tag
// ("b10502") that the download URLs and the upstream comparison are keyed on,
// while llama-server prints a bare build number, which the build_number
// comparator cannot line up against a tag.
func TestInstaller_InstalledVersionPrefersRecordedFile(t *testing.T) {
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	versionFile := fsroot.ProviderVersionFile("llamacpp")
	require.NoError(t, os.MkdirAll(filepath.Dir(versionFile), 0o755))
	require.NoError(t, os.WriteFile(versionFile, []byte("b10502\n"), 0o644))

	got, err := New().InstalledVersion()
	require.NoError(t, err)
	assert.Equal(t, "b10502", got)
}

// The versions endpoint reports llama.cpp's newest release as "10502"
// (normalized) and "b10502" (the tag). The release URLs only accept the tag,
// so a caller that read the other field used to get a 404.
func TestInstallPlanAcceptsEitherVersionShape(t *testing.T) {
	withTestVariants(t)

	for _, requested := range []string{"b10502", "10502"} {
		t.Run(requested, func(t *testing.T) {
			plan, err := New().InstallPlan(context.Background(), requested)
			require.NoError(t, err)
			assert.Equal(t, "b10502", plan.Version)
		})
	}
}

// The version marker is what IsInstalled and InstalledVersion both read, so
// checking only that the file exists let a plan for one release report itself
// as already installed against a tree holding another — which is how a
// reinstall became a silent downgrade that current_state called "installed".
func TestInstallPlanVerifiesRecordedVersionMatchesPlan(t *testing.T) {
	withTestVariants(t)

	plan, err := New().InstallPlan(context.Background(), "b10549")
	require.NoError(t, err)

	var found bool
	for _, step := range plan.Steps {
		if step.Description != "Record installed version" {
			continue
		}
		found = true
		assert.Equal(t, "file_equals", step.Verify.Type,
			"file_exists passes for any version, including an older one")
		assert.Equal(t, plan.Version, step.Verify.Expected)
		assert.True(t, step.Verify.PlanScoped,
			"a node on another release still has an intact install, so install/verify must not call it damage")
	}
	require.True(t, found, "plan has no record-version step")
}

// Step numbers address steps: /install/execute-step takes one, and guided
// install walks them in order. A per-artifact loop that emits N steps but
// advances the counter by something other than N produces duplicate or
// skipped numbers, and the multi-artifact Windows path — the only one where
// the loop runs twice — is skipped on every platform CI actually runs on.
// So call the builder directly rather than going through CurrentPlatform().
// winLayout / nixLayout stand in for the real fsroot-derived layout so
// the plan-shape tests do not depend on a provider root existing.
func winLayout() binLayout {
	return binLayout{
		StagedDir: install.StagedDir{Live: `C:\bin`, Staging: `C:\bin.incoming`, Previous: `C:\bin.previous`},
		binary:    "llama-server.exe",
	}
}

func nixLayout() binLayout {
	return binLayout{
		StagedDir: install.StagedDir{Live: "/opt/bin", Staging: "/opt/bin.incoming", Previous: "/opt/bin.previous"},
		binary:    "llama-server",
	}
}

func TestBuildStepsNumberContiguously(t *testing.T) {
	artifacts := []artifactRef{
		{Filename: "llama-b10549-bin-win-cuda-13.3-x64.zip", URL: "https://example.invalid/a.zip"},
		{Filename: "cudart-llama-bin-win-cuda-13.3-x64.zip", URL: "https://example.invalid/b.zip"},
	}
	src := &config.VersionSource{
		Type:        config.VersionSourceGitHubRelease,
		Repo:        "ggml-org/llama.cpp",
		StripPrefix: "b",
	}

	for _, tt := range []struct {
		name  string
		build func(*install.Plan)
	}{
		{"windows", func(p *install.Plan) {
			New().buildWindowsSteps(p, src, winLayout(), artifacts)
		}},
		{"unix", func(p *install.Plan) {
			New().buildUnixSteps(p, src, nixLayout(), artifacts)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := &install.Plan{Provider: "llamacpp", Version: "b10549"}
			tt.build(plan)

			require.NotEmpty(t, plan.Steps)
			for i, step := range plan.Steps {
				assert.Equal(t, i+1, step.Number,
					"step %q is numbered %d but sits at position %d", step.Description, step.Number, i+1)
			}
		})
	}
}

// The checksum step used to fetch a SHA256SUMS file llama.cpp has never
// published, and Optional made the resulting 404 a swallowed warning — so
// every binary it placed was unverified while the step reported green. The
// hook degrades to a warning on its own when no digest can be fetched, so
// the step must not be Optional, or a genuine mismatch is swallowed too.
func TestChecksumStepIsEnforcedOnEveryPlatform(t *testing.T) {
	artifacts := []artifactRef{{Filename: "llama-b10549-bin-ubuntu-vulkan-x64.tar.gz", URL: "https://example.invalid/a.tar.gz"}}
	src := &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", StripPrefix: "b"}

	for _, tt := range []struct {
		name  string
		build func(*install.Plan)
	}{
		{"windows", func(p *install.Plan) { New().buildWindowsSteps(p, src, winLayout(), artifacts) }},
		{"unix", func(p *install.Plan) { New().buildUnixSteps(p, src, nixLayout(), artifacts) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := &install.Plan{Provider: "llamacpp", Version: "b10549"}
			tt.build(plan)

			var found bool
			for _, step := range plan.Steps {
				if !strings.HasPrefix(step.Description, "Verify checksum of ") {
					continue
				}
				found = true
				assert.False(t, step.Optional, "an Optional step swallows the PostExec error that reports a mismatch")
				assert.NotNil(t, step.PostExec, "the comparison happens in PostExec, not in the printed command")
				assert.NotContains(t, step.Command, "SHA256SUMS",
					"llama.cpp publishes no SHA256SUMS; that URL 404s on every release")
			}
			assert.True(t, found, "no checksum step in the %s plan", tt.name)
		})
	}
}

// runInstallStep short-circuits any step whose Verify already passes, without
// running it. The checksum step therefore must carry no Verify: with
// file_exists on the archive it reported "done" for a present-but-corrupt
// download, never hashing it — verified live against a planted bad archive.
func TestChecksumStepCannotBeSkipped(t *testing.T) {
	artifacts := []artifactRef{{Filename: "llama-b10549-bin-ubuntu-vulkan-x64.tar.gz", URL: "https://example.invalid/a.tar.gz"}}
	src := &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", StripPrefix: "b"}

	for _, tt := range []struct {
		name  string
		build func(*install.Plan)
	}{
		{"windows", func(p *install.Plan) { New().buildWindowsSteps(p, src, winLayout(), artifacts) }},
		{"unix", func(p *install.Plan) { New().buildUnixSteps(p, src, nixLayout(), artifacts) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := &install.Plan{Provider: "llamacpp", Version: "b10549"}
			tt.build(plan)

			for _, step := range plan.Steps {
				if !strings.HasPrefix(step.Description, "Verify checksum of ") {
					continue
				}
				assert.Empty(t, step.Verify.Type,
					"a Verify that passes lets runInstallStep skip the hash entirely")
			}
		})
	}
}

// TestExtractNeverTargetsTheLiveDirectory is the stale-soname defect.
// Extraction used to write into bin/ directly, and tar overlays without
// removing what a previous release left, so bin/ accumulated every
// libggml.so.* ever installed — all of them loadable, because ggml
// discovers backends by scanning that directory.
func TestExtractNeverTargetsTheLiveDirectory(t *testing.T) {
	artifacts := []artifactRef{
		{Filename: "llama-b10549-bin-ubuntu-vulkan-x64.tar.gz", URL: "https://example.invalid/a.tar.gz"},
		{Filename: "cudart-llama-bin-win-cuda-13.3-x64.zip", URL: "https://example.invalid/b.zip"},
	}
	src := &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", StripPrefix: "b"}

	for _, tt := range []struct {
		name   string
		layout binLayout
		build  func(*install.Plan, binLayout)
	}{
		{"windows", winLayout(), func(p *install.Plan, l binLayout) { New().buildWindowsSteps(p, src, l, artifacts) }},
		{"unix", nixLayout(), func(p *install.Plan, l binLayout) { New().buildUnixSteps(p, src, l, artifacts) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := &install.Plan{Provider: "llamacpp", Version: "b10549"}
			tt.build(plan, tt.layout)

			var extracts int
			for _, step := range plan.Steps {
				if !strings.HasPrefix(step.Description, "Extract ") {
					continue
				}
				extracts++
				assert.Contains(t, step.Command, tt.layout.Staging,
					"extract must unpack into staging")
				assert.Equal(t, tt.layout.Staging, step.Verify.Path,
					"extract verify should watch staging, not the live tree")
			}
			assert.Equal(t, len(artifacts), extracts, "one extract per artifact")
		})
	}
}

// TestNoStepClearsTheLiveDirectory pins why this is a swap and not an
// `rm -rf` before extracting. Multi-artifact variants unpack several
// archives into one directory, so clearing up front means a failed
// second download leaves the node with no llama.cpp at all.
func TestNoStepClearsTheLiveDirectory(t *testing.T) {
	artifacts := []artifactRef{{Filename: "a.tar.gz", URL: "https://example.invalid/a.tar.gz"}}
	src := &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", StripPrefix: "b"}

	l := nixLayout()
	plan := &install.Plan{Provider: "llamacpp", Version: "b10549"}
	New().buildUnixSteps(plan, src, l, artifacts)

	for _, step := range plan.Steps {
		if strings.HasPrefix(step.Description, "Activate ") {
			continue // the swap legitimately moves the live tree aside
		}
		assert.NotContains(t, step.Command, "rm -rf "+l.Live,
			"step %q clears the live install", step.Description)
	}
}

// TestActivationIsGatedAndTerminal pins the ordering that makes staging
// safe: the live tree is replaced only after the staged binary has been
// proven to run, and the version file is written only after that.
func TestActivationIsGatedAndTerminal(t *testing.T) {
	artifacts := []artifactRef{{Filename: "a.tar.gz", URL: "https://example.invalid/a.tar.gz"}}
	src := &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", StripPrefix: "b"}

	for _, tt := range []struct {
		name   string
		layout binLayout
		build  func(*install.Plan, binLayout)
	}{
		{"windows", winLayout(), func(p *install.Plan, l binLayout) { New().buildWindowsSteps(p, src, l, artifacts) }},
		{"unix", nixLayout(), func(p *install.Plan, l binLayout) { New().buildUnixSteps(p, src, l, artifacts) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := &install.Plan{Provider: "llamacpp", Version: "b10549"}
			tt.build(plan, tt.layout)

			gate, activate, record := -1, -1, -1
			for i, step := range plan.Steps {
				switch {
				case strings.HasPrefix(step.Description, "Verify the staged"):
					gate = i
				case strings.HasPrefix(step.Description, "Activate "):
					activate = i
				case strings.HasPrefix(step.Description, "Record installed version"):
					record = i
				}
			}
			require.NotEqual(t, -1, gate, "no staged-binary gate")
			require.NotEqual(t, -1, activate, "no activation step")
			require.NotEqual(t, -1, record, "no record-version step")
			assert.Less(t, gate, activate, "a tree that cannot run must not replace one that can")
			assert.Less(t, activate, record, "version must be recorded only once the tree is live")
			assert.Equal(t, len(plan.Steps)-1, record, "record-version must be terminal")

			// The durable integrity check lives on activation: every
			// earlier check is scoped to a directory the swap consumes.
			act := plan.Steps[activate]
			assert.False(t, act.Verify.Transient, "activation is what VerifyAll leans on")
			assert.Contains(t, act.Verify.Command, tt.layout.liveBinary())
		})
	}
}

// TestStagingScopedChecksAreTransient guards the interaction with
// VerifyAll, which re-runs every step's check after later steps. Without
// Transient, each staging-scoped check fails the moment the swap removes
// the directory, VerifyAll reports the install as damaged, and
// FinalizeOnboarding never fires — the provider installs but never
// becomes active.
func TestStagingScopedChecksAreTransient(t *testing.T) {
	artifacts := []artifactRef{{Filename: "a.tar.gz", URL: "https://example.invalid/a.tar.gz"}}
	src := &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", StripPrefix: "b"}

	l := nixLayout()
	plan := &install.Plan{Provider: "llamacpp", Version: "b10549"}
	New().buildUnixSteps(plan, src, l, artifacts)

	for _, step := range plan.Steps {
		if step.Verify.Type == "" {
			continue
		}
		// Activation is the exception, and for the opposite reason: its
		// check names staging to assert the directory is GONE, which is
		// permanently true once the swap succeeds. A check that needs
		// staging to exist is transient; one that needs it absent is the
		// most durable check in the plan.
		if strings.HasPrefix(step.Description, "Activate ") {
			assert.False(t, step.Verify.Transient)
			continue
		}
		mentionsStaging := strings.Contains(step.Verify.Path, l.Staging) ||
			strings.Contains(step.Verify.Command, l.Staging)
		if mentionsStaging {
			assert.True(t, step.Verify.Transient,
				"step %q needs staging to exist but is not Transient", step.Description)
		}
	}
}

// TestActivationVerifyFailsWhileStagingExists guards the single-step
// executor, which skips any step whose Verify already passes. A check
// that merely ran the live binary would be satisfied by the install
// being replaced, so a guided upgrade would skip the swap entirely and
// leave the staged tree unused.
func TestActivationVerifyFailsWhileStagingExists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("asserts the POSIX form of the check")
	}
	root := t.TempDir()
	l := binLayout{
		StagedDir: install.StagedDir{
			Live:     filepath.Join(root, "bin"),
			Staging:  filepath.Join(root, "bin.incoming"),
			Previous: filepath.Join(root, "bin.previous"),
		},
		binary: "llama-server",
	}
	// A working live install AND a staged tree waiting to replace it.
	require.NoError(t, os.MkdirAll(l.Live, 0o755))
	require.NoError(t, os.WriteFile(l.liveBinary(), []byte("#!/bin/sh\necho 'version: 1 (old)'\n"), 0o755))
	require.NoError(t, os.MkdirAll(l.Staging, 0o755))

	src := &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", StripPrefix: "b"}
	plan := &install.Plan{Provider: "llamacpp", Version: "b10549"}
	New().buildUnixSteps(plan, src, l, []artifactRef{{Filename: "a.tar.gz", URL: "https://example.invalid/a.tar.gz"}})

	var activate install.Step
	for _, step := range plan.Steps {
		if strings.HasPrefix(step.Description, "Activate ") {
			activate = step
		}
	}
	require.NotEmpty(t, activate.Verify.Command)

	got := install.VerifyStep(activate)
	assert.False(t, got.Passed,
		"activation reported satisfied while the staged tree was still waiting")

	// Once activation has happened, the same check must pass.
	require.NoError(t, os.Remove(l.Staging))
	assert.True(t, install.VerifyStep(activate).Passed)
}

// TestUnixActivateReplacesRatherThanOverlays runs the real command. A
// stale shared object left by an earlier release must NOT survive into
// the activated tree — that is the whole defect, and asserting it on the
// generated string rather than on a filesystem would not show it.
func TestUnixActivateReplacesRatherThanOverlays(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exercises the POSIX sh activation command")
	}
	root := t.TempDir()
	l := binLayout{
		StagedDir: install.StagedDir{
			Live:     filepath.Join(root, "bin"),
			Staging:  filepath.Join(root, "bin.incoming"),
			Previous: filepath.Join(root, "bin.previous"),
		},
		binary: "llama-server",
	}

	// A live install from an older release, plus the staged new one.
	require.NoError(t, os.MkdirAll(l.Live, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(l.Live, "libggml.so.0.9.11"), []byte("old"), 0o644))
	require.NoError(t, os.WriteFile(l.liveBinary(), []byte("old"), 0o755))
	require.NoError(t, os.MkdirAll(l.Staging, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(l.Staging, "libggml.so.0.20.2"), []byte("new"), 0o644))
	require.NoError(t, os.WriteFile(l.stagedBinary(), []byte("new"), 0o755))

	// Running the generated command is the point of the test: asserting
	// on the string would not show whether a stale file survives.
	out, err := host.Command("sh", "-c", l.ActivateUnixCommand()).CombinedOutput()
	require.NoError(t, err, "activation failed: %s", out)

	entries, err := os.ReadDir(l.Live)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"libggml.so.0.20.2", "llama-server"}, names,
		"the stale soname survived activation")

	body, err := os.ReadFile(l.liveBinary())
	require.NoError(t, err)
	assert.Equal(t, "new", string(body), "live binary is not the staged one")

	for _, leftover := range []string{l.Staging, l.Previous} {
		_, err := os.Stat(leftover)
		assert.True(t, os.IsNotExist(err), "%s should not survive activation", leftover)
	}
}

// TestUnixActivateRollsBackAFailedMove covers the window the ordering
// exists to protect: if the staged tree cannot be moved into place, the
// node must be left with the install it already had, not with nothing.
func TestUnixActivateRollsBackAFailedMove(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exercises the POSIX sh activation command")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	root := t.TempDir()
	l := binLayout{
		StagedDir: install.StagedDir{
			Live:     filepath.Join(root, "bin"),
			Staging:  filepath.Join(root, "nonexistent.incoming"),
			Previous: filepath.Join(root, "bin.previous"),
		},
		binary: "llama-server",
	}
	require.NoError(t, os.MkdirAll(l.Live, 0o755))
	require.NoError(t, os.WriteFile(l.liveBinary(), []byte("old"), 0o755))

	// staging does not exist, so the move fails and the rollback runs.
	// Running the generated command is the point of the test: asserting
	// on the string would not show whether a stale file survives.
	out, err := host.Command("sh", "-c", l.ActivateUnixCommand()).CombinedOutput()
	assert.Error(t, err, "a failed activation must report failure: %s", out)

	body, readErr := os.ReadFile(l.liveBinary())
	require.NoError(t, readErr, "the working install was lost")
	assert.Equal(t, "old", string(body))
}
