package ollama

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// withTestVariants uses the shipped artifact and version declarations on every platform.
func withTestVariants(t *testing.T) {
	t.Helper()
	data, err := templates.AppsFS.ReadFile("files/providers/external/ollama/config.yaml")
	require.NoError(t, err)
	var declared config.ExternalProvider
	require.NoError(t, yaml.Unmarshal(data, &declared))
	ollama := config.ServiceConfig{
		Name: "ollama", Protocol: config.ProtocolOllama, Mode: "external",
		InstallVariants: declared.InstallVariants,
		VersionSource:   declared.VersionSource,
		Runtime:         &config.AppRuntimeConfig{Endpoint: declared.Runtime.Endpoint},
		Capabilities:    declared.Capabilities,
	}
	fixture := &config.AppsConfig{}
	require.NoError(t, fixture.AddApp("ollama", ollama))
	old := install.LoadAppsConfig
	install.LoadAppsConfig = func() (*config.AppsConfig, error) { return fixture, nil }
	t.Cleanup(func() { install.LoadAppsConfig = old })
}

// newTestInstaller supplies a synthetic latest tag without making the suite depend on GitHub.
func newTestInstaller(t *testing.T) *Installer {
	t.Helper()
	inst := New(nil)
	inst.latestRelease = func(ctx context.Context, provider string) (upstream.Release, error) {
		require.NoError(t, ctx.Err())
		src, err := install.ProviderVersionSource(provider)
		require.NoError(t, err)
		require.Equal(t, "ollama/ollama", src.Repo)
		tag := "v0.32.15"
		return upstream.Release{Version: src.Normalize(tag), Tag: tag}, nil
	}
	return inst
}

func TestInstaller_InstallPlan(t *testing.T) {
	withTestVariants(t)
	inst := New(nil)

	plan, err := inst.InstallPlan(context.Background(), "0.20.7")
	require.NoError(t, err)
	assert.Equal(t, "ollama", plan.Provider)
	assert.Equal(t, "install", plan.Action)
	assert.NotEmpty(t, plan.Steps)

	// Linux cleans its archive after recording ownership; every plan must still mark the install.
	found := false
	for _, step := range plan.Steps {
		if strings.Contains(step.Description, "Mark installation as managed by zzRouter") {
			found = true
		}
	}
	assert.True(t, found, "the plan must record managed ownership")
}

func TestInstaller_UninstallPlan(t *testing.T) {
	inst := New(nil)
	plan, err := inst.UninstallPlan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "uninstall", plan.Action)
	assert.NotEmpty(t, plan.Steps)
}

func TestInstaller_UpgradePlan(t *testing.T) {
	withTestVariants(t)
	inst := New(nil)
	plan, err := inst.UpgradePlan(context.Background(), "0.20.7")
	require.NoError(t, err)
	assert.Equal(t, "upgrade", plan.Action)
	assert.NotEmpty(t, plan.Steps)

	// Ollama upgrades have no backup step (system-level install, not zzRouter-controlled)
	for _, step := range plan.Steps {
		assert.NotContains(t, step.Description, "Backup")
	}
}

func TestInstaller_SupportedPlatforms(t *testing.T) {
	inst := New(nil)
	platforms := inst.SupportedPlatforms()
	assert.GreaterOrEqual(t, len(platforms), 3)

	// Must include all three major OSes
	oses := make(map[string]bool)
	for _, p := range platforms {
		oses[p.OS] = true
	}
	assert.True(t, oses["linux"])
	assert.True(t, oses["darwin"])
	assert.True(t, oses["windows"])
}

// isolateProviderRoot points the provider tree at a scratch dir for one test.
//
// Without it this test reads the real provider root, so it fails on any
// machine where zzRouter has actually installed Ollama — the marker from that
// install is indistinguishable from one the test created.
func isolateProviderRoot(t *testing.T) {
	t.Helper()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
}

func TestInstaller_IsInstalled(t *testing.T) {
	isolateProviderRoot(t)

	inst := New(nil)

	// The managed marker is what separates "zzRouter installed this" from
	// "the operator installed it themselves". Absent marker means not
	// installed even on a machine with ollama on PATH, which is the whole
	// point: uninstall must not remove a binary zzRouter did not place.
	assert.False(t, inst.IsInstalled(), "no managed marker means not installed")

	// The complementary case — marker present but binary missing — is not
	// asserted here: findBinary falls back to absolute paths like
	// /opt/homebrew/bin/ollama that no env var can redirect, so it cannot be
	// made to fail deterministically. Asserting it against findBinary's own
	// result would just restate the implementation.
}

func TestManagedBinaryUsesWindowsInstallerDirectory(t *testing.T) {
	isolateProviderRoot(t)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	installed := filepath.Join(windowsInstallDir(), "ollama.exe")
	writeProviderFile(t, installed, "managed executable")
	inst := New(nil)
	assert.Empty(t, inst.managedBinary("windows"), "an existing binary alone does not establish ownership")
	writeProviderFile(t, fsroot.ManagedMarkerPath("ollama"), "managed")
	assert.Equal(t, installed, inst.managedBinary("windows"))
	require.NoError(t, os.Remove(installed))
	writeProviderFile(t, filepath.Join(fsroot.ProviderBinDir("ollama"), binaryName()), "unrelated executable")
	assert.Empty(t, inst.managedBinary("windows"), "do not fall back to a different installation")
}

// InstalledVersion refuses to answer for an unmanaged install, so an
// operator-installed Ollama is never reported as a zzRouter-managed version.
func TestInstaller_InstalledVersionRequiresManagedMarker(t *testing.T) {
	isolateProviderRoot(t)

	_, err := New(nil).InstalledVersion()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed")
}

// writeProviderFile creates a file under the (already isolated) provider dir.
func writeProviderFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// The recorded version outranks the binary because `ollama --version` answers
// about the running daemon. Without this, an upgrade that leaves the old
// daemon serving reads back as the version it was supposed to replace.
func TestInstaller_InstalledVersionPrefersRecordedFile(t *testing.T) {
	isolateProviderRoot(t)
	writeProviderFile(t, fsroot.ManagedMarkerPath("ollama"), "managed")
	// A value no ollama binary could ever print, so reading the file is the
	// only way to produce it. A realistic version would also be satisfied by
	// whatever ollama happens to be installed on the machine running this.
	writeProviderFile(t, fsroot.ProviderVersionFile("ollama"), "9.9.9-recorded\n")

	got, err := New(nil).InstalledVersion()
	require.NoError(t, err)
	assert.Equal(t, "9.9.9-recorded", got)
}

// This is the exact output that produced the wrong answer: the daemon that is
// already serving reports first, the binary just installed reports second.
func TestClientVersion(t *testing.T) {
	const mismatched = "ollama version is 0.21.2\nWarning: client version is 0.32.14\n"

	assert.Equal(t, "0.32.14", clientVersion(mismatched))
	assert.Equal(t, "0.21.2", install.ParseVersionOutput(mismatched),
		"the general parser takes the daemon's line, which is why clientVersion exists")

	// No daemon disagreement, no client line: the general parser is correct.
	assert.Empty(t, clientVersion("ollama version is 0.32.14\n"))
}

// The step must write the resolved release rather than shell out to the
// binary — no probe can distinguish the daemon from the binary it replaced.
func TestRecordVersionStepWritesResolvedVersion(t *testing.T) {
	step := recordVersionStep(7, "/tmp/version", "0.32.14")

	assert.Contains(t, step.Command, "0.32.14")
	assert.NotContains(t, step.Command, "--version",
		"probing the binary is what recorded the daemon's version")
	assert.Equal(t, "/tmp/version", step.Verify.Path)

	// The version is a literal this plan already resolved, so the check can
	// be that the file holds it. file_exists passed for any version at all,
	// which let a plan for one release read as already installed against a
	// tree holding another.
	assert.Equal(t, "file_equals", step.Verify.Type)
	assert.Equal(t, "0.32.14", step.Verify.Expected)
	assert.True(t, step.Verify.PlanScoped,
		"a node on another release still has an intact install")
}

// Homebrew picks the version, so there is nothing to write ahead of time.
// `brew list --versions` names the installed formula and cannot report a
// running daemon instead.
func TestRecordBrewVersionStepAsksHomebrew(t *testing.T) {
	step := recordBrewVersionStep(5, "/tmp/version")

	assert.Contains(t, step.Command, "brew list --versions ollama")
	assert.NotContains(t, step.Command, "--version ")
}

// A stale daemon on 11434 answers the generic probe, so an upgrade whose new
// binary never bound the port would otherwise verify green.
func TestHealthCheckStepAssertsServingVersion(t *testing.T) {
	pinned := healthCheckStep(6, "0.32.14")
	assert.Equal(t, "0.32.14", pinned.Verify.Expected)
	assert.Contains(t, pinned.Description, "0.32.14")

	// Homebrew picks the version, so there is nothing to assert against.
	unpinned := healthCheckStep(4, "")
	assert.Equal(t, "version", unpinned.Verify.Expected)
}

func TestServeStepUsesManagedLifecycle(t *testing.T) {
	withTestVariants(t)
	plan, err := New(nil).InstallPlan(context.Background(), "0.32.14")
	require.NoError(t, err)
	serve := startStep(t, plan.Steps)
	expected := install.ServiceStart
	if runtime.GOOS == "darwin" {
		expected = install.ServiceRestart
	}
	assert.Equal(t, expected, serve.ServiceAction)
	assert.Empty(t, serve.Command)
	assert.False(t, serve.Optional, "a failed managed start must fail installation")
}

// A foreign Ollama reporting the version being installed passes the health
// check, so the upgrade reported every step green while the managed binary
// had lost the bind race for 11434 and exited. Every model load then failed
// against a server that could not find its own runner.
func TestVerifyManagedServerStep(t *testing.T) {
	t.Parallel()

	step := verifyManagedServerStep(8, "/opt/zzrouter/providers/ollama/bin/ollama")

	assert.False(t, step.Optional, "an optional step never runs its Verify, which IS the assertion")
	assert.Equal(t, "command_output", step.Verify.Type)
	assert.Equal(t, "managed", step.Verify.Expected)
	assert.Contains(t, step.Command, "pgrep -f")
	assert.Contains(t, step.Command, "exit 1", "a foreign server must fail the step, not just report")

	// pkill/pgrep -f read whole command lines including the matching
	// shell's own, so an unbracketed path matches itself: the probe would
	// report the managed server running no matter what is serving.
	for _, cmd := range []string{step.Command, step.Verify.Command} {
		assert.Contains(t, cmd, "/opt/zzrouter/providers/ollama/bin/[o]llama serve")
		assert.NotContains(t, cmd, "bin/ollama serve")
	}
}

// The two steps are one assertion: the version alone cannot tell a fresh
// install from a same-version stranger, and a live managed process alone
// cannot tell whether it is the one answering.
func TestInstallPlanVerifiesTheServerItStarted(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Homebrew owns the daemon through launchd; zzRouter starts no process to verify")
	}
	withTestVariants(t)

	plan, err := New(nil).InstallPlan(context.Background(), "0.32.14")
	require.NoError(t, err)

	health, verify := -1, -1
	for i := range plan.Steps {
		switch {
		case strings.HasPrefix(plan.Steps[i].Description, "Verify Ollama"):
			health = i
		case strings.HasPrefix(plan.Steps[i].Description, "Verify the managed Ollama"):
			verify = i
		}
	}
	require.NotEqual(t, -1, verify, "install plan must prove the managed binary is serving")
	require.NotEqual(t, -1, health)
	assert.Greater(t, verify, health, "a binary that will exit on address-in-use needs the health check's wait to do it")
}

// The versions endpoint reports ollama's raw tag as "v0.32.14" while the
// download URLs spell the "v" themselves. A caller that passed the tag
// through used to build ".../download/vv0.32.14/", which 404s.
func TestInstallPlanAcceptsEitherVersionShape(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("the Homebrew path does not pin a version")
	}
	withTestVariants(t)

	for _, requested := range []string{"0.32.14", "v0.32.14"} {
		t.Run(requested, func(t *testing.T) {
			plan, err := New(nil).InstallPlan(context.Background(), requested)
			require.NoError(t, err)
			assert.Equal(t, "0.32.14", plan.Version)
			for _, step := range plan.Steps {
				assert.NotContains(t, step.Command, "vv0.32.14")
			}
		})
	}
}

// `brew install` on an already-installed formula prints "already installed"
// and exits 0, so an upgrade changed nothing yet still reported success.
func TestDarwinStepsUpgradeRatherThanReinstall(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Homebrew path only builds on darwin")
	}
	withTestVariants(t)

	plan, err := New(nil).InstallPlan(context.Background(), "")
	require.NoError(t, err)

	var brew string
	for _, s := range plan.Steps {
		if strings.Contains(s.Command, "brew") && strings.Contains(s.Description, "Homebrew") {
			brew = s.Command
			break
		}
	}
	require.NotEmpty(t, brew, "the darwin plan must install via Homebrew")
	assert.Contains(t, brew, "brew upgrade")
	assert.Contains(t, brew, "brew install", "a first-time install has nothing to upgrade")
}

// Homebrew picks the version, and its formula trails the GitHub release the
// version check reports. Asking for a version brew does not have must fail
// loudly rather than install something else and report success.
func TestDarwinStepsConfirmRequestedVersion(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Homebrew path only builds on darwin")
	}
	withTestVariants(t)

	plan, err := New(nil).InstallPlan(context.Background(), "0.32.15")
	require.NoError(t, err)

	var confirm *install.Step
	for i := range plan.Steps {
		if strings.HasPrefix(plan.Steps[i].Description, "Confirm Homebrew installed") {
			confirm = &plan.Steps[i]
			break
		}
	}
	require.NotNil(t, confirm, "a requested version must be confirmed")
	assert.Contains(t, confirm.Description, "0.32.15")
	assert.Contains(t, confirm.Command, "exit 1")

	// No request, nothing to confirm — brew's choice is the answer.
	unpinned, err := New(nil).InstallPlan(context.Background(), "")
	require.NoError(t, err)
	for _, s := range unpinned.Steps {
		assert.NotContains(t, s.Description, "Confirm Homebrew installed")
	}
}

// Step numbers must stay dense and ordered whichever branch ran, because the
// progress percentage is derived from the "[n/m]" counter.
func TestInstallPlanStepNumbersAreSequential(t *testing.T) {
	withTestVariants(t)

	for _, requested := range []string{"", "0.32.15"} {
		plan, err := newTestInstaller(t).InstallPlan(context.Background(), requested)
		require.NoError(t, err)
		for i, s := range plan.Steps {
			assert.Equal(t, i+1, s.Number, "step %d out of order: %q", i, s.Description)
		}
	}
}

// Steps run as `sh -c` with no pipefail, so a pipeline reports only tar's
// exit status: a truncated archive lets zstd emit valid data and then fail,
// tar extracts the partial content and exits 0, and the install reports
// success having replaced only part of itself.
func TestExtractCommandDoesNotMaskDecompressionFailure(t *testing.T) {
	zst := extractCommand("ollama-linux-amd64.tar.zst", "'/tmp/a.tar.zst'", "'/opt/x'")
	assert.NotContains(t, zst, "|", "a pipe hides the decompressor's exit status")
	assert.Contains(t, zst, "--use-compress-program=zstd")

	// gzip needs no help; tar has always owned that one.
	assert.Equal(t, "tar -xzf '/tmp/a.tgz' -C '/opt/x'",
		extractCommand("ollama-linux-amd64.tgz", "'/tmp/a.tgz'", "'/opt/x'"))
}

// Execute only runs a step's Verify when the step is not Optional, and
// VerifyAll ignores an optional failure. The health check's Verify IS the
// proof that the right version is serving, so marking it optional made the
// assertion inert and let a stale daemon keep the job green.
func TestHealthCheckIsRequiredSoItsVerifyRuns(t *testing.T) {
	withTestVariants(t)

	plan, err := newTestInstaller(t).InstallPlan(context.Background(), "")
	require.NoError(t, err)

	var found bool
	for _, s := range plan.Steps {
		if !strings.Contains(s.Description, "Verify Ollama") {
			continue
		}
		found = true
		assert.False(t, s.Optional, "an optional step's Verify is never run")
		assert.NotEmpty(t, s.Verify.Expected)
	}
	require.True(t, found, "the plan must verify Ollama responds")
}

// `brew list | awk > file` reports awk's status, so a brew failure wrote an
// empty version and still exited 0 — the confirm step then reported
// "Homebrew installed ." rather than the real failure.
func TestRecordBrewVersionStepObservesBrewFailure(t *testing.T) {
	cmd := recordBrewVersionStep(5, "/tmp/version").Command
	assert.NotContains(t, cmd, "| awk", "a pipeline hides brew's exit status")
	assert.Contains(t, cmd, "&&")
}

func TestStopServerStepUsesManagedLifecycle(t *testing.T) {
	step := stopServerStep(5)
	assert.Equal(t, install.ServiceStop, step.ServiceAction)
	assert.Empty(t, step.Command, "lifecycle owner identifies the managed process without a shell")
}

// startStep returns the step that launches the daemon, from steps built
// for a named platform regardless of the platform running the test. The
// build methods are called directly because the env prefix has to be
// asserted on Linux and Windows from a macOS dev machine.
func startStep(t *testing.T, steps []install.Step) install.Step {
	t.Helper()
	for _, s := range steps {
		if strings.Contains(s.Description, "Start Ollama") {
			return s
		}
	}
	t.Fatal("no start step in plan")
	return install.Step{}
}

func TestManagedPlansKeepEnvironmentOutOfShell(t *testing.T) {
	hostile := `x" & calc.exe & "`
	env := map[string]string{"OLLAMA_HOST": hostile, "OLLAMA_NUM_PARALLEL": "2"}
	installer := New(func(string) map[string]string { return env })
	for _, platform := range []string{"linux", "windows", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			plan := &install.Plan{}
			switch platform {
			case "linux":
				installer.buildLinuxSteps(plan, "https://example.test/o.tgz", "o.tgz", "0.32.15")
			case "windows":
				installer.buildWindowsSteps(plan, "https://example.test/o.zip", "o.zip", "0.32.15")
			case "darwin":
				installer.buildDarwinSteps(plan, "")
			}
			step := startStep(t, plan.Steps)
			expected := install.ServiceStart
			if platform == "darwin" {
				expected = install.ServiceRestart
			}
			assert.Equal(t, expected, step.ServiceAction)
			assert.Empty(t, step.Command)
			assert.Equal(t, env, step.Env)
		})
	}
}

func linuxPlan(t *testing.T) *install.Plan {
	t.Helper()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	plan := &install.Plan{Provider: "ollama", Version: "0.32.15"}
	New(nil).buildLinuxSteps(plan, "https://example.invalid/ollama-linux-amd64.tar.zst",
		"ollama-linux-amd64.tar.zst", "0.32.15")
	return plan
}

// TestLinuxExtractNeverTargetsTheLiveTree is the stale-soname defect as
// it exists in ollama: the archive carries lib/ollama, which is the same
// ggml bundle llama.cpp ships, and tar overlays without removing what an
// earlier release left. The worker's lib/ollama holds libggml-base.so.0
// alongside its versioned soname for exactly this reason.
func TestLinuxExtractNeverTargetsTheLiveTree(t *testing.T) {
	plan := linuxPlan(t)
	live := fsroot.ProviderBinDir("ollama")
	staging := filepath.Join(fsroot.ProviderDir("ollama"), "incoming")

	var found bool
	for _, step := range plan.Steps {
		if !strings.HasPrefix(step.Description, "Extract ") {
			continue
		}
		found = true
		assert.Contains(t, step.Command, staging, "extract must unpack into staging")
		assert.NotContains(t, step.Command, " -C "+fsroot.ShellQuote(live))
	}
	assert.True(t, found, "no extract step in the linux plan")
}

// TestLinuxActivatesOnlyAfterTheDaemonIsStopped is the second thing
// staging buys ollama. The old plan extracted over bin/ and lib/ at step
// 3 and stopped the server at step 5, so it rewrote files the running
// daemon had mapped.
func TestLinuxActivatesOnlyAfterTheDaemonIsStopped(t *testing.T) {
	plan := linuxPlan(t)

	stop, activate, start := -1, -1, -1
	for i, step := range plan.Steps {
		switch {
		case strings.HasPrefix(step.Description, "Stop "):
			stop = i
		case strings.HasPrefix(step.Description, "Activate "):
			activate = i
		case strings.HasPrefix(step.Description, "Start Ollama"):
			start = i
		}
	}
	require.NotEqual(t, -1, stop, "no stop step")
	require.NotEqual(t, -1, activate, "no activation step")
	require.NotEqual(t, -1, start, "no start step")
	assert.Less(t, stop, activate, "the swap must not run under a live daemon")
	assert.Less(t, activate, start, "the daemon must come back on the new tree")
}

// TestLinuxActivationSwapsEveryPayloadDirectory guards the part unique to
// ollama: the archive carries bin/ AND lib/, and the provider root also
// holds the version file, the managed marker and serve.log. Swapping the
// root would destroy the records; missing lib/ would leave half the
// install stale.
func TestLinuxActivationSwapsEveryPayloadDirectory(t *testing.T) {
	plan := linuxPlan(t)
	root := fsroot.ProviderDir("ollama")

	var activate install.Step
	for _, step := range plan.Steps {
		if strings.HasPrefix(step.Description, "Activate ") {
			activate = step
		}
	}
	require.NotEmpty(t, activate.Command)

	for _, dir := range []string{filepath.Join(root, "bin"), filepath.Join(root, "lib")} {
		assert.Contains(t, activate.Command, fsroot.ShellQuote(dir),
			"activation does not swap %s", dir)
	}
	assert.NotContains(t, activate.Command, "mv "+fsroot.ShellQuote(root)+" ",
		"the provider root holds the version file and marker; it must not be swapped")
	assert.Contains(t, activate.Command, filepath.Join(root, "incoming"),
		"the emptied staging root should be removed by the same step")
}

// TestLinuxStagingScopedChecksAreTransient: VerifyAll re-runs every check
// after later steps and asks "is this install intact?". A check on a
// directory the swap consumes must not report damage once it is gone, or
// FinalizeOnboarding never fires and the provider installs without ever
// becoming active.
func TestLinuxStagingScopedChecksAreTransient(t *testing.T) {
	plan := linuxPlan(t)
	staging := filepath.Join(fsroot.ProviderDir("ollama"), "incoming")

	for _, step := range plan.Steps {
		if step.Verify.Type == "" {
			continue
		}
		if strings.HasPrefix(step.Description, "Activate ") {
			assert.False(t, step.Verify.Transient,
				"activation asserts staging is GONE, which is durable")
			continue
		}
		if strings.Contains(step.Verify.Path, staging) || strings.Contains(step.Verify.Command, staging) {
			assert.True(t, step.Verify.Transient,
				"step %q needs staging to exist but is not Transient", step.Description)
		}
	}
}

// TestLinuxStepNumbersAreSequential covers the renumbering that inserting
// staging steps forces. The existing sequential test runs whichever
// platform the test host is, so on anything but Linux it never sees this.
func TestLinuxStepNumbersAreSequential(t *testing.T) {
	plan := linuxPlan(t)
	require.NotEmpty(t, plan.Steps)
	for i, step := range plan.Steps {
		assert.Equal(t, i+1, step.Number, "step %q is numbered %d at position %d", step.Description, step.Number, i+1)
	}
}

// TestWindowsStopsClearingBeforeExtracting is the opposite defect on the
// other platform: the Windows plan removed the install directory and then
// expanded the archive into it, so a truncated download or a failed
// extract left the node with no ollama at all.
func TestWindowsStopsClearingBeforeExtracting(t *testing.T) {
	withTestVariants(t)
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	plan := &install.Plan{Provider: "ollama", Version: "0.32.15"}
	New(nil).buildWindowsSteps(plan, "https://example.invalid/ollama-windows-amd64.zip",
		"ollama-windows-amd64.zip", "0.32.15")

	live := windowsInstallDir()
	for _, step := range plan.Steps {
		if !strings.HasPrefix(step.Description, "Extract ") {
			continue
		}
		assert.NotContains(t, step.Command, "Remove-Item",
			"the live directory must not be cleared before a complete tree exists")
		assert.Contains(t, step.Command, live+".incoming")
	}

	for i, step := range plan.Steps {
		assert.Equal(t, i+1, step.Number, "step %q is numbered %d at position %d", step.Description, step.Number, i+1)
	}
}

// TestWindowsInstallDirMatchesDetection pins the single spelling. The
// plan used to name this location as a PowerShell $env: expression while
// findBinary resolved it in Go; if those ever disagreed, the installer
// would place ollama somewhere detection does not look.
func TestWindowsInstallDirMatchesDetection(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("findBinary only consults LOCALAPPDATA on Windows")
	}
	assert.Equal(t, filepath.Join(windowsInstallDir(), "ollama.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Ollama", "ollama.exe"))
}
