package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

func TestLocalizeAssetParams(t *testing.T) {
	dir := assets.NewDir(t.TempDir())
	_, err := dir.Write("t.jinja", []byte("x"))
	require.NoError(t, err)
	shapes := map[string]schema.ParamShape{
		"chat-template-file": {Kind: schema.ParamAsset},
		"ctx-size":           {Kind: schema.ParamInt},
	}

	in := map[string]string{"chat-template-file": "t.jinja", "ctx-size": "4096", "free": "t.jinja"}
	localized, err := localizeAssetParams("eng", in, shapes, dir.Path)
	require.NoError(t, err)
	out := localized.Params
	want, err := dir.Path("t.jinja")
	require.NoError(t, err)
	assert.Equal(t, want, out["chat-template-file"])
	assert.True(t, filepath.IsAbs(out["chat-template-file"]))
	assert.Equal(t, "4096", out["ctx-size"])
	assert.Equal(t, "t.jinja", out["free"], "only asset-typed keys are resolved")
	assert.Equal(t, map[string]string{"chat-template-file": assets.Digest([]byte("x"))}, localized.Files,
		"the content of each resolved asset is part of what the launch was given")
	assert.Equal(t, "t.jinja", in["chat-template-file"], "the caller's map is not modified")

	same, err := localizeAssetParams("eng", map[string]string{"ctx-size": "1"}, shapes, dir.Path)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"ctx-size": "1"}, same.Params)
	assert.Empty(t, same.Files)

	_, err = localizeAssetParams("eng", map[string]string{"chat-template-file": "absent.jinja"}, shapes, dir.Path)
	assert.ErrorIs(t, err, assets.ErrNotFound)
	_, err = localizeAssetParams("eng", map[string]string{"chat-template-file": "../../etc/passwd"}, shapes, dir.Path)
	assert.ErrorIs(t, err, assets.ErrInvalidName)
	_, err = localizeAssetParams("eng", map[string]string{"chat-template-file": "t.jinja"}, shapes, assets.Dir{}.Path)
	assert.Error(t, err, "a node without the provider's assets cannot resolve one")
}

// The resolver answers from this node's own schema and assets, including
// a key declared only in the launch endpoint's overlay.
func TestAssetParamLocalizer_UsesNodeSchemaAndOverlay(t *testing.T) {
	root := t.TempDir()
	seedTestProviders(t, root, []string{"llamacpp"}, nil)
	providerDir := filepath.Join(root, "on-demand", "llamacpp")
	require.NoError(t, os.WriteFile(filepath.Join(providerDir, "schema.yaml"), []byte(
		"parameters:\n  chat-template-file: {type: asset}\n"+
			"endpoints:\n  embeddings:\n    parameters:\n      pooling-file: {type: asset}\n"), 0o600))
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	_, err = store.WriteAsset("llamacpp", "t.jinja", []byte("x"))
	require.NoError(t, err)
	localize := assetParamLocalizer(store)

	out, err := localize("llamacpp", "chat", "", map[string]string{"chat-template-file": "t.jinja", "pooling-file": "t.jinja"}, loadProviderSchema(store, "llamacpp").ForEndpoint("chat"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(providerDir, "assets", "t.jinja"), out.Params["chat-template-file"])
	assert.Equal(t, "t.jinja", out.Params["pooling-file"], "an overlay key is not an asset on another endpoint")

	out, err = localize("llamacpp", "embeddings", "", map[string]string{"pooling-file": "t.jinja"}, loadProviderSchema(store, "llamacpp").ForEndpoint("embeddings"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(providerDir, "assets", "t.jinja"), out.Params["pooling-file"])

	_, err = localize("llamacpp", "chat", "", map[string]string{"chat-template-file": "absent.jinja"}, loadProviderSchema(store, "llamacpp").ForEndpoint("chat"))
	assert.ErrorIs(t, err, assets.ErrNotFound)

	untouched := map[string]string{"chat-template-file": "t.jinja"}
	out, err = assetParamLocalizer(nil)("llamacpp", "chat", "", untouched, loadProviderSchema(nil, "llamacpp").ForEndpoint("chat"))
	require.NoError(t, err)
	assert.Equal(t, untouched, out.Params, "with no store there is no schema declaring an asset")
}

// The node's provider manager resolves asset parameters: a launch naming
// an asset this node does not have is refused before any argv is built.
func TestServerManagerLocalizesAssetParams(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "m.gguf"), []byte("GGUF"), 0o600))
	cfg := DefaultTestNodeConfig()
	cfg.NodeName = "test-host"
	server := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: cfg, modelsDir: root})
	_, err := server.configStore.SetProviderEnabled("llamacpp", "", true)
	require.NoError(t, err)

	_, err = server.providers.appMgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{
		Provider: "llamacpp", Model: "m",
		Parameters: map[string]string{"chat-template-file": "absent.jinja"},
	})
	require.ErrorIs(t, err, prov_apps.ErrParameterValidation)
	assert.ErrorIs(t, err, assets.ErrNotFound)
}

// The shipped schemas declare the engines' template flags as assets, so an
// operator's template resolves to the file beside the provider's config.
func TestTemplateParamsResolveToTheAssetFile(t *testing.T) {
	root := t.TempDir()
	seedTestProviders(t, root, []string{"llamacpp", "vllm"}, nil)
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	_, err = store.WriteAsset("llamacpp", "t.jinja", []byte("{{ messages }}"))
	require.NoError(t, err)

	out, err := assetParamLocalizer(store)("llamacpp", "chat", "", map[string]string{"chat-template-file": "t.jinja"}, loadProviderSchema(store, "llamacpp").ForEndpoint("chat"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "on-demand", "llamacpp", assets.DirName, "t.jinja"), out.Params["chat-template-file"])

	assert.Equal(t, schema.ParamAsset, loadProviderSchema(store, "vllm").Parameters["chat-template"].Kind)
}

// A provider's schema is its own schema.yaml: another provider's broken
// one, or an asset named like a schema, has no bearing on it.
func TestLoadProviderSchema_ReadsOnlyThatProvider(t *testing.T) {
	root := t.TempDir()
	seedTestProviders(t, root, []string{"llamacpp", "vllm"}, nil)
	write := func(rel, data string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	}
	write("on-demand/llamacpp/schema.yaml", "parameters:\n  chat-template-file: {type: asset}\n")
	write("on-demand/vllm/schema.yaml", ":\n:not yaml")
	write("on-demand/llamacpp/assets/schema.yaml", "parameters:\n  injected: {type: string}\n")
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)

	llama := loadProviderSchema(store, "llamacpp")
	assert.Equal(t, schema.ParamAsset, llama.Parameters["chat-template-file"].Kind)
	assert.NotContains(t, llama.Parameters, "injected")

	vllm := loadProviderSchema(store, "vllm")
	assert.Equal(t, schema.ParamInt, vllm.Parameters["max-model-len"].Kind, "a broken schema.yaml falls back to the Go spine")

	assert.Equal(t, schema.ParamInt, loadProviderSchema(nil, "vllm").Parameters["max-model-len"].Kind)
}

// The locator keeps the reason there is no asset, so an operator sees it.
func TestProviderAssets_KeepsTheCause(t *testing.T) {
	root := t.TempDir()
	seedTestProviders(t, root, []string{"llamacpp"}, nil)
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)

	_, err = providerAssets(store, "no-such-provider")("t.jinja")
	assert.ErrorIs(t, err, assets.ErrNotFound)
	assert.ErrorIs(t, err, pkgConfig.ErrProviderNotFound)

	_, err = providerAssets(nil, "llamacpp")("t.jinja")
	assert.ErrorIs(t, err, assets.ErrNotFound)
}

// mlx_lm.server takes the template text; the launch hands it over as the
// flag's value, keeps the digest, and refuses what exec could not take.
func TestLocalizeAssetParams_PassByContent(t *testing.T) {
	dir := assets.NewDir(t.TempDir())
	_, err := dir.Write("t.jinja", []byte("{% for m in messages %}\n{{ m.content }}{% endfor %}"))
	require.NoError(t, err)
	_, err = dir.Write("zero.jinja", []byte("a\x00b"))
	require.NoError(t, err)
	_, err = dir.Write("big.jinja", make([]byte, maxContentArgBytes+1))
	require.NoError(t, err)
	shapes := map[string]schema.ParamShape{
		"chat-template":      {Kind: schema.ParamAsset, Pass: schema.AssetPassContent},
		"chat-template-file": {Kind: schema.ParamAsset, Pass: schema.AssetPassPath},
	}

	out, err := localizeAssetParams("mlx", map[string]string{"chat-template": "t.jinja", "chat-template-file": "t.jinja"}, shapes, dir.Path)
	require.NoError(t, err)
	assert.Equal(t, "{% for m in messages %}\n{{ m.content }}{% endfor %}", out.Params["chat-template"])
	path, err := dir.Path("t.jinja")
	require.NoError(t, err)
	assert.Equal(t, path, out.Params["chat-template-file"])
	assert.Equal(t, out.Files["chat-template"], out.Files["chat-template-file"], "the digest is of the content either way")

	for _, name := range []string{"zero.jinja", "big.jinja"} {
		_, err = localizeAssetParams("mlx", map[string]string{"chat-template": name}, shapes, dir.Path)
		assert.Error(t, err, name)
	}
}

func schemaLaunchManager(t *testing.T, store *pkgConfig.AppsConfigStore, provider string) *prov_apps.ProviderAppManager {
	t.Helper()
	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp(provider, pkgConfig.ServiceConfig{
		Enabled: new(true), Name: provider, Protocol: pkgConfig.ProtocolOpenAI, Mode: "on-demand",
		Runtime: &pkgConfig.AppRuntimeConfig{
			PortRange: []int{8100, 8105}, BasePort: 8100,
			Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "engine"},
		},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	m, err := prov_apps.NewProviderAppManager(cfg,
		prov_apps.WithProviderSchema(providerSchema(store)),
		prov_apps.WithParamLocalizer(assetParamLocalizer(store)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, m.Stop(context.Background())) })
	return m
}

func TestContextAutoSpellsOnlyArgv(t *testing.T) {
	root := t.TempDir()
	seedTestProviders(t, root, []string{"llamacpp", "vllm"}, nil)
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	for _, tc := range []struct{ provider, key, spelling, served string }{
		{"vllm", "max-model-len", "auto", "max_model_len"},
		{"llamacpp", "ctx-size", "0", "meta.n_ctx"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			merged, err := providerSchema(store)(tc.provider)
			require.NoError(t, err)
			assert.Equal(t, tc.spelling, merged.Parameters[tc.key].Auto)
			assert.Equal(t, tc.served, merged.Parameters[tc.key].Served)
			dto := paramShapeMapToDTO(merged.Parameters)[tc.key]
			assert.Equal(t, tc.spelling, dto.Auto)
			assert.Equal(t, tc.served, dto.Served)
			cfg, ok := store.Config().LookupApp(tc.provider)
			require.True(t, ok)
			assert.Equal(t, "auto", cfg.Defaults.Parameters[tc.key])
			input := map[string]string{tc.key: "auto"}
			if tc.provider == "llamacpp" {
				input["chat-template-file"] = "t.jinja"
				_, err = store.WriteAsset(tc.provider, "t.jinja", []byte("template"))
				require.NoError(t, err)
			}
			m := schemaLaunchManager(t, store, tc.provider)
			out, err := m.PreviewLaunch(t.Context(), prov_apps.EnsureRequest{Provider: tc.provider, Model: "m", Parameters: input}, 8100)
			require.NoError(t, err)
			assert.Contains(t, out.Args, "--"+tc.key)
			assert.Contains(t, out.Args, tc.spelling)
			assert.Equal(t, "auto", out.Params[tc.key])
			assert.Equal(t, "auto", input[tc.key])
			if tc.provider == "llamacpp" {
				assert.Contains(t, out.Args, filepath.Join(root, "on-demand", tc.provider, "assets", "t.jinja"))
			}
		})
	}
}

func TestLaunchSchemaErrorsRemainHardFailures(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"unreadable", ""},
		{"malformed", ":\n:not yaml"},
		{"invalid diagnostics", "diagnostics: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			seedTestProviders(t, root, []string{"vllm"}, nil)
			store, err := pkgConfig.NewAppsConfigStore(root)
			require.NoError(t, err)
			path := filepath.Join(root, "on-demand", "vllm", "schema.yaml")
			if tc.name == "unreadable" {
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0700))
			} else {
				require.NoError(t, os.WriteFile(path, []byte(tc.data), 0600))
			}
			_, err = providerSchema(store)("vllm")
			require.Error(t, err)
			assert.Equal(t, schema.ParamInt, loadProviderSchema(store, "vllm").Parameters["max-model-len"].Kind)
			m := schemaLaunchManager(t, store, "vllm")
			_, err = m.PreviewLaunch(t.Context(), prov_apps.EnsureRequest{Provider: "vllm", Model: "m"}, 8100)
			require.Error(t, err)
		})
	}
}

func TestLaunchSchemaShapeConflictsKeepValidEntries(t *testing.T) {
	root := t.TempDir()
	seedTestProviders(t, root, []string{"vllm"}, nil)
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "on-demand", "vllm", "schema.yaml"), []byte(`parameters:
  max-model-len: {type: string, auto: broken}
  invalid: {type: unknown}
  context: {type: int, auto: "0"}
`), 0600))
	merged, err := providerSchema(store)("vllm")
	require.NoError(t, err)
	assert.Equal(t, schema.ParamInt, merged.Parameters["max-model-len"].Kind)
	assert.Empty(t, merged.Parameters["max-model-len"].Auto)
	assert.NotContains(t, merged.Parameters, "invalid")
	assert.Equal(t, "0", merged.Parameters["context"].Auto)
	assert.Equal(t, merged, loadProviderSchema(store, "vllm"))
	m := schemaLaunchManager(t, store, "vllm")
	out, err := m.PreviewLaunch(t.Context(), prov_apps.EnsureRequest{
		Provider: "vllm", Model: "m", Parameters: map[string]string{"context": "auto", "max-model-len": "8192"},
	}, 8100)
	require.NoError(t, err)
	assert.Contains(t, out.Args, "0")
	assert.Contains(t, out.Args, "8192")
	assert.Equal(t, "auto", out.Params["context"])
}

func TestProviderSchemaInvalidDiagnosticsKeepLongTailShapes(t *testing.T) {
	root := t.TempDir()
	seedTestProviders(t, root, []string{"vllm"}, nil)
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "on-demand", "vllm", "schema.yaml"), []byte(`parameters:
  long-tail: {type: int, min: 1}
  chat-template: {type: asset}
endpoints:
  embeddings:
    parameters:
      pooling: {type: bool}
diagnostics:
  runtime: test
  runtimes:
    test: {kernels: unknown}
  memory: {kind: broken, parameter: gpu-memory-utilization}
`), 0600))
	merged, err := providerSchema(store)("vllm")
	require.ErrorContains(t, err, "unknown memory budget")
	require.NotNil(t, merged)
	assert.Nil(t, merged.Diagnostics)
	assert.Equal(t, schema.ParamInt, merged.Parameters["long-tail"].Kind)
	assert.Equal(t, schema.ParamAsset, merged.Parameters["chat-template"].Kind)
	assert.Equal(t, schema.ParamBool, merged.ForEndpoint("embeddings")["pooling"].Kind)
	available := loadProviderSchema(store, "vllm")
	assert.Equal(t, merged, available)
	assert.Nil(t, validateLeaf("long-tail", raw(4), false, available.Parameters))
	assert.Equal(t, "int", paramShapeMapToDTO(available.Parameters)["long-tail"].Type)
	assert.Empty(t, available.Parameters["gpu-memory-utilization"].Auto)
	m := schemaLaunchManager(t, store, "vllm")
	_, err = m.PreviewLaunch(t.Context(), prov_apps.EnsureRequest{Provider: "vllm", Model: "m"}, 8100)
	require.ErrorContains(t, err, "unknown memory budget")
}

func assertCacheWarning(t *testing.T, provider string, action func()) {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	func() {
		defer slog.SetDefault(previous)
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
		action()
	}()
	require.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("\n")), logs.String())
	var record map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
	assert.Equal(t, "WARN", record["level"])
	assert.Equal(t, provider, record["provider"])
	assert.NotEmpty(t, record["error"])
}

func TestInvalidCacheDeclarationsDoNotBlockLaunch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "on-demand", "fake")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`enabled: true
protocol: openai
runtime:
  base_port: 8780
  port_range: [8780, 8789]
  execution: {type: cli, command: engine}
capabilities:
  wire_endpoints: [chat_completions]
`), 0600))
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	for _, declaration := range []string{"tokens: '('", "unknown: '(x)'", "tokens: '(x)(y)'"} {
		t.Run(declaration, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "schema.yaml"), []byte("parameters:\n  long-tail: {type: int}\ncache_log:\n  concurrency: 'Maximum concurrency for ([0-9]+) tokens per request: ([0-9.]+)x'\n  "+declaration+"\n"), 0600))
			assertCacheWarning(t, "fake", func() {
				merged, err := providerSchema(store)("fake")
				require.NoError(t, err)
				assert.Nil(t, merged.CacheLog)
				assert.Equal(t, schema.ParamInt, merged.Parameters["long-tail"].Kind)
			})
			r := newReadinessRig(t, prov_apps.WithProviderSchema(providerSchema(store)))
			script := strings.Replace(crashingEngine, `printf 'READY\n'`, `printf 'GPU KV cache size: 814421 tokens\nMaximum concurrency for 32768 tokens per request: 24.85x\nREADY\n'`, 1)
			require.NoError(t, os.WriteFile(r.engine, []byte(script), 0700))
			require.NoError(t, os.WriteFile(r.engine+".crashed", nil, 0600))
			result, err := r.mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "fake", Model: "m", Parameters: map[string]string{"long-tail": "7"}})
			require.NoError(t, err)
			select {
			case <-result.Run.LeftStarting():
			case <-time.After(readinessWait):
				t.Fatal("run never became ready")
			}
			assert.Equal(t, instance.StatusRunning, result.Run.GetStatus())
			assert.Nil(t, result.Run.ToInfo("").Cache)
			assert.Equal(t, "7", result.Run.Resolved().Parameters["long-tail"])
		})
	}
}
