package prov_apps

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

// Synthetic installed files isolate launch evidence. Archive/wheel integrity has separate real installer tests.
func browserLaunchFixture(t *testing.T) (*ProviderAppManager, preparedLaunch, *instance.Instance) {
	t.Helper()
	isolateProviderRoot(t)
	directory := t.TempDir()
	require.NoError(t, templates.InstallDefaults(directory))
	loaded, err := config.LoadAppsConfig(directory)
	require.NoError(t, err)
	svc, ok := loaded.LookupApp("comfyui")
	require.True(t, ok)
	svc.Platforms, svc.Requirements = nil, nil
	svc.Enabled = new(true)
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("comfyui", svc))
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/comfyui/schema.yaml")
	require.NoError(t, err)
	declared, err := schema.LoadYAMLSchema(data)
	require.NoError(t, err)
	m, err := NewProviderAppManager(cfg, WithProviderSchema(func(provider string) (*schema.ProviderSchema, error) {
		merged, errs := schema.Merge(provider, declared, config.ServiceConfig{})
		return merged, errors.Join(errs...)
	}))
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	m.memoryInventory = func(context.Context) []gpu.CardMetrics {
		return []gpu.CardMetrics{{UUID: "GPU-A", TotalMemoryMiB: 24576}}
	}
	m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 24576, FreeMiB: 24576}}, nil
	}
	snapshot, err := m.resolveRecipe("comfyui", "comfyui")
	require.NoError(t, err)
	manifest := &fsroot.InstallManifest{Version: snapshot.Recipe.Source.Version, WorkingDir: snapshot.WorkingDir, Recipe: &snapshot.Recipe, RecipeFingerprint: snapshot.Fingerprint, PlanID: "installed-browser-source", Interpreter: &fsroot.ManifestInterpreter{Path: "python", Version: "3.14.0"}, Inventory: []fsroot.ManifestPackage{{Name: "torch", Version: "2.9"}}}
	require.NoError(t, fsroot.WriteInstallManifest("comfyui", manifest))
	python := fsroot.ProviderVenvPython("comfyui")
	require.NoError(t, os.MkdirAll(filepath.Dir(python), 0700))
	require.NoError(t, os.WriteFile(python, []byte("fixture"), 0700))
	req := EnsureRequest{Provider: "comfyui", EnvVars: map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A"}}
	prepared, err := (processEngine{m}).prepare(t.Context(), svc, "comfyui", req, 8188)
	require.NoError(t, err)
	require.NotNil(t, prepared.resolved.Browser)
	run := instance.NewInstance("shared-browser", "comfyui", "", 8188, 0, 1)
	require.NoError(t, m.instances.Register(run))
	admitted, err := m.prepareRun(t.Context(), run, req, launchSelection{key: "comfyui", config: svc, model: process.ResolveModel(svc, "")}, requireLaunchSchema(t, m, "comfyui"))
	require.NoError(t, err)
	require.NotNil(t, run.Resolved().Browser, "actual launch preparation must retain its own receipt")
	require.Equal(t, install.Fingerprint(process.Launch{Command: admitted.cmd, Args: admitted.args, WorkingDir: run.Resolved().WorkingDir}), run.Resolved().Browser.Execution, "receipt binds the final memory-admitted argv")
	run.SetStatus(instance.StatusRunning)
	return m, prepared, run
}

func TestBrowserReceiptCapturesCanonicalSourceLaunch(t *testing.T) {
	m, prepared, run := browserLaunchFixture(t)
	receipt := prepared.resolved.Browser
	require.Equal(t, "ComfyUI", receipt.Name)
	require.Equal(t, "installed-browser-source", receipt.InstallPlanID)
	require.NotEmpty(t, receipt.SourceSHA256)
	require.NotEmpty(t, receipt.Execution)
	actual, endpoint, err := m.BrowserRun(t.Context(), run.ID)
	require.NoError(t, err)
	require.Same(t, run, actual)
	require.Equal(t, processRunEndpoint(run.Port), endpoint)
	view := run.Resolved()
	view.Browser.SourceSHA256 = "changed by reader"
	require.Equal(t, receipt.SourceSHA256, run.Resolved().Browser.SourceSHA256)
}

func TestBrowserReceiptRefusesUnsafeEffectiveLaunch(t *testing.T) {
	for _, scenario := range []string{"manager", "legacy manager", "agent", "custom nodes", "whitelist", "network listener", "hidden argv", "alternate interpreter"} {
		t.Run(scenario, func(t *testing.T) {
			m, prepared, _ := browserLaunchFixture(t)
			prepared.resolved.Parameters = maps.Clone(prepared.resolved.Parameters)
			switch scenario {
			case "manager":
				prepared.resolved.Parameters["enable-manager"] = "true"
			case "legacy manager":
				prepared.resolved.Parameters["enable-manager-legacy-ui"] = "true"
			case "agent":
				prepared.resolved.Parameters["enable-agent"] = "true"
			case "custom nodes":
				prepared.resolved.Parameters["disable-all-custom-nodes"] = "false"
			case "whitelist":
				prepared.resolved.Parameters["whitelist-custom-nodes"] = "custom-node"
			case "network listener":
				prepared.resolved.Parameters["listen"] = "0.0.0.0"
			case "hidden argv":
				runtime := *prepared.config.Runtime
				runtime.Execution.Args = append(slices.Clone(runtime.Execution.Args), "--enable-manager")
				prepared.config.Runtime = &runtime
			case "alternate interpreter":
				prepared.launch.Command = "/unmanaged/python"
			}
			require.Nil(t, m.captureBrowserReceipt(t.Context(), prepared, 8188))
		})
	}
}

func TestBrowserRunCannotQualifyOldUnsafeOrReplacedRuns(t *testing.T) {
	for _, scenario := range []string{"no receipt", "replaced install", "cancelled run", "stopped run"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, run := browserLaunchFixture(t)
			switch scenario {
			case "no receipt":
				resolved := run.Resolved()
				resolved.Browser = nil
				run.SetResolved(resolved)
			case "replaced install":
				manifest, err := fsroot.ReadInstallManifest("comfyui")
				require.NoError(t, err)
				manifest.PlanID = "replacement"
				require.NoError(t, fsroot.WriteInstallManifest("comfyui", manifest))
			case "cancelled run":
				run.Cancel()
			case "stopped run":
				run.SetStatus(instance.StatusStopped)
			}
			_, _, err := m.BrowserRun(t.Context(), run.ID)
			require.ErrorIs(t, err, ErrProviderUnavailable)
		})
	}
}

func TestBrowserReceiptRefusesUnprovenArgv(t *testing.T) {
	for _, test := range []struct {
		name, key, value, kind string
		baseSchema             bool
	}{
		{"operator value injection", "verbose", "--enable-manager", "string", false},
		{"operator flag abbreviation", "enable-manager-legacy-u", "true", "bool", false},
		{"released value injection", "cuda-device", "--enable-manager", "", false},
		{"modified base schema", "enable-manager-legacy-u", "true", "bool", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, prepared, run := browserLaunchFixture(t)
			svc := prepared.config
			data, err := templates.AppsFS.ReadFile("files/providers/on-demand/comfyui/schema.yaml")
			require.NoError(t, err)
			declared, err := schema.LoadYAMLSchema(data)
			require.NoError(t, err)
			if test.baseSchema {
				declared.Parameters[test.key] = schema.YAMLParam{Type: test.kind}
			} else if test.kind != "" {
				svc.Schema = map[string]config.ParameterDeclaration{test.key: {Type: &test.kind}}
			}
			m.providerSchema = func(provider string) (*schema.ProviderSchema, error) {
				merged, errs := schema.Merge(provider, declared, svc)
				return merged, errors.Join(errs...)
			}
			req := EnsureRequest{Provider: "comfyui", Parameters: map[string]string{test.key: test.value}, EnvVars: map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A"}}
			admitted, err := m.prepareRun(t.Context(), run, req, launchSelection{key: "comfyui", config: svc, model: process.ResolveModel(svc, "")}, requireLaunchSchema(t, m, "comfyui"))
			require.NoError(t, err, "private operator tuning remains supported")
			require.Contains(t, admitted.args, "--"+test.key)
			if test.value != "true" {
				require.Contains(t, admitted.args, test.value)
			}
			require.Equal(t, "false", run.Resolved().Parameters["enable-manager"])
			require.Nil(t, run.Resolved().Browser, "an unsafe upstream interpretation must not acquire shared browser access")
		})
	}
}

func TestBrowserReceiptAcceptsReleasedTuningAndInactiveOperatorFlag(t *testing.T) {
	m, prepared, run := browserLaunchFixture(t)
	svc := prepared.config
	svc.Schema = map[string]config.ParameterDeclaration{"enable-manager-legacy-u": {Type: new("bool")}}
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/comfyui/schema.yaml")
	require.NoError(t, err)
	declared, err := schema.LoadYAMLSchema(data)
	require.NoError(t, err)
	m.providerSchema = func(provider string) (*schema.ProviderSchema, error) {
		merged, errs := schema.Merge(provider, declared, svc)
		return merged, errors.Join(errs...)
	}
	req := EnsureRequest{Provider: "comfyui", Parameters: map[string]string{"enable-manager-legacy-u": "false", "default-device": "-1", "keep-alive": "-1", "max-upload-size": "100"}, EnvVars: map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A"}}
	admitted, err := m.prepareRun(t.Context(), run, req, launchSelection{key: "comfyui", config: svc, model: process.ResolveModel(svc, "")}, requireLaunchSchema(t, m, "comfyui"))
	require.NoError(t, err)
	require.NotContains(t, admitted.args, "--enable-manager-legacy-u")
	require.Contains(t, admitted.args, "--default-device")
	require.Contains(t, admitted.args, "-1")
	require.NotNil(t, run.Resolved().Browser)
}
