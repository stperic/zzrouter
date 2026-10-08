package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

const (
	variantAgent = qwenModel + "+agent"
	variantChat  = qwenModel + "+chat"
)

// variantNode is a coordinator holding Qwen3.8 weights on disk, with
// llamacpp enabled.
func variantNode(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, qwenModel+".gguf"), []byte("GGUF"), 0o600))
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	s, _ := assetTestNode(t)
	require.NoError(t, s.configStore.SetProviderEnabled("llamacpp", true))
	return s
}

func patchParams(t *testing.T, s *Server, body map[string]any) (int, string) {
	t.Helper()
	resp := makeAuthRequest(t, s, "PATCH", apipath.ProviderParameters("llamacpp"), TestAdminKey, body)
	return resp.Code, string(resp.Body)
}

func defineVariants(t *testing.T, s *Server) {
	t.Helper()
	code, body := patchParams(t, s, map[string]any{"models": map[string]any{
		qwenModel: map[string]any{"parameters": map[string]any{"ctx-size": 32768}},
		variantAgent: map[string]any{
			"from":       qwenModel,
			"parameters": map[string]any{"ctx-size": 131072},
			"request":    map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
		},
		variantChat: map[string]any{"from": qwenModel, "request": map[string]any{"temperature": 0.7}},
	}})
	require.Equal(t, http.StatusOK, code, body)
	// no manual invalidate: a config write must reach the catalog by itself
}

// One call tells an agent what a variant launches and which request
// defaults it carries, and where each came from.
func TestVariants_ResolvedDescribesTheVariant(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)

	r := resolvedFor(t, s, "llamacpp", variantAgent)
	assert.Equal(t, qwenModel, r.From)
	assert.Equal(t, "131072", r.Parameters["ctx-size"].Value)
	assert.Equal(t, variantAgent, r.Parameters["ctx-size"].Model)
	assert.Equal(t, map[string]any{"enable_thinking": false}, r.Request["chat_template_kwargs"].Value)
	assert.Equal(t, "model", r.Request["chat_template_kwargs"].Tier)

	base := resolvedFor(t, s, "llamacpp", qwenModel)
	assert.Empty(t, base.From)
	assert.Empty(t, base.Request)
}

// A variant has no weights of its own, so its first request, the one
// that launches it, must load its base's while the run keeps its name.
func TestVariants_LaunchLoadsTheBaseWeights(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)

	res, err := s.buildInstanceConfig(t.Context(), variantAgent, "llamacpp", "chat", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, variantAgent, res.LaunchReq.Model, "the run is the variant's")
	require.Len(t, res.LaunchReq.Files, 1)
	assert.Equal(t, qwenModel+".gguf", filepath.Base(res.LaunchReq.Files[0].NodePath))
}

// A write that touches one model answers with that model's resolution,
// so the writer reads back what it wrote, not the provider defaults.
func TestVariants_PatchAnswersWithTheCellWritten(t *testing.T) {
	s := variantNode(t)
	code, body := patchParams(t, s, map[string]any{"models": map[string]any{
		variantAgent: map[string]any{"from": qwenModel, "parameters": map[string]any{"ctx-size": 131072}},
	}})
	require.Equal(t, http.StatusOK, code, body)
	var got resolvedResponse
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	want := resolvedFor(t, s, "llamacpp", variantAgent)
	assert.Equal(t, qwenModel, got.From)
	assert.Equal(t, want.Parameters, got.Parameters)
	assert.Equal(t, "131072", got.Parameters["ctx-size"].Value)
}

func TestPatchView(t *testing.T) {
	t.Parallel()
	cell := &modelPatch{}
	cases := map[string]struct {
		patch     paramPatchBody
		node, mdl string
	}{
		"one model":     {paramPatchBody{Models: map[string]*modelPatch{"m": cell}}, "", "m"},
		"deleted model": {paramPatchBody{Models: map[string]*modelPatch{"m": nil}}, "", "m"},
		"two models":    {paramPatchBody{Models: map[string]*modelPatch{"a": cell, "b": cell}}, "", ""},
		"glob model":    {paramPatchBody{Models: map[string]*modelPatch{"[ab]*": cell}}, "", ""},
		"one node":      {paramPatchBody{Nodes: map[string]*nodePatch{"n": {}}}, "n", ""},
		"node x model":  {paramPatchBody{Nodes: map[string]*nodePatch{"n": {Models: map[string]*specPatch{"m": {}}}}}, "n", "m"},
		"model wins":    {paramPatchBody{Models: map[string]*modelPatch{"m": cell}, Nodes: map[string]*nodePatch{"n": {Models: map[string]*specPatch{"x": {}}}}}, "n", "m"},
		"defaults only": {paramPatchBody{Defaults: &defaultsPatch{}}, "", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node, model := patchView(&tc.patch)
			assert.Equal(t, tc.node, node)
			assert.Equal(t, tc.mdl, model)
		})
	}
}

// The router's process decision, on the node that serves: a variant
// that adds only request defaults is its base's process, one that
// changes the launch is its own.
func TestVariants_CanonicalNameIsTheServingProcess(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)

	assert.Equal(t, qwenModel, s.resolveCanonicalModelName(variantChat))
	assert.Equal(t, variantAgent, s.resolveCanonicalModelName(variantAgent))
	assert.Equal(t, qwenModel, s.resolveCanonicalModelName(qwenModel))

	// Giving the base the variant's ctx-size makes them one launch.
	code, body := patchParams(t, s, map[string]any{"models": map[string]any{
		qwenModel: map[string]any{"parameters": map[string]any{"ctx-size": 131072}},
	}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, qwenModel, s.resolveCanonicalModelName(variantAgent))
}

// A variant is in the catalog wherever its weights are, so an agent
// finds it by listing models and the router can route it.
func TestVariants_ListedInV1Models(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)

	m, err := s.LookupModel(context.Background(), variantAgent)
	require.NoError(t, err)
	assert.Equal(t, qwenModel, m.VariantOf)
	assert.Equal(t, "llamacpp", m.Provider)

	resp := makeAuthRequest(t, s, "GET", "/v1/models", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code)
	var list struct {
		Data []OpenAIModelObject `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &list))
	byID := map[string]OpenAIModelObject{}
	for _, o := range list.Data {
		byID[o.ID] = o
	}
	node := s.node.Nodename()
	require.Contains(t, byID, variantAgent+"@"+node)
	assert.Equal(t, qwenModel+"@"+node, byID[variantAgent+"@"+node].VariantOf)
	assert.Empty(t, byID[qwenModel+"@"+node].VariantOf)
}

// Request defaults reach the engine for the name the client addressed;
// a field the client sent wins.
func TestVariants_RequestDefaultsApplyToTheAddressedName(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)

	body := func(model string, extra map[string]any) map[string]any {
		b := map[string]any{"model": model, "messages": []any{}}
		for k, v := range extra {
			b[k] = v
		}
		raw, err := json.Marshal(b)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(s.applyRequestDefaults(context.Background(), "llamacpp", model, "/v1/chat/completions", raw), &out))
		return out
	}
	got := body(variantAgent, nil)
	assert.Equal(t, map[string]any{"enable_thinking": false}, got["chat_template_kwargs"])

	got = body(variantChat, map[string]any{"temperature": 0.1})
	assert.Equal(t, 0.1, got["temperature"], "the client's value wins")
	got = body(variantChat, nil)
	assert.Equal(t, 0.7, got["temperature"])

	got = body(qwenModel, nil)
	assert.NotContains(t, got, "temperature", "the base has none of its variants' defaults")

	// A route that does not generate text gets no sampling fields.
	raw := []byte(`{"model":"` + variantChat + `","input":"x"}`)
	assert.Equal(t, raw, s.applyRequestDefaults(context.Background(), "llamacpp", variantChat, "/v1/embeddings", raw))
	assert.Contains(t, string(s.applyRequestDefaults(context.Background(), "llamacpp", variantChat, "/v1/messages", raw)), "temperature")
}

// request merges per RFC 7396; from: null turns a variant back into a
// plain entry, and an entry left with nothing is dropped.
func TestVariants_PatchSemantics(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)

	code, body := patchParams(t, s, map[string]any{"models": map[string]any{
		variantAgent: map[string]any{"request": map[string]any{
			"chat_template_kwargs": map[string]any{"enable_thinking": nil, "reasoning_effort": "low"},
			"top_p":                0.9,
		}},
	}})
	require.Equal(t, http.StatusOK, code, body)
	r := resolvedFor(t, s, "llamacpp", variantAgent)
	assert.Equal(t, map[string]any{"reasoning_effort": "low"}, r.Request["chat_template_kwargs"].Value)
	assert.Equal(t, 0.9, r.Request["top_p"].Value)

	code, body = patchParams(t, s, map[string]any{"models": map[string]any{variantChat: map[string]any{"from": nil, "request": nil}}})
	require.Equal(t, http.StatusOK, code, body)
	cfg, ok := s.configStore.Config().LookupApp("llamacpp")
	require.True(t, ok)
	assert.NotContains(t, cfg.Models, variantChat, "nothing left, so the entry is gone")
	assert.Contains(t, cfg.Models, variantAgent)
}

func TestVariants_PatchRefusals(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)

	cases := map[string]struct {
		body    map[string]any
		key     string
		code    httperr.ParamErrorCode
		wantKey bool
	}{
		"glob name": {map[string]any{"models": map[string]any{"qwen*": map[string]any{"from": qwenModel}}},
			"models.qwen*.from", httperr.CodeInvalidValue, true},
		"bracket name": {map[string]any{"models": map[string]any{qwenModel + "[claude]": map[string]any{"from": qwenModel}}},
			"models." + qwenModel + "[claude].from", httperr.CodeInvalidValue, true},
		"tag name": {map[string]any{"models": map[string]any{"qwen:agent": map[string]any{"from": qwenModel}}},
			"models.qwen:agent.from", httperr.CodeInvalidValue, true},
		"itself": {map[string]any{"models": map[string]any{"solo": map[string]any{"from": "solo"}}},
			"models.solo.from", httperr.CodeInvalidValue, true},
		"variant of a variant": {map[string]any{"models": map[string]any{"deep": map[string]any{"from": variantAgent}}},
			"models.deep.from", httperr.CodeInvalidValue, true},
		"base becomes a variant": {map[string]any{"models": map[string]any{qwenModel: map[string]any{"from": "other"}}},
			"models." + qwenModel + ".from", httperr.CodeInvalidValue, true},
		"from not a string": {map[string]any{"models": map[string]any{"x": map[string]any{"from": 3}}},
			"models.x.from", httperr.CodeWrongType, true},
		"reserved request field": {map[string]any{"models": map[string]any{variantChat: map[string]any{"request": map[string]any{"messages": []any{}}}}},
			"models." + variantChat + ".request.messages", httperr.CodeInvalidValue, true},
		"request not an object": {map[string]any{"models": map[string]any{variantChat: map[string]any{"request": "hot"}}},
			"models." + variantChat + ".request", httperr.CodeWrongType, true},
		"from at a node tier": {map[string]any{"nodes": map[string]any{s.node.Nodename(): map[string]any{
			"models": map[string]any{variantAgent: map[string]any{"from": qwenModel}}}}},
			"from", httperr.CodeUnknownField, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, body := patchParams(t, s, tc.body)
			require.Equal(t, http.StatusBadRequest, code, body)
			var p struct {
				Errors []struct {
					Key  string `json:"key"`
					Code string `json:"code"`
				} `json:"errors"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &p))
			require.NotEmpty(t, p.Errors, body)
			assert.Equal(t, tc.key, p.Errors[0].Key, body)
			assert.Equal(t, string(tc.code), p.Errors[0].Code, body)
		})
	}

	// Nothing was written by any of them.
	cfg, _ := s.configStore.Config().LookupApp("llamacpp")
	assert.NotContains(t, cfg.Models, "deep")
	assert.Empty(t, cfg.Models[qwenModel].From)
}

// A variant name selects one engine.
func TestVariants_NameIsUniqueAcrossProviders(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	resp := makeAuthRequest(t, s, "PATCH", apipath.ProviderParameters("vllm"), TestAdminKey, map[string]any{
		"models": map[string]any{variantAgent: map[string]any{"from": "Qwen/Qwen3.8-27B"}},
	})
	require.Equal(t, http.StatusBadRequest, resp.Code, string(resp.Body))
	assert.Contains(t, string(resp.Body), "provider llamacpp already defines variant")
}

// End to end on one node: a request for a variant that adds only request
// defaults is served by its base's running process, with no launch, and
// the engine receives the defaults the client left out.
func TestVariants_SharedVariantIsServedByTheBaseProcess(t *testing.T) {
	var seen map[string]any
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(engine.Close)

	s := variantNode(t)
	defineVariants(t, s)
	u, err := url.Parse(engine.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	inst := instance.NewInstance("base-run", "llamacpp", qwenModel, port, 0, 0)
	inst.Endpoint = "chat"
	inst.MarkRunning()
	require.NoError(t, s.providers.appMgr.Instances().Register(inst))

	node := httptest.NewServer(s.engine)
	t.Cleanup(node.Close)
	req, err := http.NewRequest(http.MethodPost, node.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+variantChat+`","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", TestAdminKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", out)

	require.NotNil(t, seen, "the base's process served the variant")
	assert.Equal(t, 0.7, seen["temperature"], "the variant's request default reached the engine")
	assert.Len(t, s.providers.appMgr.Instances().List(), 1, "no second process")
	assert.Contains(t, string(out), `"model":"`+variantChat+`"`, "the client sees the name it asked for")
}

// A variant cannot take a name real weights carry: the launch would load
// its base and serve those under the real model's name.
func TestVariants_NameOfRealWeightsIsRefused(t *testing.T) {
	s := variantNode(t)
	code, body := patchParams(t, s, map[string]any{"models": map[string]any{
		qwenModel: map[string]any{"from": "Qwen3.8-27B-Q4_K_M"},
	}})
	require.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, `"key":"models.`+qwenModel+`.from"`)
	assert.Contains(t, body, "names real weights on "+s.node.Nodename())

	// Re-patching an existing variant is not a collision with itself.
	defineVariants(t, s)
	code, body = patchParams(t, s, map[string]any{"models": map[string]any{
		variantChat: map[string]any{"from": qwenModel, "request": map[string]any{"top_p": 0.8}},
	}})
	require.Equal(t, http.StatusOK, code, body)
}

// The group chain finds a variant's process the way a direct request
// does: a variant its base's process serves resolves to that run.
func TestVariants_ChainFindsTheServingRun(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	inst := instance.NewInstance("base-run", "llamacpp", qwenModel, 18080, 0, 0)
	inst.Endpoint = "chat"
	inst.MarkRunning()
	require.NoError(t, s.providers.appMgr.Instances().Register(inst))

	deps := &chainServerDeps{s: s}
	got, ok := deps.AppInstance(variantChat)
	require.True(t, ok)
	assert.Equal(t, "base-run", got.ID)
	_, ok = deps.AppInstance(variantAgent)
	assert.False(t, ok, "a variant with its own launch has no run yet")
	assert.Equal(t, 0.7, deps.RequestDefaults("llamacpp", variantChat, "/v1/chat/completions")["temperature"])
}

// An instance launched under a display spelling of its provider still
// gets the provider's request defaults.
func TestVariants_RequestDefaultsFollowProviderSpelling(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	got := s.requestDefaults(context.Background(), "llama.cpp", variantChat, "/v1/chat/completions")
	assert.Equal(t, 0.7, got["temperature"])
}

// A provider config change reaches the catalog by itself. The cache's
// listener used to be registered before the cache existed, so it never
// was, and the catalog kept what it had until something else refreshed it.
func TestModelCacheHearsProviderConfigChanges(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	_, err := s.LookupModel(context.Background(), variantAgent)
	require.NoError(t, err)

	require.NoError(t, s.configStore.SetProviderEnabled("llamacpp", false))
	_, err = s.LookupModel(context.Background(), variantAgent)
	assert.Error(t, err, "a disabled provider's variants leave the catalog")
}

// A cold load that outlasts the grace period streams through
// streamFromInstance, not proxyToInstance. A variant served by its base's
// process must reach the engine the same way on that path: with its
// request defaults and the engine's own token for the model, and the
// client must get its own name back.
func TestVariants_ColdStreamSendsWhatTheWarmPathSends(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)

	const wireName = "engine-token"
	var seen map[string]any
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"model":"`+wireName+`","choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(engine.Close)
	u, err := url.Parse(engine.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)

	inst := instance.NewInstance("cold-stream", "llamacpp", qwenModel, port, 0, 4)
	inst.WireModel = wireName
	inst.SetStatus(instance.StatusRunning)

	body := []byte(`{"model":"` + variantChat + `","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	ctx := context.WithValue(req.Context(), CtxKeyModel, variantChat)
	req = req.WithContext(context.WithValue(ctx, CtxKeyOriginalBody, body))
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	s.streamFromInstance(w, req, inst, dialectOf(req).(httperr.InBandStreamer))

	require.NotNil(t, seen, "the engine was reached")
	assert.Equal(t, 0.7, seen["temperature"], "the variant's request defaults")
	assert.Equal(t, wireName, seen["model"], "the engine's own token")
	assert.Contains(t, w.Body.String(), `"model":"`+variantChat+`"`, "the client's name back")
	assert.NotContains(t, w.Body.String(), wireName)
}

func TestVariants_FallbackUsesServingInstancesWireModel(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	seen := make(chan map[string]any, 1)
	const wireName = "engine-base-token"
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seen <- body
		w.Header().Set("Content-Type", "application/json")
		if body["model"] != wireName {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"unknown model"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"r1","model":"`+wireName+`","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(engine.Close)
	u, err := url.Parse(engine.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	inst := instance.NewInstance("fallback-base", "llamacpp", qwenModel, port, 0, 0)
	inst.WireModel = wireName
	inst.Endpoint = "chat"
	inst.MarkRunning()
	require.NoError(t, s.providers.appMgr.Instances().Register(inst))
	raw := []byte(`{"model":"agent-group","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw)))
	rec := llm.NewInferenceRecorder(c.Request.Context(), "agent-group", "")
	resolved := &resolver.Resolved{OriginalName: "agent-group", GroupName: "agent-group", Strategy: "priority",
		Candidates: []fallback.Candidate{{Name: "variant", App: "llamacpp", Model: variantChat, Node: s.node.Nodename(), OnDemand: true}}}
	s.providers.fallback.ProxyWithFallback(c, resolved, raw, rec, "agent-group", false)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	sent := <-seen
	assert.Equal(t, wireName, sent["model"])
	assert.Equal(t, 0.7, sent["temperature"])
	var reply map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &reply))
	assert.Equal(t, "agent-group", reply["model"])
}
