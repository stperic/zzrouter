package config

import (
	"os"
	"path/filepath"
	"testing"

	configschema "github.com/stperic/zzrouter/pkg/config/schema"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func modelStoreProvider() *OnDemandProvider {
	return &OnDemandProvider{
		Name: "studio", RunScope: RunScopeNode,
		Runtime: OnDemandRuntime{
			DataDir: "data", ModelStore: &ModelStoreConfig{Directory: "models", Categories: []string{"vae", "text_encoders"}},
			PortRange: []int{8188, 8188}, BasePort: 8188,
			Execution: ExecutionConfig{Type: "python", Command: "python", WorkingDir: "source", Args: []string{"--base-directory", PlaceholderDataDir}},
		},
	}
}

func TestModelStoreValidation(t *testing.T) {
	require.NoError(t, modelStoreProvider().Validate())
	for name, edit := range map[string]func(*OnDemandProvider){
		"missing data":     func(p *OnDemandProvider) { p.Runtime.DataDir = "" },
		"wrong scope":      func(p *OnDemandProvider) { p.RunScope = "model" },
		"empty categories": func(p *OnDemandProvider) { p.Runtime.ModelStore.Categories = nil },
		"duplicate":        func(p *OnDemandProvider) { p.Runtime.ModelStore.Categories = []string{"vae", "vae"} },
		"nested category":  func(p *OnDemandProvider) { p.Runtime.ModelStore.Categories = []string{"vae/other"} },
		"uppercase":        func(p *OnDemandProvider) { p.Runtime.ModelStore.Categories = []string{"VAE"} },
		"empty category":   func(p *OnDemandProvider) { p.Runtime.ModelStore.Categories = []string{""} },
		"working ancestor": func(p *OnDemandProvider) { p.Runtime.DataDir, p.Runtime.Execution.WorkingDir = "code", "code/source" },
	} {
		t.Run(name, func(t *testing.T) {
			p := modelStoreProvider()
			edit(p)
			require.Error(t, p.Validate())
		})
	}
	for _, dir := range []string{"", ".", "..", "../data", "/tmp/data", "C:/data", `data\folder`, "data//other", "data/../other", "data\x00", "data\nchild", "${HF_HOME}"} {
		t.Run("directory "+dir, func(t *testing.T) {
			p := modelStoreProvider()
			p.Runtime.DataDir = dir
			require.Error(t, p.Validate())
			p = modelStoreProvider()
			p.Runtime.ModelStore.Directory = dir
			require.Error(t, p.Validate())
		})
	}
	for _, dir := range []string{"source", "source/cache", "venv", "bin/assets", "Source/cache"} {
		p := modelStoreProvider()
		p.Runtime.DataDir = dir
		require.ErrorContains(t, p.Validate(), "overlaps")
	}
}

func TestModelStoreDataDirUsesDeclaredWorkingDirectory(t *testing.T) {
	p := modelStoreProvider()
	p.Runtime.Execution.WorkingDir = "engine"
	p.Runtime.DataDir = "source"
	require.NoError(t, p.Validate())
	p.Runtime.DataDir = "engine/cache"
	require.ErrorContains(t, p.Validate(), "overlaps")
}

func TestModelStoreDataDirPlaceholderRequiresDeclaration(t *testing.T) {
	p := modelStoreProvider()
	p.Runtime.DataDir, p.Runtime.ModelStore = "", nil
	require.ErrorContains(t, p.Validate(), "${DATA_DIR} requires runtime.data_dir")
	cfg := &AppsConfig{}
	require.ErrorContains(t, cfg.AddApp(p.Name, providerToServiceConfig(p)), "${DATA_DIR} requires runtime.data_dir")
	require.Nil(t, cfg.Find(p.Name))
}

func TestModelStoreCannotDisappearAcrossKindConversion(t *testing.T) {
	for _, mode := range []string{"external", "service", "cloud", "registry"} {
		t.Run(mode, func(t *testing.T) {
			for _, dataOnly := range []bool{false, true} {
				cfg := &AppsConfig{}
				svc := providerToServiceConfig(modelStoreProvider())
				if dataOnly {
					svc.Runtime.ModelStore = nil
				}
				svc.Mode = mode
				require.ErrorContains(t, cfg.AddApp("studio", svc), "on-demand ownership")
				require.Nil(t, cfg.Find("studio"))
			}
			cfg := &AppsConfig{}
			require.NoError(t, cfg.AddApp("studio", providerToServiceConfig(modelStoreProvider())))
			require.ErrorContains(t, cfg.UpdateApp("studio", func(s *ServiceConfig) error { s.Mode = mode; return nil }), "on-demand ownership")
			require.Equal(t, modelStoreProvider(), cfg.GetOnDemand("studio"))
		})
	}
}

func TestModelStoreStrictSchema(t *testing.T) {
	raw, err := yaml.Marshal(modelStoreProvider())
	require.NoError(t, err)
	require.NoError(t, configschema.ValidateOnDemand(raw))
	for name, edit := range map[string]func(map[string]any, map[string]any){
		"unknown runtime field": func(_ map[string]any, r map[string]any) { r["data_path"] = "data" },
		"unknown store field": func(_ map[string]any, r map[string]any) {
			r["model_store"].(map[string]any)["workflow_loaders"] = map[string]any{}
		},
		"no data":   func(_ map[string]any, r map[string]any) { delete(r, "data_dir") },
		"no scope":  func(d map[string]any, _ map[string]any) { delete(d, "run_scope") },
		"bad scope": func(d map[string]any, _ map[string]any) { d["run_scope"] = "model" },
		"no directory": func(_ map[string]any, r map[string]any) {
			delete(r["model_store"].(map[string]any), "directory")
		},
		"duplicate categories": func(_ map[string]any, r map[string]any) {
			r["model_store"].(map[string]any)["categories"] = []string{"vae", "vae"}
		},
		"bad category": func(_ map[string]any, r map[string]any) {
			r["model_store"].(map[string]any)["categories"] = []string{"../vae"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var doc map[string]any
			require.NoError(t, yaml.Unmarshal(raw, &doc))
			edit(doc, doc["runtime"].(map[string]any))
			body, err := yaml.Marshal(doc)
			require.NoError(t, err)
			_, err = DecodeProviderBytes(KindOnDemand, "studio", body)
			require.Error(t, err)
		})
	}
	for _, kind := range []Kind{KindExternal, KindCloud, KindRegistry} {
		_, err := DecodeProviderBytes(kind, "studio", raw)
		require.Error(t, err)
	}
}

func TestModelStoreUpdateSaveReconcileAndSync(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	p := modelStoreProvider()
	view := providerToServiceConfig(p)
	converted, err := serviceConfigToProvider(p.Name, view)
	require.NoError(t, err)
	require.Equal(t, p, converted)
	require.NoError(t, cfg.AddApp(p.Name, view))
	require.NoError(t, cfg.UpdateApp(p.Name, func(s *ServiceConfig) error {
		s.Description, s.Runtime.DataDir = "updated", "storage/nested"
		return nil
	}))
	require.NoError(t, cfg.UpdateApp("comfyui", func(s *ServiceConfig) error {
		s.Enabled = new(true)
		s.Defaults.Parameters["gpu-reserve-mib"] = "16384"
		s.Runtime.DataDir, s.Runtime.ModelStore = "", nil
		s.Runtime.Execution.Args = []string{"main.py", "--port", PlaceholderPort, "--base-directory", "../data"}
		return nil
	}))
	require.NoError(t, cfg.SaveToDir(dir))
	changed, err := templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	require.Contains(t, changed, "on-demand/comfyui/config.yaml")
	fresh, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	studio := fresh.GetOnDemand(p.Name)
	require.Equal(t, "storage/nested", studio.Runtime.DataDir)
	require.Equal(t, p.Runtime.ModelStore, studio.Runtime.ModelStore)
	comfy := fresh.GetOnDemand("comfyui")
	require.True(t, *comfy.Enabled)
	require.Equal(t, "16384", comfy.Defaults.Parameters["gpu-reserve-mib"])
	require.Equal(t, "data", comfy.Runtime.DataDir)
	require.Equal(t, []string{"diffusion_models", "text_encoders", "vae", "loras", "checkpoints", "controlnet", "clip_vision", "upscale_models", "embeddings", "audio_encoders", "model_patches", "latent_upscale_models"}, comfy.Runtime.ModelStore.Categories)

	body, err := os.ReadFile(filepath.Join(dir, "on-demand", p.Name, "config.yaml"))
	require.NoError(t, err)
	workerDir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(workerDir))
	worker, err := NewAppsConfigStore(workerDir)
	require.NoError(t, err)
	_, err = worker.WriteProviderFiles(p.Name, KindOnDemand, ProviderFiles{Config: body})
	require.NoError(t, err)
	require.Equal(t, studio, worker.Config().GetOnDemand(p.Name))
	freshWorker, err := LoadAppsConfig(workerDir)
	require.NoError(t, err)
	require.Equal(t, studio, freshWorker.GetOnDemand(p.Name))
}
