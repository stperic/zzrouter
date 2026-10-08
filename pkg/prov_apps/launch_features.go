package prov_apps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/prov_apps/detect"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

// LocalizeModelFeatures supplies present file features without replacing any
// configured key, including an explicit auto or false opt-out.
func LocalizeModelFeatures(svc config.ServiceConfig, model string, params map[string]string) (Localized, error) {
	out := Localized{Params: params}
	hasFiles := false
	for _, f := range svc.Features {
		if len(f.Files) > 0 {
			hasFiles = true
		}
	}
	if !hasFiles || model == "" {
		return out, nil
	}
	dir, ok := process.ModelDirectory(svc, model)
	if !ok {
		return out, nil
	}
	manifest, err := integrity.ReadManifest(dir)
	if err != nil {
		return Localized{}, err
	}
	candidates, err := layout.OnDisk(dir)
	if err != nil {
		return Localized{}, err
	}
	digests := map[string]string{}
	if manifest != nil {
		for _, f := range manifest.Files {
			p := filepath.ToSlash(f.RelativePath)
			digests[p] = f.SHA256
			if f.Feature != "" {
				candidates = append(candidates, layout.File{Path: p, Size: f.Size})
			}
		}
	}
	slices.SortFunc(candidates, func(a, b layout.File) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})
	for _, name := range slices.Sorted(maps.Keys(svc.Features)) {
		f := svc.Features[name]
		if len(f.Files) == 0 {
			continue
		}
		if _, occupied := params[f.Flag]; occupied {
			continue
		}
		var present []layout.File
		for _, candidate := range candidates {
			p := filepath.Join(dir, filepath.FromSlash(candidate.Path))
			rel, err := filepath.Rel(dir, p)
			if err != nil || !fs.ValidPath(filepath.ToSlash(rel)) || strings.Contains(candidate.Path, "\\") {
				continue
			}
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			realDir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				continue
			}
			contained, err := filepath.Rel(realDir, real)
			if err != nil || !fs.ValidPath(filepath.ToSlash(contained)) {
				continue
			}
			if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Size() == candidate.Size {
				present = append(present, candidate)
			}
		}
		file, found := layout.FirstMatch(present, f.Files)
		if !found {
			continue
		}
		if out.Files == nil {
			out.Params = maps.Clone(params)
			if out.Params == nil {
				out.Params = map[string]string{}
			}
			out.Files = map[string]string{}
			out.Sources = map[string]string{}
		}
		out.Params[f.Flag] = filepath.Join(dir, filepath.FromSlash(file.Path))
		// Legacy unmanifested files have an unknown digest. Never hash large
		// projectors while resolving a view or listing run status.
		out.Files[f.Flag] = digests[file.Path]
		out.Sources[f.Flag] = "feature:" + name
	}
	return out, nil
}

func (m *ProviderAppManager) featureRuntime(svc config.ServiceConfig, model string, selected ...string) (config.ServiceConfig, string, []string, error) {
	runtimeKey := ""
	var excluded []string
	hasRuntime := false
	for _, f := range svc.Features {
		if f.Runtime != "" {
			hasRuntime = true
		}
	}
	if !hasRuntime {
		return svc, runtimeKey, excluded, nil
	}
	dir, local := process.ModelDirectory(svc, model)
	if !local {
		return svc, runtimeKey, excluded, nil
	}
	for _, name := range slices.Sorted(maps.Keys(svc.Features)) {
		feature := svc.Features[name]
		if feature.Runtime == "" {
			continue
		}
		explicit := ""
		if len(selected) > 0 {
			explicit = selected[0]
		}
		if explicit != "" && feature.Runtime != explicit {
			continue
		}
		installer, err := m.installs.Installer(feature.Runtime)
		if err != nil {
			return svc, "", nil, err
		}
		if !installer.IsInstalled() && explicit != feature.Runtime {
			continue
		}
		// Missing or unreadable optional evidence must not block the base runtime.
		qualifies, err := featurePredicate(dir, feature.When)
		if err != nil || !qualifies {
			continue
		}
		if runtimeKey != "" {
			return svc, "", nil, fmt.Errorf("multiple installed runtime features for model %q", model)
		}
		if feature.Execution == nil || svc.Runtime == nil {
			return svc, "", nil, fmt.Errorf("feature %q has no execution configuration", name)
		}
		runtimeCopy := *svc.Runtime
		runtimeCopy.Execution = *feature.Execution
		svc.Runtime = &runtimeCopy
		if svc.Capabilities != nil {
			caps := *svc.Capabilities
			caps.WireEndpoints = slices.Clone(feature.WireEndpoints)
			svc.Capabilities = &caps
		}
		runtimeKey = feature.Runtime
		excluded = feature.ExcludeParameters
	}
	return svc, runtimeKey, excluded, nil
}

func executionDigest(svc config.ServiceConfig, runtimeKey, command string) string {
	if svc.Runtime == nil {
		return ""
	}
	data, _ := json.Marshal(struct {
		Runtime       string
		Command       string
		Execution     config.ExecutionConfig
		WireEndpoints []string
	}{runtimeKey, command, svc.Runtime.Execution, wireEndpoints(svc)})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func wireEndpoints(svc config.ServiceConfig) []string {
	if svc.Capabilities == nil {
		return nil
	}
	return slices.Clone(svc.Capabilities.WireEndpoints)
}

// LaunchParameters is the node's effective launch configuration before argv
// construction. Preview and live preparation share this resolution.
type LaunchParameters struct {
	autoMemory           string
	ExecutionEnvironment process.EnvironmentTransform
	Executable           string
	Params               map[string]string
	Environment          map[string]string
	Files                map[string]string
	Sources              map[string]string
	Config               config.ServiceConfig
	RuntimeKey           string
}

func (m *ProviderAppManager) modelLaunchParameters(svc config.ServiceConfig, req LaunchRequest, endpoint string) (LaunchParameters, error) {
	if req.Runtime != "" && req.DisposablePlanID == "" && !req.installSmoke {
		return LaunchParameters{}, fmt.Errorf("runtime selection requires disposable_plan_id")
	}
	params, env := m.resolveRawLaunchParams(svc, req, endpoint)
	selected := req.Runtime
	if selected == "" {
		selected = req.selectedRuntime
	}
	svc, key, excluded, err := m.featureRuntime(svc, req.Model, selected)
	if err != nil {
		return LaunchParameters{}, err
	}
	for _, name := range excluded {
		delete(params, name)
	}
	features, err := LocalizeModelFeatures(svc, req.Model, params)
	if err != nil {
		return LaunchParameters{}, err
	}
	if env == nil {
		env = map[string]string{}
	}
	runtime := svc.Name
	if key != "" {
		runtime = key
	}
	if req.selectedRuntime != "" && runtime != req.selectedRuntime {
		return LaunchParameters{}, fmt.Errorf("selected runtime changed while waiting; retry launch")
	}
	executable := ""
	var toolkit install.ToolkitSelection
	if req.DisposablePlanID != "" {
		if req.Runtime != "" && req.Runtime != runtime {
			return LaunchParameters{}, fmt.Errorf("selected runtime does not apply to this model")
		}
		manifest, err := install.ReadDisposableManifest(runtime, req.DisposablePlanID)
		if err != nil {
			return LaunchParameters{}, err
		}
		snapshot, err := m.resolveRecipe(req.Provider, runtime)
		if err != nil {
			return LaunchParameters{}, err
		}
		if snapshot.Fingerprint != manifest.Recipe.Fingerprint || snapshot.PolicyFingerprint != manifest.Recipe.PolicyFingerprint || install.Fingerprint(m.providerServiceEnv(req.Provider)) != manifest.RuntimeEnvironmentFingerprint {
			return LaunchParameters{}, install.ErrStalePlan
		}
		if err := install.CheckRecipeAuthority(context.Background(), snapshot, m.resolveRecipe); err != nil {
			return LaunchParameters{}, err
		}
		venv := filepath.Join(install.DisposableDir(runtime, req.DisposablePlanID), "venv")
		toolkit = install.ToolkitEnvironmentAt(venv, manifest.Recipe.Recipe, manifest.PythonVersion)
		for key, value := range toolkit.Variables() {
			env[key] = value
		}
		if svc.Runtime == nil {
			return LaunchParameters{}, fmt.Errorf("runtime execution not declared")
		}
		subdir := "bin"
		if fsroot.CurrentPlatform().OS == "windows" {
			subdir = "Scripts"
		}
		command := svc.Runtime.Execution.Command
		if svc.Runtime.Execution.Type == "python" {
			command = "python"
			if subdir == "Scripts" {
				command = "python.exe"
			}
		}
		if filepath.Base(command) != command {
			return LaunchParameters{}, fmt.Errorf("runtime entrypoint is not a managed executable")
		}
		executable = filepath.Join(venv, subdir, command)
		if _, err := os.Stat(executable); err != nil {
			return LaunchParameters{}, fmt.Errorf("disposable entrypoint missing: %w", err)
		}
	} else {
		var err error
		toolkit, err = install.ApplyToolkitEnvironment(context.Background(), env, runtime)
		if err != nil {
			return LaunchParameters{}, err
		}
	}
	params, autoMemory, err := m.filterLaunchAuto(req.Provider, features.Params)
	if err != nil {
		return LaunchParameters{}, err
	}
	return LaunchParameters{autoMemory: autoMemory, ExecutionEnvironment: toolkit.Compose, Executable: executable, Params: params, Environment: env, Files: features.Files, Sources: features.Sources, Config: svc, RuntimeKey: key}, nil
}

// ResolveLaunchParameters resolves this node's tiers and selected feature
// runtime for previews without starting a process or installing anything.
func (m *ProviderAppManager) ResolveLaunchParameters(req LaunchRequest) (LaunchParameters, error) {
	_, svc, ok := m.resolveConfigKey(req.Provider)
	if !ok {
		return LaunchParameters{}, ErrProviderNotFound
	}
	return m.modelLaunchParameters(svc, req, string(EndpointOrDefault(req.Endpoint)))
}

// ExecutionCommand locates the effective managed executable for this launch.
func (p LaunchParameters) ExecutionCommand(provider string) string {
	if p.Executable != "" {
		return p.Executable
	}
	key := provider
	if p.RuntimeKey != "" {
		key = p.RuntimeKey
	}
	if p.Config.Runtime == nil {
		return ""
	}
	switch p.Config.Runtime.Execution.Type {
	case "python":
		return detect.ProviderPython(key, &p.Config)
	case "cli":
		return detect.ProviderCLI(key, &p.Config)
	default:
		return p.Config.Runtime.Execution.Command
	}
}
