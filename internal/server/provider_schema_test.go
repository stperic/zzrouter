package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps"
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
	require.NoError(t, templates.InstallDefaults(root))
	providerDir := filepath.Join(root, "on-demand", "llamacpp")
	require.NoError(t, os.WriteFile(filepath.Join(providerDir, "schema.yaml"), []byte(
		"parameters:\n  chat-template-file: {type: asset}\n"+
			"endpoints:\n  embeddings:\n    parameters:\n      pooling-file: {type: asset}\n"), 0o600))
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	_, err = store.WriteAsset("llamacpp", "t.jinja", []byte("x"))
	require.NoError(t, err)
	localize := assetParamLocalizer(store)

	out, err := localize("llamacpp", "chat", "", map[string]string{"chat-template-file": "t.jinja", "pooling-file": "t.jinja"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(providerDir, "assets", "t.jinja"), out.Params["chat-template-file"])
	assert.Equal(t, "t.jinja", out.Params["pooling-file"], "an overlay key is not an asset on another endpoint")

	out, err = localize("llamacpp", "embeddings", "", map[string]string{"pooling-file": "t.jinja"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(providerDir, "assets", "t.jinja"), out.Params["pooling-file"])

	_, err = localize("llamacpp", "chat", "", map[string]string{"chat-template-file": "absent.jinja"})
	assert.ErrorIs(t, err, assets.ErrNotFound)

	untouched := map[string]string{"chat-template-file": "t.jinja"}
	out, err = assetParamLocalizer(nil)("llamacpp", "chat", "", untouched)
	require.NoError(t, err)
	assert.Equal(t, untouched, out.Params, "with no store there is no schema declaring an asset")
}

// The node's provider manager resolves asset parameters: a launch naming
// an asset this node does not have is refused before any argv is built.
func TestServerManagerLocalizesAssetParams(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.SeedProvidersDir = true
	server := createTestNode(t, cfg)
	require.NoError(t, server.configStore.SetProviderEnabled("llamacpp", true))

	_, err := server.providers.appMgr.LaunchInstance(t.Context(), prov_apps.LaunchRequest{
		Provider: "llamacpp", Model: "m",
		Parameters: map[string]string{"chat-template-file": "absent.jinja"},
	})
	require.ErrorIs(t, err, prov_apps.ErrParameterValidation)
	assert.ErrorIs(t, err, assets.ErrNotFound)
}

// What the release ships fits together: the shipped schemas declare the
// engines' template flags as assets, and the shipped template resolves
// through them to the file installed beside the provider's config.
func TestShippedTemplateParamsResolveToShippedAsset(t *testing.T) {
	const shippedTemplate = "qwen3.8-system-anywhere.jinja"
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	require.True(t, templates.IsShippedAsset("llamacpp", shippedTemplate))

	out, err := assetParamLocalizer(store)("llamacpp", "chat", "", map[string]string{"chat-template-file": shippedTemplate})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "on-demand", "llamacpp", assets.DirName, shippedTemplate), out.Params["chat-template-file"])

	assert.Equal(t, schema.ParamAsset, loadProviderSchema(store, "vllm").Parameters["chat-template"].Kind)
}

// A provider's schema is its own schema.yaml: another provider's broken
// one, or an asset named like a schema, has no bearing on it.
func TestLoadProviderSchema_ReadsOnlyThatProvider(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
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
	require.NoError(t, templates.InstallDefaults(root))
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
