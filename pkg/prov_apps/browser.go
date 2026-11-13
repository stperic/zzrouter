package prov_apps

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/runsettings"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"gopkg.in/yaml.v3"
)

func (m *ProviderAppManager) captureBrowserReceipt(ctx context.Context, prepared preparedLaunch, port int) *instance.BrowserReceipt {
	svc := prepared.config
	if ctx.Err() != nil || svc.Runtime == nil || svc.Runtime.Browser == nil || svc.RunScope != config.RunScopeNode || prepared.resolved.Runtime != svc.Name {
		return nil
	}
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/" + svc.Name + "/config.yaml")
	if err != nil {
		return nil
	}
	var release config.OnDemandProvider
	if yaml.Unmarshal(data, &release) != nil || !reflect.DeepEqual(svc.Runtime.Browser, release.Runtime.Browser) || !reflect.DeepEqual(svc.Runtime.Execution, release.Runtime.Execution) || !reflect.DeepEqual(svc.Features, release.Features) {
		return nil
	}
	for key, expected := range release.Runtime.Browser.RequiredParameters {
		if actual, present := prepared.resolved.Parameters[key]; !present || actual != expected {
			return nil
		}
	}
	if !browserArgumentsProven(prepared) {
		return nil
	}
	runtime := prepared.resolved.Runtime
	if prepared.launch.Command != fsroot.ProviderVenvPython(runtime) || prepared.launch.WorkingDir != filepath.Join(fsroot.ProviderDir(runtime), release.Runtime.Execution.WorkingDir) {
		return nil
	}
	manifest := m.browserManifest(ctx, svc, runtime)
	if manifest == nil {
		return nil
	}
	return &instance.BrowserReceipt{Endpoint: processRunEndpoint(port), Name: release.Runtime.Browser.Name, InstallPlanID: manifest.PlanID, SourceSHA256: manifest.Recipe.Source.SHA256, RecipeFingerprint: manifest.RecipeFingerprint, Contract: install.Fingerprint(release.Runtime.Browser), Execution: install.Fingerprint(prepared.launch)}
}

// Only release-declared flags with proven scalar boundaries can establish shared access.
func browserArgumentsProven(prepared preparedLaunch) bool {
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/" + prepared.config.Name + "/schema.yaml")
	if err != nil {
		return false
	}
	definition, err := schema.LoadYAMLSchema(data)
	if err != nil {
		return false
	}
	released, problems := schema.Merge(prepared.config.Name, definition, config.ServiceConfig{})
	if len(problems) != 0 {
		return false
	}
	for key, value := range prepared.resolved.Parameters {
		name := runsettings.FlagName(key)
		if name == runsettings.KeepAliveParameter || name == runsettings.EvictIdleParameter {
			continue
		}
		actual, known := prepared.parameters.parameterShapes[name]
		curated, declared := released.Parameters[name]
		if !declared && known && actual.Kind == schema.ParamBool && value == "false" && !actual.Negatable {
			continue // The canonical renderer emits no argument for this inactive declaration.
		}
		if !known || !declared || schema.Check(key, value, released.Parameters) != nil || !browserScalarProven(actual, curated, value) {
			return false
		}
	}
	return true
}

func browserScalarProven(actual, released schema.ParamShape, value string) bool {
	if actual.Flag != released.Flag || actual.Kind != released.Kind || actual.Negatable != released.Negatable || actual.Pass != released.Pass || actual.Auto != released.Auto {
		return false
	}
	// These later transformations need proof of their emitted values before shared use.
	if actual.Kind == schema.ParamAsset || value == runsettings.Auto && actual.Auto != "" {
		return false
	}
	return len(value) < 2 || !strings.HasPrefix(value, "-") || actual.Kind == schema.ParamInt || actual.Kind == schema.ParamFloat
}

func (m *ProviderAppManager) browserManifest(ctx context.Context, svc config.ServiceConfig, runtime string) *fsroot.InstallManifest {
	if ctx.Err() != nil {
		return nil
	}
	installer, err := m.installs.Installer(runtime)
	if err != nil || !installer.IsInstalled() {
		return nil
	}
	manifest, err := fsroot.ReadInstallManifest(runtime)
	if err != nil || manifest == nil || manifest.Recipe == nil || manifest.Recipe.Source == nil {
		return nil
	}
	approved, err := m.CheckInstallAuthority(svc, runtime, manifest)
	if err != nil || approved.Authority != "release" || approved.Recipe.Source == nil || manifest.Recipe.Source.SHA256 != approved.Recipe.Source.SHA256 || manifest.RecipeFingerprint != approved.Fingerprint {
		return nil
	}
	return manifest
}

// BrowserRun returns a ready run and its immutable launch-owned browser endpoint.
func (m *ProviderAppManager) BrowserRun(ctx context.Context, id string) (*instance.Instance, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	run, ok := m.instances.Get(id)
	if !ok || run.GetStatus() != instance.StatusRunning || run.Context() == nil || run.Context().Err() != nil {
		return nil, "", ErrProviderUnavailable
	}
	resolved := run.Resolved()
	receipt := resolved.Browser
	if receipt == nil || receipt.Endpoint != processRunEndpoint(run.Port) {
		return nil, "", ErrProviderUnavailable
	}
	_, svc, ok := m.resolveConfigKey(run.Provider)
	if !ok {
		return nil, "", ErrProviderUnavailable
	}
	enabled := svc.EnabledOn(m.localNodename())
	if enabled == nil || !*enabled {
		return nil, "", ErrProviderUnavailable
	}
	if svc.Runtime == nil || svc.Runtime.Browser == nil || install.Fingerprint(svc.Runtime.Browser) != receipt.Contract {
		return nil, "", ErrProviderUnavailable
	}
	manifest := m.browserManifest(ctx, svc, resolved.Runtime)
	if manifest == nil || manifest.PlanID != receipt.InstallPlanID || manifest.RecipeFingerprint != receipt.RecipeFingerprint || manifest.Recipe.Source.SHA256 != receipt.SourceSHA256 || run.Context().Err() != nil {
		return nil, "", ErrProviderUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return run, receipt.Endpoint, nil
}
