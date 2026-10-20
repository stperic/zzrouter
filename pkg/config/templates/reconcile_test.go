package templates

import (
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config/assets"
)

func TestMergeManagedSpine(t *testing.T) {
	const tmpl = `description: "engine"
runtime:
  execution:
    command: engine
    args: ["--model", "${MODEL_PATH}"]
    wire_model: path
  exclude_parameters: [chat-template]
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
		merged, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl), nil)
		require.NoError(t, err)
		require.True(t, updated)

		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(merged, &doc))

		runtime := doc["runtime"].(map[string]any)
		exec := runtime["execution"].(map[string]any)
		assert.Equal(t, []any{"--model", "${MODEL_PATH}"}, exec["args"], "launch args come from the template")
		assert.Equal(t, "path", exec["wire_model"])
		assert.Equal(t, []any{"chat-template"}, runtime["exclude_parameters"], "what the engine does not take comes from the template")
		assert.Equal(t, "/v1/models", runtime["health_check"].(map[string]any)["path"])

		assert.Equal(t, true, doc["enabled"], "operator state survives")
		assert.Equal(t, "1.2.3", doc["pinned_version"], "operator state survives")
		assert.Equal(t, map[string]any{"max-tokens": "4096"},
			doc["defaults"].(map[string]any)["parameters"], "parameter tiers survive")
		assert.Contains(t, string(merged), "# MANAGED FILE", "header survives")
	})

	t.Run("no-op when the spine already matches", func(t *testing.T) {
		_, updated, err := mergeManagedSpine([]byte(tmpl), []byte(tmpl), nil)
		require.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("providers that launch nothing are left alone", func(t *testing.T) {
		live := "description: \"cloud\"\nenabled: true\n"
		merged, updated, err := mergeManagedSpine([]byte(live), []byte("description: \"cloud\"\n"), nil)
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
		merged, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl), nil)
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
		merged, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl), nil)
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
		_, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl), nil)
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
	merged, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl), nil)
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
				merged, updated, err := mergeManagedSpine([]byte(live.body), []byte(tmpl), nil)
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
	merged, updated, err := mergeManagedSpine([]byte(live), []byte(tmpl), nil)
	require.NoError(t, err)
	require.True(t, updated)

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(merged, &doc))
	caps := doc["capabilities"].(map[string]any)
	assert.Equal(t, []any{"chat_completions", "messages"}, caps["wire_endpoints"])
	assert.Equal(t, []any{"gguf", "safetensors"}, caps["formats"], "operator state survives")
	assert.Equal(t, 9, caps["priority"], "operator state survives")
	assert.Equal(t, true, doc["enabled"])

	_, updated, err = mergeManagedSpine(merged, []byte(tmpl), nil)
	require.NoError(t, err)
	assert.False(t, updated, "an in-sync config must not be rewritten")
}

// An existing llamacpp config that still declares native Messages loses it on
// the next start, so Messages is translated there too.
func TestReconcileManagedSpine_DropsNativeMessagesFromAnExistingLlamacpp(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, InstallDefaults(dir))

	path := filepath.Join(dir, "on-demand", "llamacpp", "config.yaml")
	seeded, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(seeded, &doc))
	doc["capabilities"].(map[string]any)["wire_endpoints"] = []any{"chat_completions", "completions", "embeddings", "rerank", "messages"}
	stale, err := yaml.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, stale, 0o600))

	changed, err := ReconcileManagedSpine(dir)
	require.NoError(t, err)
	assert.Contains(t, changed, pathpkg.Join("on-demand", "llamacpp", "config.yaml"))

	repaired, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(repaired, &doc))
	assert.NotContains(t, doc["capabilities"].(map[string]any)["wire_endpoints"], "messages")
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

// A shipped schema.yaml is owned outright: drift is rewritten whole. An
// operator's asset beside it is never touched.
func TestReconcileManagesSchema(t *testing.T) {
	src := fstest.MapFS{
		"files/providers/on-demand/eng/config.yaml": {Data: []byte("description: engine\n")},
		"files/providers/on-demand/eng/schema.yaml": {Data: []byte("parameters:\n  tmpl: {type: asset}\n")},
	}
	dst := t.TempDir()
	engDir := filepath.Join(dst, "on-demand", "eng")
	require.NoError(t, os.MkdirAll(filepath.Join(engDir, "assets"), 0o755))
	write := func(rel, data string) {
		require.NoError(t, os.WriteFile(filepath.Join(engDir, rel), []byte(data), 0o600))
	}
	write("config.yaml", "description: engine\n")
	write("schema.yaml", "parameters: {}\n")
	write("assets/mine.jinja", "operator bytes")

	changed, err := reconcile(src, dst)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{pathpkg.Join("on-demand", "eng", "schema.yaml")}, changed)

	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(engDir, rel))
		require.NoError(t, err)
		return string(data)
	}
	assert.Equal(t, "parameters:\n  tmpl: {type: asset}\n", read("schema.yaml"))
	assert.Equal(t, "operator bytes", read("assets/mine.jinja"))

	again, err := reconcile(src, dst)
	require.NoError(t, err)
	assert.Empty(t, again, "an install in step is not rewritten")
}

// Ownership follows the exact layout: a provider named "assets" keeps its
// config.yaml merged key by key, not overwritten whole.
func TestShippedFileOwnership(t *testing.T) {
	cases := map[string]ownership{
		"on-demand/eng/config.yaml":        managedKeys,
		"on-demand/eng/schema.yaml":        managedWhole,
		"on-demand/eng/assets/t.jinja":     notManaged,
		"on-demand/assets/config.yaml":     managedKeys,
		"on-demand/assets/schema.yaml":     managedWhole,
		"on-demand/eng/assets/sub/t.jinja": notManaged,
		"on-demand/eng/assets/schema.yaml": notManaged,
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
			got, updated, err := mergeManagedSpine([]byte(tc.live), tmpl, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.wantUpdate, updated)
			if tc.wantUpdate {
				assert.Contains(t, string(got), "vision:")
				assert.Contains(t, string(got), "default: true")
			} else {
				assert.Equal(t, tc.live, string(got))
			}
			again, changed, err := mergeManagedSpine(got, tmpl, nil)
			require.NoError(t, err)
			assert.False(t, changed)
			assert.Equal(t, got, again)
		})
	}
	live := []byte("features:\n  vision: {}\n")
	got, updated, err := mergeManagedSpine(live, []byte("description: cloud\n"), nil)
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
		got, changed, err := mergeManagedSpine(live, []byte(template), nil)
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
		again, changed, err := mergeManagedSpine(got, []byte(template), nil)
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Equal(t, got, again)
		live = got
	}
}

// Feature declarations are managed: an operator keeps only each feature's
// default, so edited files and flags and unshipped names are replaced.
func TestReconcileReplacesOperatorFeatureEdits(t *testing.T) {
	live := []byte("features:\n  vision:\n    default: false\n    files: [custom-*.gguf]\n    flag: custom\n  draft:\n    files: [draft-*.gguf]\n    flag: model-draft\n")
	tmpl := []byte("features:\n  vision:\n    default: true\n    files: [mmproj-*.gguf]\n    flag: mmproj\n")
	got, changed, err := mergeManagedSpine(live, tmpl, nil)
	require.NoError(t, err)
	require.True(t, changed)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(got, &doc))
	features := doc["features"].(map[string]any)
	assert.NotContains(t, features, "draft")
	assert.Equal(t, map[string]any{"default": false, "files": []any{"mmproj-*.gguf"}, "flag": "mmproj"}, features["vision"])
}

func TestReconcileRemovesRetiredFeatureDeclaration(t *testing.T) {
	got, changed, err := mergeManagedSpine([]byte("features:\n  vision: {builtin: true}\n"), []byte("features: {}\n"), nil)
	require.NoError(t, err)
	assert.True(t, changed)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(got, &doc))
	assert.Empty(t, doc["features"])
	again, changed, err := mergeManagedSpine(got, []byte("features: {}\n"), nil)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, got, again)
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

func TestReconcileManagedSpineAddsFailurePatterns(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, InstallDefaults(dir))
	for _, rel := range []string{"on-demand/llamacpp/config.yaml", "on-demand/vllm/config.yaml", "on-demand/mlx/config.yaml", "external/ollama/config.yaml"} {
		file := filepath.Join(dir, filepath.FromSlash(rel))
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(raw, &doc))
		runtime := doc["runtime"].(map[string]any)
		expected := runtime["failure_patterns"]
		delete(runtime, "failure_patterns")
		stale, err := yaml.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(file, stale, 0600))
		changed, err := ReconcileManagedSpine(dir)
		require.NoError(t, err)
		require.Contains(t, changed, rel)
		repaired, err := os.ReadFile(file)
		require.NoError(t, err)
		require.NoError(t, yaml.Unmarshal(repaired, &doc))
		assert.Equal(t, expected, doc["runtime"].(map[string]any)["failure_patterns"])
	}
	changed, err := ReconcileManagedSpine(dir)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestReconcileVllmReadinessStall(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, InstallDefaults(dir))
	rel := "on-demand/vllm/config.yaml"
	file := filepath.Join(dir, filepath.FromSlash(rel))
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	readiness := doc["runtime"].(map[string]any)["health_check"].(map[string]any)["readiness_probe"].(map[string]any)
	require.Equal(t, "240s", readiness["stall_timeout"])
	require.Equal(t, "900s", readiness["timeout"])
	delete(readiness, "stall_timeout")
	readiness["timeout"] = "300s"
	stale, err := yaml.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, stale, 0600))
	changed, err := ReconcileManagedSpine(dir)
	require.NoError(t, err)
	require.Contains(t, changed, rel)
	repaired, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(repaired, &doc))
	readiness = doc["runtime"].(map[string]any)["health_check"].(map[string]any)["readiness_probe"].(map[string]any)
	require.Equal(t, "240s", readiness["stall_timeout"])
	require.Equal(t, "900s", readiness["timeout"])
	changed, err = ReconcileManagedSpine(dir)
	require.NoError(t, err)
	require.Empty(t, changed)
}

func TestReconcilePreservesOperatorSchema(t *testing.T) {
	src := fstest.MapFS{"files/providers/on-demand/eng/config.yaml": {Data: []byte("description: updated\nruntime:\n  execution: {command: engine}\n")}}
	root := t.TempDir()
	filename := filepath.Join(root, "on-demand", "eng", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(filename), 0700))
	require.NoError(t, os.WriteFile(filename, []byte("description: old\nschema:\n  future_flag: {type: bool, negatable: true}\n  dtype: {enum: [old, new]}\nruntime:\n  execution: {command: previous}\n"), 0600))
	var before map[string]any
	data, err := os.ReadFile(filename)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(data, &before))
	changed, err := reconcile(src, root)
	require.NoError(t, err)
	require.NotEmpty(t, changed)
	data, err = os.ReadFile(filename)
	require.NoError(t, err)
	var after map[string]any
	require.NoError(t, yaml.Unmarshal(data, &after))
	assert.Equal(t, before["schema"], after["schema"])
	assert.Equal(t, "engine", after["runtime"].(map[string]any)["execution"].(map[string]any)["command"])
}
