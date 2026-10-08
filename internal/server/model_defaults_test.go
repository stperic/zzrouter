package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/httperr"
)

const qwenModel = "Qwen3.8-27B-Q8_0"

func resolvedFor(t *testing.T, s *Server, provider, model string) resolvedResponse {
	t.Helper()
	w := assetRequest(t, s, http.MethodGet, "/zzrouter/v1/providers/"+provider+"/resolved?model="+url.QueryEscape(model), nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	var r resolvedResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
	return r
}

// An agent asking where a value comes from sees that the release supplied
// it, which family pattern matched, and which bytes the launch will get;
// once an operator tier takes over, it says so.
func TestResolved_ReportsTheShippedModelDefault(t *testing.T) {
	s, _ := assetTestNode(t)
	shipped, err := templates.AppsFS.ReadFile("files/providers/on-demand/llamacpp/assets/" + shippedLlamacppTemplate)
	require.NoError(t, err)

	got := resolvedFor(t, s, "llamacpp", qwenModel).Parameters["chat-template-file"]
	assert.Equal(t, shippedLlamacppTemplate, got.Value)
	assert.Equal(t, "model-default", got.Tier)
	assert.Equal(t, "qwen3.8-*", got.Pattern)
	assert.Equal(t, qwenModel, got.Model)
	assert.Equal(t, assets.Digest(shipped), got.SHA256)

	assert.NotContains(t, resolvedFor(t, s, "llamacpp", "Qwen3-8B-Q4_K_M").Parameters, "chat-template-file")

	resp := makeAuthRequest(t, s, "PATCH", apipath.ProviderParameters("llamacpp"), TestAdminKey, map[string]any{
		"models": map[string]any{qwenModel: map[string]any{"parameters": map[string]any{"chat-template-file": "t.jinja"}}},
	})
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)
	got = resolvedFor(t, s, "llamacpp", qwenModel).Parameters["chat-template-file"]
	assert.Equal(t, "t.jinja", got.Value)
	assert.Equal(t, "model", got.Tier)
	assert.Empty(t, got.Pattern, "an exact model key needs no pattern")
}

// "auto" is how an operator turns the shipped template off. It names no
// asset, so it must not be refused as one that does not exist.
func TestPatch_AutoTurnsAShippedTemplateOff(t *testing.T) {
	s, _ := assetTestNode(t)
	resp := makeAuthRequest(t, s, "PATCH", apipath.ProviderParameters("llamacpp"), TestAdminKey, map[string]any{
		"models": map[string]any{qwenModel: map[string]any{"parameters": map[string]any{"chat-template-file": "auto"}}},
	})
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)
	got := resolvedFor(t, s, "llamacpp", qwenModel).Parameters["chat-template-file"]
	assert.Equal(t, "auto", got.Value)
	assert.Equal(t, "model", got.Tier)
	assert.Empty(t, got.SHA256)

	// A real name that is absent is still refused.
	resp = makeAuthRequest(t, s, "PATCH", apipath.ProviderParameters("llamacpp"), TestAdminKey, map[string]any{
		"models": map[string]any{qwenModel: map[string]any{"parameters": map[string]any{"chat-template-file": "absent.jinja"}}},
	})
	require.Equal(t, http.StatusBadRequest, resp.Code)
	assert.Contains(t, string(resp.Body), string(httperr.CodeUnknownAsset))
}

// model_defaults is the release's; the write API refuses it and says what
// to do instead.
func TestPatch_ModelDefaultsIsNotWritable(t *testing.T) {
	s, _ := assetTestNode(t)
	resp := makeAuthRequest(t, s, "PATCH", apipath.ProviderParameters("llamacpp"), TestAdminKey, map[string]any{
		"model_defaults": map[string]any{"qwen3.8-*": map[string]any{"parameters": map[string]any{"chat-template-file": "t.jinja"}}},
	})
	require.Equal(t, http.StatusBadRequest, resp.Code)
	body := string(resp.Body)
	assert.Contains(t, body, string(httperr.CodeUnknownField))
	assert.Contains(t, body, `set the key under models.\u003cmodel\u003e`)
}

// The asset listing names the shipped default among a template's users,
// so nobody is surprised which models use it.
func TestAssetsList_NamesModelDefaultReferences(t *testing.T) {
	s, _ := assetTestNode(t)
	w := assetRequest(t, s, http.MethodGet, "/zzrouter/v1/providers/llamacpp/assets", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	assert.Contains(t, w.Body.String(), `"model_defaults.qwen3.8-*.parameters.chat-template-file"`)
}

// The shipped MLX config reaches the engine as text end to end.
func TestShippedMLXTemplateLocalizesToContent(t *testing.T) {
	s, _ := assetTestNode(t)
	shipped, err := templates.AppsFS.ReadFile("files/providers/on-demand/mlx/assets/" + shippedLlamacppTemplate)
	require.NoError(t, err)
	out, err := assetParamLocalizer(s.configStore)("mlx", "chat", "", map[string]string{"chat-template": shippedLlamacppTemplate})
	require.NoError(t, err)
	assert.Equal(t, string(shipped), out.Params["chat-template"])
}
