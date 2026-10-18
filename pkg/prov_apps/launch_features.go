package prov_apps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/prov_apps/detect"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
)

// LocalizeModelFeatures supplies present file features without replacing any
// configured key, including an explicit auto or false opt-out.
func LocalizeModelFeatures(svc config.ServiceConfig, model string, params map[string]string) (Localized, error) {
	return localizeModelFeatures(svc, process.ResolveModel(svc, model).Directory(), params)
}

func localizeModelFeatures(svc config.ServiceConfig, dir string, params map[string]string) (Localized, error) {
	out := Localized{Params: params}
	hasFiles := false
	for _, f := range svc.Features {
		if len(f.Files) > 0 {
			hasFiles = true
		}
	}
	if !hasFiles || dir == "" {
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

func (m *ProviderAppManager) featureRuntime(svc config.ServiceConfig, model process.ResolvedModel, selected, endpoint string) (config.ServiceConfig, string, error) {
	endpoint = string(EndpointOrDefault(Endpoint(endpoint)))
	runtimeKey := ""
	hasRuntime := false
	dir := model.Directory()
	for _, name := range slices.Sorted(maps.Keys(svc.Features)) {
		feature := svc.Features[name]
		hasRuntime = hasRuntime || feature.Runtime != ""
		if feature.Runtime == "" || dir == "" || config.FeatureEndpoint(name) != endpoint {
			continue
		}
		explicit := selected
		if explicit != "" && feature.Runtime != explicit {
			continue
		}
		qualifies, err := featurePredicate(dir, feature.When)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return svc, "", fmt.Errorf("%w: model %q feature %q: %w; repair or re-download malformed evidence; ask the operator to correct unreadable evidence", ErrFeatureEvidence, model.Name, name, err)
		}
		if !qualifies {
			continue
		}
		installer, err := m.installs.Installer(feature.Runtime)
		if err != nil {
			return svc, "", err
		}
		if !installer.IsInstalled() && explicit != feature.Runtime {
			continue
		}
		if runtimeKey != "" {
			return svc, "", fmt.Errorf("multiple installed runtime features for model %q", model)
		}
		if feature.Execution == nil || svc.Runtime == nil {
			return svc, "", fmt.Errorf("feature %q has no execution configuration", name)
		}
		runtimeCopy := *svc.Runtime
		runtimeCopy.Execution = *feature.Execution
		runtimeCopy.ExcludeParameters = feature.ExcludeParameters
		if feature.HealthCheck != nil {
			runtimeCopy.HealthCheck = *feature.HealthCheck
		}
		svc.Runtime = &runtimeCopy
		if svc.Capabilities != nil {
			caps := *svc.Capabilities
			caps.WireEndpoints = slices.Clone(feature.WireEndpoints)
			svc.Capabilities = &caps
		}
		runtimeKey = feature.Runtime
	}
	effective := svc
	effective.Features = nil
	if err := effective.ServesEndpoint(endpoint); hasRuntime && err != nil {
		for name, feature := range svc.Features {
			if installer, lookupErr := m.installs.Installer(feature.Runtime); config.FeatureEndpoint(name) == endpoint && feature.Runtime != "" && lookupErr == nil && !installer.IsInstalled() {
				return svc, "", fmt.Errorf("%w: %w; requires runtime %q; install with POST /zzrouter/v1/providers/%s/install {\"runtime\": \"%s\"}", ErrParameterValidation, err, feature.Runtime, svc.Name, feature.Runtime)
			}
		}
		return svc, "", fmt.Errorf("%w: %w", ErrParameterValidation, err)
	}
	return svc, runtimeKey, nil
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
		DataDir       string `json:",omitempty"`
	}{runtimeKey, command, svc.Runtime.Execution, wireEndpoints(svc), svc.Runtime.DataDir})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func wireEndpoints(svc config.ServiceConfig) []string {
	return svc.Capabilities.EffectiveWireEndpoints()
}

// LaunchParameters is the node's effective launch configuration before argv
// construction. Preview and live preparation share this resolution.
type LaunchParameters struct {
	parameterShapes      map[string]schema.ParamShape
	declared             *schema.ProviderSchema
	Commitments          map[string]int64
	model                process.ResolvedModel
	ExecutionEnvironment process.EnvironmentTransform
	Executable           string
	Params               map[string]string
	Environment          map[string]string
	Files                map[string]string
	Sources              map[string]string
	Config               config.ServiceConfig
	RuntimeKey           string
}

func (m *ProviderAppManager) modelLaunchParameters(ctx context.Context, svc config.ServiceConfig, model process.ResolvedModel, req EnsureRequest, endpoint string, declared *schema.ProviderSchema) (LaunchParameters, error) {
	if err := ctx.Err(); err != nil {
		return LaunchParameters{}, err
	}
	if err := svc.ValidatePlatform(runtime.GOOS, runtime.GOARCH); err != nil {
		return LaunchParameters{}, fmt.Errorf("%w: %w", ErrUnsupportedPlatform, err)
	}
	if req.Runtime != "" && req.DisposablePlanID == "" && req.Origin != OriginInstall {
		return LaunchParameters{}, fmt.Errorf("runtime selection requires disposable_plan_id")
	}
	resolved, env := m.resolveRawLaunchParams(svc, req, endpoint)
	selected := req.Runtime
	if selected == "" {
		selected = req.selectedRuntime
	}
	featureEndpoint := endpoint
	if req.Endpoint != "" {
		featureEndpoint = string(req.Endpoint)
	}
	svc, key, err := m.featureRuntime(svc, model, selected, featureEndpoint)
	if err != nil {
		return LaunchParameters{}, err
	}
	runtime := svc.Name
	if key != "" {
		runtime = key
	}
	if req.DisposablePlanID != "" && svc.Install != nil && svc.Install.Runtimes[runtime].Source != nil {
		return LaunchParameters{}, fmt.Errorf("source recipes do not support disposable launches")
	}
	excluded := dropExcluded(svc, resolved)
	if err := checkLaunchParameters(svc, runtime, declared.ForEndpoint(endpoint), excluded, resolved); err != nil {
		return LaunchParameters{}, err
	}
	params, sources := config.FlattenParameters(resolved), make(map[string]string, len(resolved))
	for key, value := range resolved {
		sources[key] = value.Tier.String()
	}
	features, err := localizeModelFeatures(svc, model.Directory(), params)
	if err != nil {
		return LaunchParameters{}, err
	}
	if env == nil {
		env = map[string]string{}
	}
	if req.selectedRuntime != "" && runtime != req.selectedRuntime {
		return LaunchParameters{}, fmt.Errorf("selected runtime changed while waiting; retry launch")
	}
	if req.Origin != OriginInstall && svc.Requirements != nil {
		_, managed := m.installs.managedInstaller(runtime)
		if !managed {
			if result, checked := preflight.GPU(gpu.WithInventory(ctx, gpu.List(false)), svc.Requirements.GPU); checked && !result.Passed {
				return LaunchParameters{}, fmt.Errorf("%w: provider %s: %s; %s", ErrUnsupportedPlatform, svc.Name, result.Message, result.Hint)
			}
		}
	}
	var executable string
	var toolkit install.ToolkitSelection
	if req.DisposablePlanID != "" {
		toolkit, executable, err = m.disposableLaunch(ctx, svc, req, runtime, env)
	} else {
		toolkit, err = install.ApplyToolkitEnvironment(ctx, env, runtime)
	}
	if err != nil {
		return LaunchParameters{}, err
	}
	maps.Copy(sources, features.Sources)
	params = filterLaunchAuto(declared, endpoint, features.Params)
	commitments := make(map[string]int64)
	budget := launchBudget(declared)
	if budget != nil && budget.Kind == schema.MemoryReserveMiB {
		if commitments, err = m.reservedCommitments(ctx, *budget, params, env); err != nil {
			return LaunchParameters{}, err
		}
	}
	freezeFeaturePaths(svc, params)
	return LaunchParameters{parameterShapes: declared.ForEndpoint(endpoint), declared: declared, Commitments: commitments, model: model, ExecutionEnvironment: toolkit.Compose, Executable: executable, Params: params, Environment: env, Files: features.Files, Sources: sources, Config: svc, RuntimeKey: key}, nil
}

// ParameterError is a launch refused for parameters its provider's schema
// does not accept, one issue per parameter.
type ParameterError struct {
	Provider string
	// ContextParameter is the provider's context length parameter, when it declares one.
	ContextParameter string
	// Served marks a provider whose daemon owns its model settings.
	Served bool
	Issues []utils.ParamError
}

func (e *ParameterError) Error() string {
	messages := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		messages[i] = issue.Message
	}
	return fmt.Sprintf("provider %s refuses %s", e.Provider, strings.Join(messages, "; "))
}

// dropExcluded drops the configured parameters svc's runtime excludes, a
// policy across engines, and returns its exclusions; a request's stays for
// the check to refuse, since it would not take effect.
func dropExcluded(svc config.ServiceConfig, resolved map[string]config.ResolvedValue) []string {
	if svc.Runtime == nil {
		return nil
	}
	for _, name := range svc.Runtime.ExcludeParameters {
		if value, set := resolved[name]; set && value.Tier != config.TierRequest {
			delete(resolved, name)
		}
	}
	return svc.Runtime.ExcludeParameters
}

// checkLaunchParameters refuses every parameter resolved passes that shapes,
// less what the runtime excludes, do not accept, naming each key as stored or
// sent and the tier that set it. A declared key the launch drops as auto is
// not passed, so not checked: even one the runtime lacks, since auto asks
// for nothing.
func checkLaunchParameters(svc config.ServiceConfig, runtime string, shapes map[string]schema.ParamShape, excluded []string, resolved map[string]config.ResolvedValue) error {
	params := config.FlattenParameters(resolved)
	var issues []utils.ParamError
	for _, key := range slices.Sorted(maps.Keys(params)) {
		at := resolved[key]
		shape, declared := shapes[key]
		var found []utils.ParamError
		switch {
		case len(at.Conflicts) > 0:
			found = schema.Duplicate(append([]string{at.Key}, at.Conflicts...))
		case declared && dropsAuto(key, params[key], shape):
			continue
		case declared && slices.Contains(excluded, key):
			found = []utils.ParamError{{Key: at.Key, Code: string(httperr.CodeExcludedByRuntime), Message: fmt.Sprintf("runtime %s does not take %s", runtime, key)}}
		default:
			if issue := schema.Check(key, params[key], shapes); issue != nil {
				issue.Key = at.Key
				found = []utils.ParamError{*issue}
			}
		}
		for _, issue := range found {
			issue.Message += fmt.Sprintf(" (%s tier, runtime %s)", at.Tier, runtime)
			issues = append(issues, issue)
		}
	}
	if issues == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrParameterValidation, &ParameterError{Provider: svc.Name, ContextParameter: svc.GetContextParam(), Served: Lifecycle(svc) == LifecycleServed, Issues: issues})
}

// disposableLaunch binds a launch to a disposable plan's venv once the plan
// still matches the recipe, policy and environment it was built from.
func (m *ProviderAppManager) disposableLaunch(ctx context.Context, svc config.ServiceConfig, req EnsureRequest, runtime string, env map[string]string) (install.ToolkitSelection, string, error) {
	if req.Runtime != "" && req.Runtime != runtime {
		return install.ToolkitSelection{}, "", fmt.Errorf("selected runtime does not apply to this model")
	}
	manifest, err := install.ReadDisposableManifest(runtime, req.DisposablePlanID)
	if err != nil {
		return install.ToolkitSelection{}, "", err
	}
	snapshot, err := m.resolveRecipe(req.Provider, runtime)
	if err != nil {
		return install.ToolkitSelection{}, "", err
	}
	if snapshot.Fingerprint != manifest.Recipe.Fingerprint || snapshot.PolicyFingerprint != manifest.Recipe.PolicyFingerprint || install.Fingerprint(m.providerServiceEnv(req.Provider)) != manifest.RuntimeEnvironmentFingerprint {
		return install.ToolkitSelection{}, "", install.ErrStalePlan
	}
	if err := install.CheckRecipeAuthority(ctx, snapshot, m.resolveRecipe); err != nil {
		return install.ToolkitSelection{}, "", err
	}
	venv := filepath.Join(install.DisposableDir(runtime, req.DisposablePlanID), "venv")
	toolkit := install.ToolkitEnvironmentAt(venv, manifest.Recipe.Recipe, manifest.PythonVersion)
	for key, value := range toolkit.Variables() {
		env[key] = value
	}
	if svc.Runtime == nil {
		return install.ToolkitSelection{}, "", fmt.Errorf("runtime execution not declared")
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
		return install.ToolkitSelection{}, "", fmt.Errorf("runtime entrypoint is not a managed executable")
	}
	executable := filepath.Join(venv, subdir, command)
	if _, err := os.Stat(executable); err != nil {
		return install.ToolkitSelection{}, "", fmt.Errorf("disposable entrypoint missing: %w", err)
	}
	return toolkit, executable, nil
}

// reservedCommitments sizes a reserve_mib budget against the visible NVIDIA
// devices and pins that visibility into env.
func (m *ProviderAppManager) reservedCommitments(ctx context.Context, budget schema.MemoryBudget, params, env map[string]string) (map[string]int64, error) {
	devices, err := m.memoryTotals(ctx)
	if err != nil {
		return nil, err
	}
	visible, err := visibleDeviceEnvironment(env)
	if err != nil {
		return nil, err
	}
	commitments, err := process.PrepareMemoryBudget(budget, params, visible, devices)
	if err != nil {
		return nil, err
	}
	maps.Copy(env, visible)
	return commitments, nil
}

func freezeFeaturePaths(svc config.ServiceConfig, params map[string]string) {
	for _, feature := range svc.Features {
		if path := params[feature.Flag]; len(feature.Files) > 0 && filepath.IsAbs(path) {
			params[feature.Flag] = layout.PhysicalPath(path)
		}
	}
}

// ResolveLaunchParameters resolves this node's tiers and selected feature
// runtime for previews without starting a process or installing anything.
func (m *ProviderAppManager) ResolveLaunchParameters(ctx context.Context, req EnsureRequest) (LaunchParameters, error) {
	_, svc, ok := m.resolveConfigKey(req.Provider)
	if !ok {
		return LaunchParameters{}, ErrProviderNotFound
	}
	declared, err := m.launchSchema(req.Provider)
	if err != nil {
		return LaunchParameters{}, err
	}
	return m.modelLaunchParameters(ctx, svc, process.ResolveModel(svc, req.Model), req, string(EndpointOrDefault(req.Endpoint)), declared)
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
