// Package builtins wires the three built-in provider installers (Ollama,
// llama.cpp, vLLM, MLX — the last two share the pythonvenv installer with
// different configs) into a fresh install.Dispatcher. It's the single
// call site that knows which concrete installers exist; install root
// declares Dispatcher + ProviderInstaller but has no knowledge of
// individual providers. Callers that want a ready-to-use dispatcher call
// builtins.NewDispatcher; callers that want a bare dispatcher (e.g. a
// test registering only a fake installer) call install.NewDispatcher.
package builtins

import (
	"fmt"

	"github.com/stperic/zzrouter/pkg/config"
	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"

	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/builtins/llamacpp"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/builtins/ollama"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/builtins/pythonvenv"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// NewDispatcher returns an install.Dispatcher pre-populated with the
// built-in provider installers matched to the current platform.
func NewDispatcher(serviceEnv install.ServiceEnvFunc, checks install.RuntimeChecksResolver, recipes ...install.RecipeResolver) *install.Dispatcher {
	d := install.NewDispatcher()
	register(d, serviceEnv, checks, recipes...)
	return d
}

// Register adds the built-in provider installers to an existing dispatcher.
// Tests that want an empty dispatcher plus a curated subset of builtins
// call install.NewDispatcher and pick which installers to register.
//
// All installers register unconditionally. Platform gating is YAML-driven
// via ServiceConfig.Platforms at the coordinator and the same embedded
// declarations inside each managed Python runtime's installer.
//
// serviceEnv is wired only into installers that start a long-lived process
// from their plan; on-demand providers get their environment from the
// instance launcher instead. May be nil (tests, and callers with no config
// loaded yet).
func Register(d *install.Dispatcher, serviceEnv install.ServiceEnvFunc) { register(d, serviceEnv, nil) }

func register(d *install.Dispatcher, serviceEnv install.ServiceEnvFunc, checks install.RuntimeChecksResolver, recipes ...install.RecipeResolver) {
	if checks == nil {
		checks = shippedRuntimeChecks
	}
	d.Register("ollama", ollama.New(serviceEnv))
	d.Register("llamacpp", llamacpp.New())

	resolver := shippedRecipe
	if len(recipes) > 0 && recipes[0] != nil {
		resolver = recipes[0]
	}
	entries, err := templates.AppsFS.ReadDir("files/providers/on-demand")
	if err != nil {
		return
	}
	for _, entry := range entries {
		provider := entry.Name()
		data, err := templates.AppsFS.ReadFile("files/providers/on-demand/" + provider + "/config.yaml")
		if err != nil {
			continue
		}
		var declared config.OnDemandProvider
		if yaml.Unmarshal(data, &declared) != nil || declared.Install == nil {
			continue
		}
		platforms := make([]fsroot.Platform, 0, len(declared.Platforms))
		for _, platform := range declared.Platforms {
			platforms = append(platforms, fsroot.Platform{OS: platform.OS, Arch: platform.Arch})
		}
		for runtime := range declared.Install.Runtimes {
			d.Register(runtime, pythonvenv.New(pythonvenv.Config{Name: runtime, SchemaProvider: provider, Recipe: resolver, Checks: checks, Environment: serviceEnv, Platforms: platforms}))
		}
	}

}

func shippedRuntimeChecks(provider, runtime string) (schema.RuntimeChecks, error) {
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/" + provider + "/schema.yaml")
	if err != nil {
		return schema.RuntimeChecks{}, err
	}
	declared, err := schema.LoadYAMLSchema(data)
	if err != nil {
		return schema.RuntimeChecks{}, err
	}
	if err := declared.Diagnostics.Validate(); err != nil {
		return schema.RuntimeChecks{}, err
	}
	if declared.Diagnostics == nil {
		return schema.RuntimeChecks{}, fmt.Errorf("runtime diagnostics not declared")
	}
	checks, ok := declared.Diagnostics.Runtimes[runtime]
	if !ok {
		return schema.RuntimeChecks{}, fmt.Errorf("runtime %q not declared", runtime)
	}
	return checks, nil
}

func shippedRecipe(provider, runtime string) (install.RecipeSnapshot, error) {
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/" + provider + "/config.yaml")
	if err != nil {
		return install.RecipeSnapshot{}, err
	}
	var p config.OnDemandProvider
	if err := yaml.Unmarshal(data, &p); err != nil {
		return install.RecipeSnapshot{}, err
	}
	sc := config.ServiceConfig{Name: provider, Install: p.Install, Requirements: p.Requirements, Runtime: &config.AppRuntimeConfig{Execution: p.Runtime.Execution}, Features: p.Features, PinnedVersion: p.PinnedVersion, VersionSource: p.VersionSource}
	return install.ResolveRecipe(sc, "", runtime, "")
}
