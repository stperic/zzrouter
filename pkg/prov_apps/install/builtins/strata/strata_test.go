package strata

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/interpreter"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func managedFixture(t *testing.T) string {
	t.Helper()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	dir := writeReleaseFixture(t, "0.1.44.1")
	require.NoError(t, os.WriteFile(fsroot.ManagedMarkerPath("strata"), []byte("managed"), 0600))
	require.NoError(t, os.WriteFile(fsroot.ProviderVersionFile("strata"), []byte("0.1.44.1\n"), 0600))
	return dir
}

func writeReleaseFixture(t *testing.T, version string) string {
	t.Helper()
	dir := releaseDir(version)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".venv", "bin"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "serve"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "engine"), 0700))
	for _, path := range []string{filepath.Join(dir, ".venv", "bin", "python"), filepath.Join(dir, "serve", "server.py"), filepath.Join(dir, "engine", "strata")} {
		require.NoError(t, os.WriteFile(path, []byte("fixture"), 0700))
	}
	root := fsroot.ProviderDir("strata")
	for path, body := range map[string]string{
		filepath.Join(root, "data", "mtp", "rt", "experts.bin"): "draft layer",
		launcherPath(): launcher,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, []byte(body), 0700))
	}
	return dir
}

func TestStrataCompleteness(t *testing.T) {
	dir := managedFixture(t)
	i := New(nil)
	require.True(t, i.IsInstalled())
	version, err := i.InstalledVersion()
	require.NoError(t, err)
	require.Equal(t, "0.1.44.1", version)
	for _, path := range []string{
		fsroot.ManagedMarkerPath("strata"), fsroot.ProviderVersionFile("strata"),
		filepath.Join(dir, "engine", "strata"), filepath.Join(dir, ".venv", "bin", "python"),
		filepath.Join(dir, "serve", "server.py"),
		filepath.Join(fsroot.ProviderDir("strata"), "data", "mtp", "rt", "experts.bin"), launcherPath(),
	} {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.Remove(path))
		require.False(t, i.IsInstalled(), "missing %s", path)
		require.NoError(t, os.WriteFile(path, body, 0700))
	}
	require.NoError(t, os.WriteFile(launcherPath(), []byte(launcher+"\n# changed\n"), 0700))
	require.False(t, i.IsInstalled(), "launcher digest is part of completeness")
	require.NoError(t, os.WriteFile(launcherPath(), []byte(launcher), 0700))
	outside := filepath.Join(t.TempDir(), "foreign-engine")
	require.NoError(t, os.WriteFile(outside, []byte("foreign"), 0700))
	engine := filepath.Join(dir, "engine", "strata")
	require.NoError(t, os.Remove(engine))
	require.NoError(t, os.Symlink(outside, engine))
	require.False(t, i.IsInstalled(), "engine must remain within the managed release")
}

func strataPlan(t *testing.T, i *Installer, version string) *install.Plan {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
	apps, err := config.LoadAppsConfig(root)
	require.NoError(t, err)
	oldLoader := install.LoadAppsConfig
	install.LoadAppsConfig = func() (*config.AppsConfig, error) { return apps, nil }
	t.Cleanup(func() { install.LoadAppsConfig = oldLoader })
	i.latestRelease = func(context.Context, string) (string, error) { return "0.1.44.1", nil }
	plan, err := i.InstallPlan(t.Context(), version)
	require.NoError(t, err)
	return plan
}

func TestInstallPlanPinsNikoReleaseAndBuildsEngineOnly(t *testing.T) {
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	i := New(nil)
	plan := strataPlan(t, i, "")
	require.Equal(t, "0.1.44.1", plan.Version)
	require.Contains(t, plan.Steps[1].Command, repositoryURL)
	require.Contains(t, plan.Steps[1].Command, "v0.1.44.1")
	for _, step := range plan.Steps {
		require.Empty(t, step.ServiceAction)
		require.NotContains(t, step.Command, "setup.sh")
		require.NotContains(t, step.Command, "--vision")
	}
	_, err := i.InstallPlan(t.Context(), "..")
	require.ErrorContains(t, err, "numeric version")
}

func TestShippedStrataTemplateNamesTheIntendedProject(t *testing.T) {
	body, err := templates.AppsFS.ReadFile("files/providers/on-demand/strata/config.yaml")
	require.NoError(t, err)
	var provider config.OnDemandProvider
	require.NoError(t, yaml.Unmarshal(body, &provider))
	require.Equal(t, repository, provider.VersionSource.Repo)
	require.Equal(t, config.VersionSourceGitHubRelease, provider.VersionSource.Type)
	require.Equal(t, config.ComparePEP440, provider.VersionSource.Compare)
	require.NotNil(t, provider.Enabled)
	require.False(t, *provider.Enabled)
	require.Equal(t, []config.InstallPlatform{{OS: "linux", Arch: "amd64"}}, provider.Platforms)
	require.Equal(t, []string{"qwen4exp"}, provider.Capabilities.Architectures)
	require.Zero(t, provider.Capabilities.Priority)
	require.Equal(t, "strata-launch", provider.Runtime.Execution.Command)
	require.Equal(t, "--", provider.Runtime.Execution.Args[len(provider.Runtime.Execution.Args)-1])
	require.Equal(t, "900s", provider.Runtime.HealthCheck.ReadinessProbe.Timeout)
	require.NotEmpty(t, provider.Runtime.HealthCheck.ReadinessProbe.Serve.Body)
	require.Equal(t, "30s", provider.Runtime.HealthCheck.Interval)
	require.Equal(t, "5s", provider.Runtime.HealthCheck.Timeout)
	dictionary, err := templates.AppsFS.ReadFile("files/providers/on-demand/strata/schema.yaml")
	require.NoError(t, err)
	declared, err := schema.LoadYAMLSchema(dictionary)
	require.NoError(t, err)
	merged, conflicts := schema.Merge("strata", declared)
	require.Empty(t, conflicts)
	for name, value := range provider.Defaults.Parameters {
		require.Nil(t, schema.Check(name, value, merged.Parameters), name)
	}
	require.NotNil(t, schema.Check("spec", "1", merged.Parameters))
	require.NotNil(t, schema.Check("spec-min-p", "2", merged.Parameters))
}

func TestSetupGetsControlledEnvironmentWithProviderVariables(t *testing.T) {
	t.Setenv("ZZROUTER_ADMIN_API_KEY", "must-not-reach-upstream-setup")
	dir := t.TempDir()
	declared := map[string]string{"STRATA_NVCC": "/usr/local/cuda-13.0/bin/nvcc"}
	vars, err := New(func(string) map[string]string { return declared }).setupEnvironment(dir)
	require.NoError(t, err)
	joined := strings.Join(vars, "\n")
	require.NotContains(t, joined, "must-not-reach-upstream-setup", "zzRouter's own credentials stay out of third-party setup")
	require.Contains(t, vars, "STRATA_NVCC=/usr/local/cuda-13.0/bin/nvcc")
	pathPrefix := "PATH=" + filepath.Join(dir, ".venv", "bin") + string(os.PathListSeparator)
	require.True(t, strings.Contains(joined, "\n"+pathPrefix) || strings.HasPrefix(joined, pathPrefix), joined)

	_, err = New(func(string) map[string]string { return map[string]string{"PATH": "/tmp/evil"} }).setupEnvironment(dir)
	require.ErrorContains(t, err, "PATH", "provider config cannot replace the search path")

	t.Setenv("PATH", "")
	_, err = New(nil).setupEnvironment(dir)
	require.ErrorContains(t, err, "service PATH", "an empty PATH would search the checkout")
}

func TestRollbackSelectsVerifiedReleaseAndUninstallRetainsData(t *testing.T) {
	managedFixture(t)
	i := New(nil)
	require.NoError(t, os.WriteFile(previousVersionFile(), []byte("0.1.43\n"), 0600))
	require.ErrorContains(t, i.Rollback(context.Background()), "incomplete")
	require.Equal(t, "0.1.44.1", fsroot.ReadInstalledVersion("strata"))

	writeReleaseFixture(t, "0.1.43")
	require.NoError(t, i.Rollback(context.Background()))
	require.Equal(t, "0.1.43", fsroot.ReadInstalledVersion("strata"))

	plan, err := i.UninstallPlan(context.Background())
	require.NoError(t, err)
	require.Empty(t, plan.Steps[0].ServiceAction)
	require.NotContains(t, plan.Steps[0].Command, filepath.Join(fsroot.ProviderDir("strata"), "data"))
	require.NotContains(t, plan.Steps[0].Command, filepath.Join(fsroot.ProviderDir("strata"), "releases"))
}

func TestLauncherDriftUsesInstallVerification(t *testing.T) {
	managedFixture(t)
	plan := strataPlan(t, New(nil), "0.1.44.1")
	step := plan.Steps[7]
	verification := &install.Plan{Provider: "strata", Steps: []install.Step{step}}
	require.True(t, verification.VerifyAllContext(t.Context()).AllOK)
	require.NoError(t, os.WriteFile(launcherPath(), []byte("old launcher"), 0700))
	report := verification.VerifyAllContext(t.Context())
	require.False(t, report.AllOK)
	require.False(t, report.Steps[0].Passed)
	require.Contains(t, report.Steps[0].Message, "reinstall")
	_, err := New(nil).InstalledVersion()
	require.ErrorContains(t, err, "launcher differs from this zzRouter build; reinstall Strata")
	require.NoError(t, (&install.Plan{Steps: []install.Step{step}}).Execute(t.Context(), nil))
	require.True(t, verification.VerifyAllContext(t.Context()).AllOK)
}

func TestEngineVerificationKeepsPreviousRelease(t *testing.T) {
	for _, engine := range []string{"#!/bin/sh\necho qwen35moe\n", "#!/bin/sh\necho qwen4exp_extra\n", "#!/bin/sh\necho missing-library >&2\nexit 127\n"} {
		t.Run(engine, func(t *testing.T) {
			managedFixture(t)
			dir := writeReleaseFixture(t, "0.1.45")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "engine", "strata"), []byte(engine), 0700))
			mtpPythonFixture(t, dir)
			vocab := filepath.Join(fsroot.ProviderDir("strata"), "data", "mtp", "rt", "draft_vocab.bin")
			require.NoError(t, os.WriteFile(vocab, []byte("previous vocab"), 0600))
			plan := strataPlan(t, New(nil), "0.1.45")
			plan.Steps = plan.Steps[5:]
			require.Error(t, plan.Execute(t.Context(), nil))
			require.Equal(t, "0.1.44.1", fsroot.ReadInstalledVersion("strata"))
			body, err := os.ReadFile(vocab)
			require.NoError(t, err)
			require.Equal(t, "previous vocab", string(body))
		})
	}
}

func TestLauncherRefusesUnsupportedModel(t *testing.T) {
	managedFixture(t)
	dir := releaseDir("0.1.44.1")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tools"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tools", "gguf_reader.py"), []byte("class GGUFFile:\n    def __init__(self, path):\n        self.tensors = []\n"), 0600))
	choice, err := interpreter.Select(t.Context(), nil, interpreter.Options{})
	require.NoError(t, err)
	cmd := host.CommandContext(t.Context(), choice.Path, launcherPath(), "--model-path", filepath.Join(t.TempDir(), "other.gguf"), "--name", "other", "--port", "8100", "--")
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Equal(t, 2, cmd.ProcessState.ExitCode())
	require.Contains(t, string(output), "not a model Strata runs")
}

func TestMTPPreparationUsesSharedLayer(t *testing.T) {
	managedFixture(t)
	dir := releaseDir("0.1.44.1")
	mtpPythonFixture(t, dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tools"), 0700))
	tool := `import os, sys
from pathlib import Path
p = Path(sys.argv[sys.argv.index("--out") + 1])
with open("calls", "a") as log:
    log.write(Path(__file__).name + "\n")
if Path(__file__).name == "mtp_rt.py":
    p.mkdir(parents=True, exist_ok=True)
    (p / "experts.bin").write_text("mtp")
elif Path(__file__).name == "mtp_fetch.py":
    assert os.environ["STRATA_SOURCE"] == "huggingface"
    assert "STRATA_GGUF_PY" in os.environ
`
	for _, name := range []string{"mtp_fetch.py", "mtp_pack.py", "mtp_rt.py"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tools", name), []byte(tool), 0600))
	}
	plan := strataPlan(t, New(nil), "0.1.44.1")
	step := plan.Steps[6]
	require.NoError(t, (&install.Plan{Steps: []install.Step{step}}).Execute(t.Context(), nil))
	_, err := os.Stat(filepath.Join(dir, "calls"))
	require.True(t, os.IsNotExist(err), "an existing draft layer is reused")
	rt := filepath.Join(fsroot.ProviderDir("strata"), "data", "mtp", "rt")
	require.NoError(t, os.Remove(filepath.Join(rt, "experts.bin")))
	require.NoError(t, (&install.Plan{Steps: []install.Step{step}}).Execute(t.Context(), nil))
	body, err := os.ReadFile(filepath.Join(dir, "calls"))
	require.NoError(t, err)
	require.Equal(t, "mtp_fetch.py\nmtp_pack.py\nmtp_rt.py\n", string(body))
	vocab, err := os.ReadFile(filepath.Join(rt, "draft_vocab.bin"))
	require.NoError(t, err)
	require.Equal(t, "vocab", string(vocab))
}

func TestEngineVerificationActivatesSupportedRelease(t *testing.T) {
	managedFixture(t)
	dir := writeReleaseFixture(t, "0.1.45")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "engine", "strata"), []byte("#!/bin/sh\necho 'qwen4exp qwen35moe'\n"), 0700))
	mtpPythonFixture(t, dir)
	plan := strataPlan(t, New(nil), "0.1.45")
	plan.Steps = plan.Steps[5:]
	require.NoError(t, plan.Execute(t.Context(), nil))
	require.Equal(t, "0.1.45", fsroot.ReadInstalledVersion("strata"))
	require.True(t, New(nil).IsInstalled())
	vocab, err := os.ReadFile(filepath.Join(fsroot.ProviderDir("strata"), "data", "mtp", "rt", "draft_vocab.bin"))
	require.NoError(t, err)
	require.Equal(t, "vocab", string(vocab))
	previous, err := os.ReadFile(previousVersionFile())
	require.NoError(t, err)
	require.Equal(t, "0.1.44.1", strings.TrimSpace(string(previous)))
}

func mtpPythonFixture(t *testing.T, dir string) {
	t.Helper()
	choice, err := interpreter.Select(t.Context(), nil, interpreter.Options{})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, ".venv", "bin", "python")))
	require.NoError(t, os.Symlink(choice.Path, filepath.Join(dir, ".venv", "bin", "python")))
	setup := `from pathlib import Path

def model_source():
    return "huggingface"

def refresh_draft_vocab(rt):
    (rt / "draft_vocab.bin").write_text("vocab")
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "setup.py"), []byte(setup), 0600))
}
