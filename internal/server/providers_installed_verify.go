package server

import (
	"context"
	"fmt"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
	"gopkg.in/yaml.v3"
)

func (e *ProvidersExecutor) verifyInstalledRuntime(ctx context.Context, provider, runtime string, installer install.ProviderInstaller) (*install.VerifyResult, error) {
	python, ok := installer.(install.ManagedInterpreter)
	if !ok {
		return nil, fmt.Errorf("runtime has no managed interpreter")
	}
	data, err := e.configStore.ReadProviderSchemaBytes(provider)
	if err != nil {
		return nil, err
	}
	declared, err := schema.LoadYAMLSchema(data)
	if err != nil {
		return nil, err
	}
	if declared.Diagnostics == nil {
		return nil, fmt.Errorf("runtime diagnostics unavailable")
	}
	checks, ok := declared.Diagnostics.Runtimes[runtime]
	if !ok {
		return nil, fmt.Errorf("runtime diagnostics not declared")
	}
	manifest, err := fsroot.ReadInstallManifest(runtime)
	if err != nil {
		return nil, err
	}
	cfg, _ := e.appsConfig().LookupApp(provider)
	environment := config.FlattenEnvironment(cfg.Resolve(e.nodeName, "").Environment)
	if environment == nil {
		environment = map[string]string{}
	}
	if err := install.ValidateRuntimeEnvironment(environment); err != nil {
		return nil, err
	}
	snapshot, err := e.installedRecipeAuthority(provider, runtime, manifest)
	if err != nil {
		return nil, err
	}
	checks = snapshot.RuntimeChecks(checks)
	toolkit, err := install.ApplyToolkitEnvironment(ctx, environment, runtime)
	if err != nil {
		return nil, err
	}
	controlled, err := install.ControlledEnvironment()
	if err != nil {
		return nil, err
	}
	guard := func(ctx context.Context) error { return install.CheckRecipeAuthority(ctx, snapshot, nil) }
	plan := install.Plan{Provider: runtime, Version: fsroot.ReadInstalledVersion(runtime), Steps: []install.Step{{Number: 1, Description: "Verify installed runtime independently of desired version constraints", Verify: install.StepVerify{Type: "runtime_checks", Python: python.PythonPath(), RuntimeChecks: &checks, Environment: environment, ExecutionEnvironment: controlled, PreVerify: guard, Toolkit: toolkit}}}}
	return plan.VerifyAllContext(ctx), nil
}

// Installed diagnostics import only the actual recipe under current node authority.
// Desired overlays are drift data, not permission to import an installed module.
func (e *ProvidersExecutor) installedRecipeAuthority(provider, runtime string, manifest *fsroot.InstallManifest) (install.RecipeSnapshot, error) {
	cfg, _ := e.appsConfig().LookupApp(provider)
	actual, err := cloneService(&cfg)
	if err != nil {
		return install.RecipeSnapshot{}, err
	}
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/" + provider + "/config.yaml")
	if err != nil {
		return install.RecipeSnapshot{}, err
	}
	var release config.OnDemandProvider
	if err := yaml.Unmarshal(data, &release); err != nil {
		return install.RecipeSnapshot{}, err
	}
	actual.Install = release.Install
	if actual.Defaults == nil {
		actual.Defaults = &config.AppDefaultsConfig{}
	}
	actual.Defaults.Install = nil
	for node, spec := range actual.Nodes {
		spec.Install = nil
		actual.Nodes[node] = spec
	}
	if manifest != nil && manifest.Recipe != nil {
		installed := &config.InstallConfig{Runtimes: map[string]config.InstallRecipe{runtime: *manifest.Recipe}}
		actual.Install = installed
		// An operator-approved restatement still requires operator authority.
		if manifest.PolicyFingerprint != "" {
			actual.Defaults.Install = installed
		}
	}
	snapshot, err := e.mgr.CheckInstallAuthority(actual, runtime)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Policy != nil {
		if err := checkInstalledRootGrants(snapshot, manifest); err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
}

func checkInstalledRootGrants(snapshot install.RecipeSnapshot, manifest *fsroot.InstallManifest) error {
	if manifest == nil || len(manifest.Inventory) == 0 {
		return fmt.Errorf("%w: installed operator recipe lacks dependency inventory", install.ErrInstallPolicy)
	}
	roots := map[string]bool{*snapshot.Recipe.Package: false}
	for name := range snapshot.Recipe.Companions {
		roots[name] = false
	}
	for _, entry := range manifest.Inventory {
		if !entry.Requested {
			continue
		}
		constraint, granted := snapshot.Policy.Packages[entry.Name]
		if _, declared := roots[entry.Name]; !declared || !granted {
			return fmt.Errorf("%w: installed root %s is not granted", install.ErrInstallPolicy, entry.Name)
		}
		if err := upstream.VersionAllowed(entry.Version, constraint); err != nil {
			return fmt.Errorf("%w: installed root %s: %v", install.ErrInstallPolicy, entry.Name, err)
		}
		roots[entry.Name] = true
	}
	for name, present := range roots {
		if !present {
			return fmt.Errorf("%w: installed root %s lacks requested inventory", install.ErrInstallPolicy, name)
		}
	}
	return nil
}
