package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/apipath"
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

// "auto" is how an operator drops a template a lower tier set. It names no
// asset, so it must not be refused as one that does not exist.
func TestPatch_AutoDropsAnInheritedTemplate(t *testing.T) {
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

// mlx_lm.server takes the template's text, so an MLX asset reaches the
// engine as content, not as a path.
func TestMLXTemplateLocalizesToContent(t *testing.T) {
	s, _ := assetTestNode(t)
	const template = "{{ messages }}"
	_, err := s.configStore.WriteAsset("mlx", "t.jinja", []byte(template))
	require.NoError(t, err)
	out, err := assetParamLocalizer(s.configStore)("mlx", "chat", "", map[string]string{"chat-template": "t.jinja"})
	require.NoError(t, err)
	assert.Equal(t, template, out.Params["chat-template"])
}
