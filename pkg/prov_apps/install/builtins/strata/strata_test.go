package strata

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
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
	cfg, err := json.Marshal(modelConfig{Exe: filepath.Join(dir, "engine", "strata"), Port: 8080})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "strata-iq3_s.json"), cfg, 0600))
	return dir
}

func TestManagedCommandUsesGeneratedConfigAndOwnedEngine(t *testing.T) {
	dir := managedFixture(t)
	i := New(nil)
	require.True(t, i.IsInstalled())
	require.Equal(t, filepath.Join(dir, "engine", "strata"), i.ManagedBinary())
	python, args := i.DaemonCommand()
	require.Equal(t, filepath.Join(dir, ".venv", "bin", "python"), python)
	require.Equal(t, []string{filepath.Join(dir, "serve", "server.py"), "--engine", "strata", "--config", filepath.Join(dir, "strata-iq3_s.json"), "--port", "8080"}, args)

	outside := filepath.Join(t.TempDir(), "foreign-engine")
	require.NoError(t, os.WriteFile(outside, []byte("foreign"), 0700))
	cfg, err := json.Marshal(modelConfig{Exe: outside, Port: 8080})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "strata-iq3_s.json"), cfg, 0600))
	require.False(t, i.IsInstalled())
	require.Empty(t, i.ManagedBinary())
	python, _ = i.DaemonCommand()
	require.Empty(t, python)
}

func TestInstallPlanPinsNikoReleaseAndUsesManagedLifecycle(t *testing.T) {
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
	apps, err := config.LoadAppsConfig(root)
	require.NoError(t, err)
	oldLoader := install.LoadAppsConfig
	install.LoadAppsConfig = func() (*config.AppsConfig, error) { return apps, nil }
	t.Cleanup(func() { install.LoadAppsConfig = oldLoader })
	i := New(nil)
	i.latestRelease = func(context.Context, string) (string, error) { return "0.1.44.1", nil }
	plan, err := i.InstallPlan(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "0.1.44.1", plan.Version)
	require.Contains(t, plan.Steps[1].Command, repositoryURL)
	require.Contains(t, plan.Steps[1].Command, "v0.1.44.1")
	var setupCommand string
	var setupEnv []string
	var stopped bool
	for _, step := range plan.Steps {
		if strings.Contains(step.Description, "Run Strata setup") {
			setupCommand = step.Command
			setupEnv = step.ExecutionEnvironment
		}
		if step.ServiceAction == install.ServiceStop {
			stopped = true
		}
	}
	require.Contains(t, setupCommand, "--no-start")
	require.Contains(t, setupCommand, "--data-dir")
	require.Contains(t, setupCommand, "--vision no")
	require.NotContains(t, setupCommand, "--family", "setup picks its recommended family")
	require.Contains(t, strings.Join(setupEnv, "\n"), "PATH="+filepath.Join(releaseDir(plan.Version), ".venv", "bin")+string(os.PathListSeparator))
	require.True(t, stopped)
	require.Equal(t, install.ServiceStart, plan.Steps[len(plan.Steps)-1].ServiceAction)
	for _, step := range plan.Steps {
		require.NotContains(t, step.Command, "mehbul")
	}
	_, err = i.InstallPlan(context.Background(), "..")
	require.ErrorContains(t, err, "numeric version")

	apps.GetExternal("strata").Features = map[string]config.Feature{"vision": {}}
	plan, err = i.InstallPlan(context.Background(), "")
	require.NoError(t, err)
	vision := false
	for _, step := range plan.Steps {
		vision = vision || strings.Contains(step.Command, "--vision yes")
	}
	require.True(t, vision, "a template that declares vision installs the image encoder")
}

func TestShippedStrataTemplateNamesTheIntendedProject(t *testing.T) {
	body, err := templates.AppsFS.ReadFile("files/providers/external/strata/config.yaml")
	require.NoError(t, err)
	var provider config.ExternalProvider
	require.NoError(t, yaml.Unmarshal(body, &provider))
	require.Equal(t, repository, provider.VersionSource.Repo)
	require.Equal(t, config.VersionSourceGitHubRelease, provider.VersionSource.Type)
	require.Equal(t, config.ComparePEP440, provider.VersionSource.Compare)
	require.NotNil(t, provider.Enabled)
	require.False(t, *provider.Enabled, "the template ships disabled")
	require.True(t, strings.HasPrefix(provider.Runtime.Endpoint, "http://127.0.0.1:"))
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
	require.Equal(t, install.ServiceStop, plan.Steps[0].ServiceAction)
	require.NotContains(t, plan.Steps[1].Command, filepath.Join(fsroot.ProviderDir("strata"), "data"))
	require.NotContains(t, plan.Steps[1].Command, filepath.Join(fsroot.ProviderDir("strata"), "releases"))
}
