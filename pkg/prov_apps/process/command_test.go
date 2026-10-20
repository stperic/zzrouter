package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// withModelsRoot points the resolver at a temp models root holding the
// given model directories, and returns the root.
func withModelsRoot(t *testing.T, models ...string) string {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	for _, m := range models {
		require.NoError(t, os.MkdirAll(filepath.Join(root, m), 0o755))
	}
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	return root
}

func TestDeletionLaunchFreezesSymlinkContract(t *testing.T) {
	root := withModelsRoot(t, "org/model")
	a, b, alias := filepath.Join(root, "a.gguf"), filepath.Join(root, "b.gguf"), filepath.Join(root, "org/model/alias.gguf")
	for _, file := range []string{a, b} {
		require.NoError(t, os.WriteFile(file, []byte("GGUF"), 0600))
	}
	require.NoError(t, os.Symlink(a, alias))
	cfg := config.ServiceConfig{Runtime: &config.AppRuntimeConfig{Execution: config.ExecutionConfig{Command: "engine", Args: []string{"${MODEL_PATH}"}}}}
	selected := ResolveModel(cfg, "org/model#alias.gguf")
	held := selected.WeightPaths()
	require.Len(t, held, 1)
	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink(b, alias))
	launch, err := NewCommandBuilder(&cfg).Build(selected, 8080, nil, nil)
	require.NoError(t, err)
	require.Equal(t, held[0], launch.ModelPath, "argv must use the physical path held by the reservation")
	require.Equal(t, held[0], launch.Args[0])
}

func argAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("flag %q not found in %v", flag, args)
	return ""
}

// TestBuild_ModelPlaceholderStripsVariantSuffix locks the contract that
// ${MODEL} substitutes to the user-facing model name without the
// "#variant" suffix appended by the auto_deploy chain. Used by naming
// flags (llama-server --alias, vLLM --served-model-name) so the wire
// never shows the on-disk filename.
func TestBuild_ModelPlaceholderStripsVariantSuffix(t *testing.T) {
	withModelsRoot(t, "Qwen/Qwen2.5")

	cfg := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{
				Type:    "cli",
				Command: "llama-server",
				Args:    []string{"--alias", "${MODEL}", "--model", "${MODEL_PATH}"},
			},
		},
	}
	launch, err := NewCommandBuilder(cfg).Build(ResolveModel(*cfg, "Qwen/Qwen2.5#qwen2.5-q4.gguf"), 8080, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "Qwen/Qwen2.5", argAfter(t, launch.Args, "--alias"),
		"${MODEL} must strip the #variant suffix")
}

// TestBuild_PointsEngineAtLocalWeights is the regression guard for the
// class of bug where an engine is handed a bare model name: it then
// resolves the name against its own registry cache and downloads a second
// copy, leaving zzRouter's model store unused.
func TestBuild_PointsEngineAtLocalWeights(t *testing.T) {
	root := withModelsRoot(t, "mlx-community/Qwen3-4bit")

	cfg := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{
				Type:    "python",
				Command: "python3",
				Args:    []string{"-m", "mlx_lm", "server", "--model", "${MODEL_PATH}"},
			},
		},
	}
	launch, err := NewCommandBuilder(cfg).Build(ResolveModel(*cfg, "mlx-community/Qwen3-4bit"), 8090, nil, nil)
	require.NoError(t, err)

	want := filepath.Join(root, "mlx-community", "Qwen3-4bit")
	assert.Equal(t, want, launch.ModelPath)
	assert.Equal(t, want, argAfter(t, launch.Args, "--model"))
}

// TestBuild_MissingWeightsFailsLaunch locks that an unresolvable model is
// a launch failure here rather than an opaque engine error — or worse, a
// silent download by the engine.
func TestBuild_MissingWeightsFailsLaunch(t *testing.T) {
	withModelsRoot(t)

	cfg := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{
				Type:    "cli",
				Command: "vllm",
				Args:    []string{"serve", "${MODEL_PATH}"},
			},
		},
	}
	_, err := NewCommandBuilder(cfg).Build(ResolveModel(*cfg, "org/never-downloaded"), 8000, nil, nil)
	require.ErrorIs(t, err, ErrModelNotLocal)
}

// TestBuild_WireModel locks which token each engine is addressed by on the
// wire: the canonical name for engines with a naming flag, the weights
// path for engines that load whatever the request carries.
func TestBuild_WireModel(t *testing.T) {
	root := withModelsRoot(t, "org/model")

	newCfg := func(wire config.WireModel) *config.ServiceConfig {
		return &config.ServiceConfig{
			Runtime: &config.AppRuntimeConfig{
				Execution: config.ExecutionConfig{
					Type:      "cli",
					Command:   "engine",
					Args:      []string{"--model", "${MODEL_PATH}"},
					WireModel: wire,
				},
			},
		}
	}

	t.Run("name engines keep the canonical name", func(t *testing.T) {
		cfg := newCfg(config.WireModelName)
		launch, err := NewCommandBuilder(cfg).Build(ResolveModel(*cfg, "org/model"), 8000, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, "org/model", launch.WireModel)
	})

	t.Run("unset defaults to name", func(t *testing.T) {
		cfg := newCfg("")
		launch, err := NewCommandBuilder(cfg).Build(ResolveModel(*cfg, "org/model"), 8000, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, "org/model", launch.WireModel)
	})

	t.Run("path engines are addressed by path", func(t *testing.T) {
		cfg := newCfg(config.WireModelPath)
		launch, err := NewCommandBuilder(cfg).Build(ResolveModel(*cfg, "org/model"), 8000, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, "org", "model"), launch.WireModel)
	})
}

func TestBuildParameterArgs(t *testing.T) {
	t.Run("basic params", func(t *testing.T) {
		params := map[string]string{
			"max_model_len": "4096",
			"gpu_memory":    "0.9",
		}
		args := buildParameterArgs(params, nil, nil)
		assert.Contains(t, args, "--max-model-len")
		assert.Contains(t, args, "4096")
		assert.Contains(t, args, "--gpu-memory")
		assert.Contains(t, args, "0.9")
	})

	t.Run("boolean true", func(t *testing.T) {
		params := map[string]string{"trust_remote_code": "true"}
		args := buildParameterArgs(params, nil, nil)
		assert.Contains(t, args, "--trust-remote-code")
		assert.Len(t, args, 1) // no value for boolean true
	})

	t.Run("boolean false", func(t *testing.T) {
		params := map[string]string{"trust_remote_code": "false"}
		args := buildParameterArgs(params, nil, nil)
		assert.Empty(t, args) // false flags are skipped
	})

	t.Run("empty value", func(t *testing.T) {
		params := map[string]string{"verbose": ""}
		args := buildParameterArgs(params, nil, nil)
		assert.Contains(t, args, "--verbose")
		assert.Len(t, args, 1)
	})

	t.Run("excluded params", func(t *testing.T) {
		params := map[string]string{
			"keep": "yes",
			"skip": "no",
		}
		args := buildParameterArgs(params, nil, []string{"skip"})
		assert.Contains(t, args, "--keep")
		assert.NotContains(t, args, "--skip")
	})

	t.Run("nil params", func(t *testing.T) {
		args := buildParameterArgs(nil, nil, nil)
		assert.Nil(t, args)
	})

	t.Run("empty params", func(t *testing.T) {
		args := buildParameterArgs(map[string]string{}, nil, nil)
		assert.Nil(t, args)
	})

	t.Run("json value normalization", func(t *testing.T) {
		params := map[string]string{
			"config": `{"num_workers":"4","rate":"1.5"}`,
		}
		args := buildParameterArgs(params, nil, nil)
		assert.Contains(t, args, "--config")
		// Numeric strings should be converted to numbers
		assert.Contains(t, args, `{"num_workers":4,"rate":1.5}`)
	})

	t.Run("non-json value unchanged", func(t *testing.T) {
		params := map[string]string{"model": "llama3-8b"}
		args := buildParameterArgs(params, nil, nil)
		assert.Contains(t, args, "llama3-8b")
	})
}

// One key carries a negatable bool both ways; a plain bool's false stays
// unset, since the engine has no spelling for it.
func TestBuildParameterArgsNegatableBools(t *testing.T) {
	shapes := map[string]schema.ParamShape{
		"mmap":     {Kind: schema.ParamBool, Negatable: true},
		"cpu-moe":  {Kind: schema.ParamBool},
		"ctx-size": {Kind: schema.ParamInt},
	}
	for _, tc := range []struct {
		name   string
		params map[string]string
		want   []string
	}{
		{"true", map[string]string{"mmap": "true"}, []string{"--mmap"}},
		{"false", map[string]string{"mmap": "false"}, []string{"--no-mmap"}},
		{"absent", map[string]string{"ctx-size": "4096"}, []string{"--ctx-size", "4096"}},
		{"plain false", map[string]string{"cpu-moe": "false"}, []string{}},
		{"spelled with underscores", map[string]string{"mmap": "false", "cpu_moe": "true"}, []string{"--cpu-moe", "--no-mmap"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, buildParameterArgs(tc.params, shapes, nil))
		})
	}
}

// argv lists parameters in key order, so one launch renders one command.
func TestBuildParameterArgsSorted(t *testing.T) {
	params := map[string]string{"threads": "8", "batch-size": "512", "mmap": "false", "ctx-size": "4096", "jinja": "true"}
	shapes := map[string]schema.ParamShape{"mmap": {Kind: schema.ParamBool, Negatable: true}}
	want := []string{"--batch-size", "512", "--ctx-size", "4096", "--jinja", "--no-mmap", "--threads", "8"}
	for range 20 {
		require.Equal(t, want, buildParameterArgs(params, shapes, nil))
	}
}

// A variant's process loads its base's weights and answers in the
// variant's own name, so the engine never downloads or looks up the
// variant's name and the response echoes what the client asked for.
func TestBuild_VariantRunsItsBasesWeights(t *testing.T) {
	root := withModelsRoot(t, "Qwen3.8-27B-Q8_0")

	cfg := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{
				Type:    "cli",
				Command: "llama-server",
				Args:    []string{"--model", "${MODEL_PATH}", "--alias", "${MODEL}"},
			},
		},
		Models: map[string]config.ModelSpec{"Qwen3.8-27B-Q8_0+agent": {From: "Qwen3.8-27B-Q8_0"}},
	}
	launch, err := NewCommandBuilder(cfg).Build(ResolveModel(*cfg, "Qwen3.8-27B-Q8_0+agent"), 8080, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "Qwen3.8-27B-Q8_0"), argAfter(t, launch.Args, "--model"))
	assert.Equal(t, "Qwen3.8-27B-Q8_0+agent", argAfter(t, launch.Args, "--alias"))
	assert.Equal(t, "Qwen3.8-27B-Q8_0+agent", launch.WireModel)
}

func TestValidateVariantModel_RefusesWeightsArrivingUnderVariantName(t *testing.T) {
	root := withModelsRoot(t, "base")
	cfg := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{Execution: config.ExecutionConfig{Command: "engine", Args: []string{"${MODEL_PATH}", "${MODEL}"}}},
		Models:  map[string]config.ModelSpec{"base+fast": {From: "base"}},
	}
	err := ValidateVariantModel(cfg, "base+fast")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "BASE+FAST.gguf"), []byte("GGUF"), 0o600))
	for _, name := range []string{"base+fast", "BASE+FAST", "base+fast#Q4_K_M", "BASE+FAST.gguf", filepath.Join(root, "BASE+FAST.gguf")} {
		err := ValidateVariantModel(cfg, name)
		require.ErrorIs(t, err, config.ErrModelNameConflict)
	}
	cfg.Runtime.Execution.Args = []string{"${MODEL}"}
	err = ValidateVariantModel(cfg, "base+fast")
	require.ErrorIs(t, err, config.ErrModelNameConflict, "bare-name engines must also refuse")
	cfg.Models = nil
	err = ValidateVariantModel(cfg, "base+fast")
	require.NoError(t, err, "removing the variant resolves the conflict")
}

func TestValidateVariantModel_RefusesCompleteSplitWeightsUnderVariantName(t *testing.T) {
	root := withModelsRoot(t, "base")
	repo := filepath.Join(root, "owner", "repo")
	require.NoError(t, os.MkdirAll(repo, 0o700))
	cfg := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{Execution: config.ExecutionConfig{Command: "engine", Args: []string{"${MODEL_PATH}"}}},
		Models:  map[string]config.ModelSpec{"fast": {From: "base"}},
	}
	for _, name := range []string{"fast-00001-of-00002.gguf", "fast-00002-of-00002.gguf"} {
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), []byte("GGUF"), 0o600))
	}
	for _, model := range []string{"fast", "FAST", "fast#Q4", "owner/repo", "owner/repo#fast", "owner/repo#fast-00001-of-00002.gguf", filepath.Join(repo, "fast-00002-of-00002.gguf")} {
		t.Run(model, func(t *testing.T) {
			err := ValidateVariantModel(cfg, model)
			require.ErrorIs(t, err, config.ErrModelNameConflict)
		})
	}
	require.NoError(t, os.Remove(filepath.Join(repo, "fast-00002-of-00002.gguf")))
	err := ValidateVariantModel(cfg, "fast")
	require.NoError(t, err, "an incomplete split set is not loadable weights")
}

func TestValidateVariantModel_IgnoresDeclaredFeatureWeights(t *testing.T) {
	root := withModelsRoot(t, "base")
	require.NoError(t, os.WriteFile(filepath.Join(root, "mmproj-fast.gguf"), []byte("GGUF"), 0o600))
	cfg := &config.ServiceConfig{
		Runtime:  &config.AppRuntimeConfig{Execution: config.ExecutionConfig{Command: "engine", Args: []string{"${MODEL_PATH}"}}},
		Models:   map[string]config.ModelSpec{"mmproj-fast": {From: "base"}},
		Features: map[string]config.Feature{"vision": {Files: []string{"mmproj-*.gguf"}, Flag: "--mmproj"}},
	}
	err := ValidateVariantModel(cfg, "mmproj-fast")
	require.NoError(t, err, "a projector is not conflicting weights")
}

func TestBuildParameterArgsLiteralFlagSpelling(t *testing.T) {
	shapes, errs := schema.Merge("custom", &schema.YAMLSchema{Parameters: map[string]schema.YAMLParam{"fp8_e4m3fn-unet": {Type: "bool", Negatable: true}}}, config.ServiceConfig{})
	require.Empty(t, errs)
	for _, key := range []string{"fp8_e4m3fn_unet", "fp8-e4m3fn-unet"} {
		require.Equal(t, []string{"--fp8_e4m3fn-unet"}, buildParameterArgs(map[string]string{key: "true"}, shapes.Parameters, nil))
		require.Equal(t, []string{"--no-fp8_e4m3fn-unet"}, buildParameterArgs(map[string]string{key: "false"}, shapes.Parameters, nil))
	}
}

func TestShippedComfyUIFP8LiteralFlags(t *testing.T) {
	schemas, errs := schema.LoadFromFS(templates.AppsFS, "files/providers")
	require.Empty(t, errs)
	cfg := config.ServiceConfig{Runtime: &config.AppRuntimeConfig{Execution: config.ExecutionConfig{Type: "cli", Command: "comfyui"}}}
	for _, literal := range []string{"fp8_e4m3fn-unet", "fp8_e5m2-unet", "fp8_e8m0fnu-unet", "fp8_e4m3fn-text-enc", "fp8_e5m2-text-enc"} {
		name := strings.ReplaceAll(literal, "_", "-")
		require.Nil(t, schema.Check(name, "true", schemas["comfyui"].Parameters))
		launch, err := NewCommandBuilder(&cfg).Build(ResolvedModel{}, 1, map[string]string{name: "true"}, schemas["comfyui"].Parameters)
		require.NoError(t, err)
		require.Equal(t, []string{"--" + literal}, launch.Args)
	}
}
