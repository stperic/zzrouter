package templates

import (
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

func TestMergeManagedSpine(t *testing.T) {
	const tmpl = `description: "engine"
runtime:
  execution:
    command: engine
    args: ["--model", "${MODEL_PATH}"]
    wire_model: path
  health_check:
    path: /v1/models
`

	t.Run("replaces a stale launch spine", func(t *testing.T) {
		live := `# MANAGED FILE — overwritten on every cluster sync.
description: "engine"
enabled: true
pinned_version: "1.2.3"
defaults:
  parameters:
    max-tokens: "4096"
runtime:
  execution:
    command: engine
    args: ["--model", "${MODEL}"]
  health_check:
    path: /health
`
		merged, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl))
		require.NoError(t, err)
		require.True(t, updated)

		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(merged, &doc))

		runtime := doc["runtime"].(map[string]any)
		exec := runtime["execution"].(map[string]any)
		assert.Equal(t, []any{"--model", "${MODEL_PATH}"}, exec["args"], "launch args come from the template")
		assert.Equal(t, "path", exec["wire_model"])
		assert.Equal(t, "/v1/models", runtime["health_check"].(map[string]any)["path"])

		assert.Equal(t, true, doc["enabled"], "operator state survives")
		assert.Equal(t, "1.2.3", doc["pinned_version"], "operator state survives")
		assert.Equal(t, map[string]any{"max-tokens": "4096"},
			doc["defaults"].(map[string]any)["parameters"], "parameter tiers survive")
		assert.Contains(t, string(merged), "# MANAGED FILE", "header survives")
	})

	t.Run("no-op when the spine already matches", func(t *testing.T) {
		_, updated, err := mergeManagedSpine([]byte(tmpl), []byte(tmpl))
		require.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("providers that launch nothing are left alone", func(t *testing.T) {
		live := "description: \"cloud\"\nenabled: true\n"
		merged, updated, err := mergeManagedSpine([]byte(live), []byte("description: \"cloud\"\n"))
		require.NoError(t, err)
		assert.False(t, updated)
		assert.Equal(t, live, string(merged))
	})
}

// TestReconcileManagedSpine_FixesAnExistingTree covers the gap that made
// this necessary: InstallDefaults is copy-if-missing, so a provider tree
// that already exists never receives an engine-level fix.
func TestReconcileManagedSpine_FixesAnExistingTree(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, InstallDefaults(dir))

	mlx := filepath.Join(dir, "on-demand", "mlx", "config.yaml")
	original, err := os.ReadFile(mlx)
	require.NoError(t, err)

	stale := []byte(`description: "Apple MLX"
enabled: true
runtime:
  execution:
    type: python
    command: python3
    args: ["-m", "mlx_lm", "server", "--model", "${MODEL}"]
`)
	require.NoError(t, os.WriteFile(mlx, stale, 0o600))

	changed, err := ReconcileManagedSpine(dir)
	require.NoError(t, err)
	assert.Contains(t, changed, pathpkg.Join("on-demand", "mlx", "config.yaml"))

	repaired, err := os.ReadFile(mlx)
	require.NoError(t, err)
	assert.Contains(t, string(repaired), "${MODEL_PATH}")
	assert.Contains(t, string(repaired), "wire_model")
	assert.Contains(t, string(repaired), "enabled: true", "operator state survives the repair")

	// A freshly seeded tree is already correct, so reconcile is a no-op.
	require.NoError(t, os.WriteFile(mlx, original, 0o600))
	changed, err = ReconcileManagedSpine(dir)
	require.NoError(t, err)
	assert.NotContains(t, changed, pathpkg.Join("on-demand", "mlx", "config.yaml"))
}

func TestMergeManagedSpineCarriesVersionSource(t *testing.T) {
	const tmpl = `description: "engine"
version_source:
  type: github_release
  repo: ggml-org/llama.cpp
  strip_prefix: "b"
  compare: build_number
runtime:
  execution:
    command: engine
`

	t.Run("adds the key to a config predating it", func(t *testing.T) {
		// Without this, only fresh installs could ever report whether
		// upstream has moved on, which is the drift the key exists to catch.
		live := `# MANAGED FILE — overwritten on every cluster sync.
description: "engine"
enabled: true
pinned_version: "b10453"
runtime:
  execution:
    command: engine
`
		merged, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl))
		require.NoError(t, err)
		require.True(t, updated)

		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(merged, &doc))

		src, ok := doc["version_source"].(map[string]any)
		require.True(t, ok, "version_source must be present after reconcile")
		assert.Equal(t, "github_release", src["type"])
		assert.Equal(t, "ggml-org/llama.cpp", src["repo"])

		// Operator-owned keys are untouched.
		assert.Equal(t, "b10453", doc["pinned_version"])
		assert.Equal(t, true, doc["enabled"])
	})

	t.Run("corrects a hand-edited source", func(t *testing.T) {
		live := `description: "engine"
version_source:
  type: pypi
  package: wrong
runtime:
  execution:
    command: engine
`
		merged, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl))
		require.NoError(t, err)
		require.True(t, updated)

		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(merged, &doc))
		src := doc["version_source"].(map[string]any)
		assert.Equal(t, "github_release", src["type"])
		assert.Nil(t, src["package"], "the template replaces the section outright")
	})

	t.Run("no rewrite when already correct", func(t *testing.T) {
		live := `description: "engine"
version_source:
  type: github_release
  repo: ggml-org/llama.cpp
  strip_prefix: "b"
  compare: build_number
runtime:
  execution:
    command: engine
`
		_, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl))
		require.NoError(t, err)
		assert.False(t, updated, "an in-sync config must not be rewritten")
	})
}

// A provider with no runtime section still gets its top-level managed keys.
func TestMergeManagedSpineTopLevelWithoutRuntime(t *testing.T) {
	const tmpl = `description: "daemon"
version_source:
  type: github_release
  repo: ollama/ollama
  strip_prefix: "v"
  compare: semver
`
	live := `description: "daemon"
enabled: true
`
	merged, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl))
	require.NoError(t, err)
	require.True(t, updated)

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(merged, &doc))
	src, ok := doc["version_source"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "ollama/ollama", src["repo"])
	assert.Equal(t, true, doc["enabled"])
}

// A zero-byte or comment-only config.yaml unmarshals to a nil map. Assigning
// a managed key into that map panics, and reconcile runs during node startup,
// so the panic would take down the boot rather than skip one file. Reachable
// in the field via a crash mid-write or a truncated peer-sync.
func TestMergeManagedSpineSurvivesEmptyLiveConfig(t *testing.T) {
	const tmpl = `description: "engine"
version_source:
  type: github_release
  repo: ggml-org/llama.cpp
  compare: build_number
runtime:
  execution:
    command: engine
`
	for _, live := range []struct{ name, body string }{
		{"zero bytes", ""},
		{"newline only", "\n"},
		{"comment only", "# nothing here\n"},
		{"explicit null", "null\n"},
	} {
		t.Run(live.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				merged, updated, err := mergeManagedSpine([]byte(live.body), []byte(tmpl))
				require.NoError(t, err)
				require.True(t, updated)

				var doc map[string]any
				require.NoError(t, yaml.Unmarshal(merged, &doc))
				src, ok := doc["version_source"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, "ggml-org/llama.cpp", src["repo"])
			})
		})
	}
}

// wire_endpoints is the engine's, the rest of capabilities the operator's:
// an install predating a surface the engine gained gets it, and keeps its
// own format and priority choices.
func TestMergeManagedSpineCarriesWireEndpoints(t *testing.T) {
	const tmpl = `description: "engine"
capabilities:
  formats: [gguf]
  priority: 5
  wire_endpoints: [chat_completions, messages]
`
	live := `description: "engine"
enabled: true
capabilities:
  formats: [gguf, safetensors]
  priority: 9
  wire_endpoints: [chat_completions]
`
	merged, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl))
	require.NoError(t, err)
	require.True(t, updated)

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(merged, &doc))
	caps := doc["capabilities"].(map[string]any)
	assert.Equal(t, []any{"chat_completions", "messages"}, caps["wire_endpoints"])
	assert.Equal(t, []any{"gguf", "safetensors"}, caps["formats"], "operator state survives")
	assert.Equal(t, 9, caps["priority"], "operator state survives")
	assert.Equal(t, true, doc["enabled"])

	_, updated, err = mergeManagedSpine(merged, []byte(tmpl))
	require.NoError(t, err)
	assert.False(t, updated, "an in-sync config must not be rewritten")
}

// The upgrade this exists for: a llamacpp config written before llama-server
// spoke the Messages API gains it on the next start.
func TestReconcileManagedSpine_AddsMessagesToAnExistingLlamacpp(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, InstallDefaults(dir))

	path := filepath.Join(dir, "on-demand", "llamacpp", "config.yaml")
	seeded, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(seeded, &doc))
	doc["capabilities"].(map[string]any)["wire_endpoints"] = []any{"chat_completions"}
	stale, err := yaml.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, stale, 0o600))

	changed, err := ReconcileManagedSpine(dir)
	require.NoError(t, err)
	assert.Contains(t, changed, pathpkg.Join("on-demand", "llamacpp", "config.yaml"))

	repaired, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(repaired, &doc))
	assert.Contains(t, doc["capabilities"].(map[string]any)["wire_endpoints"], "messages")
}

// wire_endpoints is required of an enabled provider, so a template with a
// capabilities section but no wire_endpoints would have reconcile delete
// the operator's list and fail config load on the next start.
func TestEveryTemplateCapabilitiesDeclaresWireEndpoints(t *testing.T) {
	err := fs.WalkDir(AppsFS, "files/providers", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(p) != "config.yaml" {
			return err
		}
		data, err := fs.ReadFile(AppsFS, p)
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(data, &doc), p)
		caps, ok := doc["capabilities"].(map[string]any)
		if !ok {
			return nil
		}
		wire, _ := caps["wire_endpoints"].([]any)
		assert.NotEmpty(t, wire, "%s: capabilities without wire_endpoints", p)
		return nil
	})
	require.NoError(t, err)
}

// A shipped schema.yaml and shipped assets are owned outright: drift is
// rewritten whole. An operator's own asset beside them is never touched,
// and a shipped file the operator removed is InstallDefaults' to restore.
func TestReconcileManagesSchemaAndShippedAssets(t *testing.T) {
	src := fstest.MapFS{
		"files/providers/on-demand/eng/config.yaml":          {Data: []byte("description: engine\n")},
		"files/providers/on-demand/eng/schema.yaml":          {Data: []byte("parameters:\n  tmpl: {type: asset}\n")},
		"files/providers/on-demand/eng/assets/shipped.jinja": {Data: []byte("release bytes")},
		"files/providers/on-demand/eng/assets/gone.jinja":    {Data: []byte("release bytes")},
	}
	dst := t.TempDir()
	engDir := filepath.Join(dst, "on-demand", "eng")
	require.NoError(t, os.MkdirAll(filepath.Join(engDir, "assets"), 0o755))
	write := func(rel, data string) {
		require.NoError(t, os.WriteFile(filepath.Join(engDir, rel), []byte(data), 0o600))
	}
	write("config.yaml", "description: engine\n")
	write("schema.yaml", "parameters: {}\n")
	write("assets/shipped.jinja", "edited on disk")
	write("assets/mine.jinja", "operator bytes")

	changed, err := reconcile(src, dst)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		pathpkg.Join("on-demand", "eng", "schema.yaml"),
		pathpkg.Join("on-demand", "eng", "assets", "shipped.jinja"),
	}, changed)

	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(engDir, rel))
		require.NoError(t, err)
		return string(data)
	}
	assert.Equal(t, "parameters:\n  tmpl: {type: asset}\n", read("schema.yaml"))
	assert.Equal(t, "release bytes", read("assets/shipped.jinja"))
	assert.Equal(t, "operator bytes", read("assets/mine.jinja"))
	_, statErr := os.Stat(filepath.Join(engDir, "assets", "gone.jinja"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)

	again, err := reconcile(src, dst)
	require.NoError(t, err)
	assert.Empty(t, again, "an install in step is not rewritten")
}

func TestIsShippedAsset(t *testing.T) {
	src := fstest.MapFS{
		"files/providers/on-demand/eng/config.yaml":          {Data: []byte("x")},
		"files/providers/on-demand/eng/assets/shipped.jinja": {Data: []byte("x")},
	}
	assert.True(t, isShippedAsset(src, "eng", "shipped.jinja"))
	assert.False(t, isShippedAsset(src, "eng", "mine.jinja"))
	assert.False(t, isShippedAsset(src, "other", "shipped.jinja"), "shipped names are per provider")
	for _, provider := range []string{"", ".", "..", "../on-demand/eng", "on-demand/eng"} {
		assert.False(t, isShippedAsset(src, provider, "shipped.jinja"), "provider %q", provider)
	}
	assert.False(t, isShippedAsset(src, "eng", "../config.yaml"), "a name is a single element")
}

// Ownership follows the exact layout: a provider named "assets" keeps its
// config.yaml merged key by key, not overwritten whole.
func TestShippedFileOwnership(t *testing.T) {
	cases := map[string]ownership{
		"on-demand/eng/config.yaml":        managedKeys,
		"on-demand/eng/schema.yaml":        managedWhole,
		"on-demand/eng/assets/t.jinja":     managedWhole,
		"on-demand/assets/config.yaml":     managedKeys,
		"on-demand/assets/schema.yaml":     managedWhole,
		"on-demand/eng/assets/sub/t.jinja": notManaged,
		"on-demand/eng/assets/schema.yaml": managedWhole,
		"on-demand/eng/README.md":          notManaged,
		"on-demand/eng.schema.yaml":        notManaged,
		"on-demand/eng/nested/schema.yaml": notManaged,
	}
	for rel, want := range cases {
		assert.Equal(t, want, shippedFileOwnership(rel), rel)
	}
}

// Every asset the release ships must be addressable: a valid name within
// the caps the API and sync enforce.
func TestShippedAssetsAreAddressable(t *testing.T) {
	perProvider := map[string]int{}
	err := fs.WalkDir(AppsFS, providersRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, providersRoot), "/")
		if parts := strings.Split(rel, "/"); len(parts) != providerFileDepth+1 || parts[2] != assets.DirName {
			return nil
		}
		data, err := fs.ReadFile(AppsFS, p)
		require.NoError(t, err)
		assert.NoError(t, assets.ValidateName(pathpkg.Base(rel)), rel)
		assert.LessOrEqual(t, len(data), assets.MaxAssetBytes, rel)
		perProvider[pathpkg.Dir(pathpkg.Dir(rel))] += len(data)
		return nil
	})
	require.NoError(t, err)
	for provider, total := range perProvider {
		assert.LessOrEqual(t, total, assets.MaxProviderBytes, provider)
	}
}

// Every engine the release launches serves the Anthropic Messages API,
// natively or through the translation, so an Anthropic-API agent can use
// any model the cluster runs. Which way is the engine's own declaration.
func TestEveryShippedEngineServesMessages(t *testing.T) {
	entries, err := fs.ReadDir(AppsFS, providersRoot+"/on-demand")
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, e := range entries {
		body, err := fs.ReadFile(AppsFS, providersRoot+"/on-demand/"+e.Name()+"/config.yaml")
		require.NoError(t, err, e.Name())
		var doc struct {
			Capabilities struct {
				WireEndpoints []string `yaml:"wire_endpoints"`
			} `yaml:"capabilities"`
		}
		require.NoError(t, yaml.Unmarshal(body, &doc), e.Name())
		wire := doc.Capabilities.WireEndpoints
		assert.True(t, slices.Contains(wire, "messages") || slices.Contains(wire, "messages_compat"),
			"%s declares neither messages nor messages_compat: %v", e.Name(), wire)
	}
}

func TestReconcileSeedsFeatures(t *testing.T) {
	tmpl := []byte("features:\n  vision:\n    default: true\n    files: [mmproj-*.gguf]\n    flag: mmproj\n")
	for _, tc := range []struct {
		name, live string
		wantUpdate bool
	}{
		{"legacy", "enabled: true\n", true},
		{"operator default", "features:\n  vision:\n    default: false\n    files: [mmproj-*.gguf]\n    flag: mmproj\n", false},
		{"empty declaration", "features: {}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, updated, err := mergeManagedSpine([]byte(tc.live), tmpl)
			require.NoError(t, err)
			assert.Equal(t, tc.wantUpdate, updated)
			if tc.wantUpdate {
				assert.Contains(t, string(got), "vision:")
				assert.Contains(t, string(got), "default: true")
			} else {
				assert.Equal(t, tc.live, string(got))
			}
			again, changed, err := mergeManagedSpine(got, tmpl)
			require.NoError(t, err)
			assert.False(t, changed)
			assert.Equal(t, got, again)
		})
	}
	live := []byte("features:\n  vision: {}\n")
	got, updated, err := mergeManagedSpine(live, []byte("description: cloud\n"))
	require.NoError(t, err)
	assert.False(t, updated)
	assert.Equal(t, live, got)
}

func TestReconcileRefreshesFeatureDeclarations(t *testing.T) {
	live := []byte("features:\n  vision:\n    default: false\n    runtime: old-runtime\n    execution: {type: python, command: old}\n    when: vision_config\n    wire_endpoints: [messages]\n    exclude_parameters: [old-flag]\ndefaults:\n  features: []\nmodels:\n  model:\n    features: [vision]\n")
	for _, template := range []string{
		"features:\n  vision:\n    default: true\n    files: [projector-*.gguf]\n    flag: mmproj\n",
		"features:\n  vision:\n    default: true\n    files: [mmproj-*.gguf]\n    flag: projector\n",
	} {
		got, changed, err := mergeManagedSpine(live, []byte(template))
		require.NoError(t, err)
		require.True(t, changed)
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(got, &doc))
		feature := doc["features"].(map[string]any)["vision"].(map[string]any)
		assert.Equal(t, false, feature["default"])
		assert.NotContains(t, feature, "runtime")
		assert.NotContains(t, feature, "execution")
		assert.NotContains(t, feature, "wire_endpoints")
		assert.NotContains(t, feature, "exclude_parameters")
		assert.Contains(t, doc, "defaults")
		assert.Contains(t, doc, "models")
		again, changed, err := mergeManagedSpine(got, []byte(template))
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Equal(t, got, again)
		live = got
	}
}

func TestReconcileRemovesRetiredFeatureDeclaration(t *testing.T) {
	got, changed, err := mergeManagedSpine([]byte("features:\n  vision: {builtin: true}\n"), []byte("features: {}\n"))
	require.NoError(t, err)
	assert.True(t, changed)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(got, &doc))
	assert.Empty(t, doc["features"])
	again, changed, err := mergeManagedSpine(got, []byte("features: {}\n"))
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, got, again)
}

func TestReconcileRestoresShippedDiagnostics(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, InstallDefaults(root))
	filename := filepath.Join(root, "on-demand", "vllm", "schema.yaml")
	require.NoError(t, os.WriteFile(filename, []byte("parameters: {}\n"), 0o600))
	changed, err := ReconcileManagedSpine(root)
	require.NoError(t, err)
	assert.Contains(t, changed, pathpkg.Join("on-demand", "vllm", "schema.yaml"))
	restored, err := os.ReadFile(filename)
	require.NoError(t, err)
	declared, err := schema.LoadYAMLSchema(restored)
	require.NoError(t, err)
	require.NotNil(t, declared.Diagnostics)
	require.NoError(t, declared.Diagnostics.Validate())
	assert.Contains(t, declared.Diagnostics.Runtimes["vllm"].Imports, "vllm.entrypoints.openai.api_server")
	assert.Contains(t, declared.Diagnostics.Runtimes["vllm"].Imports, "vllm.v1.engine.core")

	for _, rel := range []string{"on-demand/mlx", "on-demand/llamacpp", "external/ollama"} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel), "schema.yaml"))
		require.NoError(t, err)
		declared, err := schema.LoadYAMLSchema(data)
		require.NoError(t, err)
		require.NoError(t, declared.Diagnostics.Validate())
		require.NotNil(t, declared.Diagnostics)
	}
}

func TestMLXDiagnosticDoesNotRequireJITOnlyBuild(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, InstallDefaults(root))
	data, err := os.ReadFile(filepath.Join(root, "on-demand", "mlx", "schema.yaml"))
	require.NoError(t, err)
	declared, err := schema.LoadYAMLSchema(data)
	require.NoError(t, err)
	for _, runtime := range []string{"mlx", "mlx-vlm"} {
		t.Run(runtime, func(t *testing.T) {
			assert.Equal(t, "prebuilt_or_jit", declared.Diagnostics.Runtimes[runtime].Kernels)
		})
	}
}

func TestReconcileRefreshesShippedPinsWithoutChangingOperatorTuning(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, InstallDefaults(dir))
	provider := filepath.Join(dir, "on-demand", "vllm", "config.yaml")
	original, err := os.ReadFile(provider)
	require.NoError(t, err)
	var live map[string]any
	require.NoError(t, yaml.Unmarshal(original, &live))
	live["pinned_version"] = "0.19.0"
	live["enabled"] = true
	nodes := map[string]any{"worker-1": map[string]any{"parameters": map[string]any{"gpu-memory-utilization": 0.55}}}
	live["nodes"] = nodes
	stale, err := yaml.Marshal(live)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(provider, stale, 0600))

	// No shipped pin: an Ollama operator pin remains persistent.
	ollama := filepath.Join(dir, "external", "ollama", "config.yaml")
	content, err := os.ReadFile(ollama)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ollama, append(content, []byte("\npinned_version: \"0.32.14\"\n")...), 0600))
	custom := filepath.Join(dir, "on-demand", "custom-engine", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(custom), 0700))
	require.NoError(t, os.WriteFile(custom, []byte("pinned_version: \"custom-pin\"\n"), 0600))

	changed, err := ReconcileManagedSpine(dir)
	require.NoError(t, err)
	assert.Contains(t, changed, "on-demand/vllm/config.yaml")
	repaired, err := os.ReadFile(provider)
	require.NoError(t, err)
	var updated map[string]any
	require.NoError(t, yaml.Unmarshal(repaired, &updated))
	assert.Equal(t, "0.29.0", updated["pinned_version"])
	assert.Equal(t, true, updated["enabled"])
	assert.Equal(t, nodes, updated["nodes"])
	for name, pin := range map[string]string{ollama: "0.32.14", custom: "custom-pin"} {
		content, err := os.ReadFile(name)
		require.NoError(t, err)
		var config map[string]any
		require.NoError(t, yaml.Unmarshal(content, &config))
		assert.Equal(t, pin, config["pinned_version"])
	}
	changed, err = ReconcileManagedSpine(dir)
	require.NoError(t, err)
	assert.Empty(t, changed, "second pass must be idempotent")
}
