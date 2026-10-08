package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

func conflictRun(t *testing.T, s *Server, model string) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"base","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(engine.Close)
	u, err := url.Parse(engine.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	inst := instance.NewInstance("conflict-run", "llamacpp", model, port, 0, 0)
	inst.Endpoint = "chat"
	inst.MarkRunning()
	require.NoError(t, s.providers.appMgr.Instances().Register(inst))
	return calls
}

func addConflictingWeights(t *testing.T, s *Server, name string) {
	t.Helper()
	root, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, strings.ToUpper(name)+".gguf"), []byte("GGUF"), 0o600))
	repo := filepath.Join(root, "owner", "weights")
	require.NoError(t, os.MkdirAll(repo, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, strings.ToUpper(name)+".gguf"), []byte("GGUF"), 0o600))
	s.model.Cache.Invalidate()
	require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
}

func TestVariants_LateConflictBlocksColdAndWarmPaths(t *testing.T) {
	for _, variant := range []string{variantChat, variantAgent} {
		t.Run(variant, func(t *testing.T) {
			s := variantNode(t)
			defineVariants(t, s)
			_, err := s.LookupModel(t.Context(), variant)
			require.NoError(t, err, "populate the catalog before weights arrive")
			serving := variant
			if variant == variantChat {
				serving = qwenModel
			}
			calls := conflictRun(t, s, serving)
			addConflictingWeights(t, s, variant)

			for _, refresh := range []bool{false, true} {
				if refresh {
					_, err = s.RescanLocal()
					require.NoError(t, err)
					require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
				}
				for _, name := range []string{variant, strings.ToUpper(variant), variant + "#Q4_K_M", variant + ".gguf", "owner/weights"} {
					resp := makeAuthRequest(t, s, "POST", "/v1/chat/completions", TestAdminKey, map[string]any{
						"model": name, "messages": []map[string]string{{"role": "user", "content": "hi"}},
					})
					require.Equal(t, http.StatusConflict, resp.Code, string(resp.Body))
					assert.Contains(t, string(resp.Body), `"code":"model_name_conflict"`)
					assert.Contains(t, string(resp.Body), "rename the variant")
					inst, err := s.providers.appMgr.LaunchInstance(t.Context(), prov_apps.LaunchRequest{Provider: "llamacpp", Model: name})
					require.ErrorIs(t, err, config.ErrModelNameConflict)
					assert.Nil(t, inst)
					// Force must not stop a warm process before checking admission.
					load, err := s.newLoadExecutor().LoadLocalModel(t.Context(), &LoadModelRequest{Provider: "llamacpp", ModelName: name, Force: true})
					require.ErrorIs(t, err, config.ErrModelNameConflict)
					assert.Nil(t, load)
				}
			}
			require.Zero(t, calls.Load(), "neither the base nor the variant engine may receive the conflict")
			require.Len(t, s.providers.appMgr.Instances().List(), 1, "force did not stop or replace the warm run")
			require.Equal(t, instance.StatusRunning, s.providers.appMgr.Instances().List()[0].GetStatus())

			// The cold path must refuse before reaching provider installation/exec.
			require.NoError(t, s.providers.appMgr.Instances().Remove("conflict-run"))
			_, err = s.providers.appMgr.LaunchInstance(t.Context(), prov_apps.LaunchRequest{Provider: "llamacpp", Model: variant})
			require.ErrorIs(t, err, config.ErrModelNameConflict)
			require.Empty(t, s.providers.appMgr.Instances().List())
		})
	}
}

func TestVariants_ConflictIsConsistentAcrossInferenceAndManagement(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	require.NoError(t, s.configStore.SetProviderEnabled("ollama", true))
	calls := conflictRun(t, s, qwenModel)
	addConflictingWeights(t, s, variantChat)
	for _, route := range []string{"/v1/completions", "/v1/embeddings", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens", "/api/chat", "/api/generate", "/api/embed", "/api/embeddings"} {
		t.Run(route, func(t *testing.T) {
			resp := makeAuthRequest(t, s, "POST", route, TestAdminKey, map[string]any{
				"model": variantChat, "messages": []map[string]string{{"role": "user", "content": "hi"}},
				"prompt": "hi", "input": "hi", "max_tokens": 16,
			})
			require.Equal(t, http.StatusConflict, resp.Code, string(resp.Body))
		})
	}
	ensure := makeAuthRequest(t, s, "POST", "/zzrouter/v1/runs/ensure", TestAdminKey, map[string]any{"model": variantChat})
	require.Equal(t, http.StatusConflict, ensure.Code, string(ensure.Body))
	launch := makeAuthRequest(t, s, "POST", "/zzrouter/v1/runs", TestAdminKey, map[string]any{"provider": "llamacpp", "model": variantChat, "auto_deploy": true, "launch_mode": "native"})
	require.Equal(t, http.StatusConflict, launch.Code, string(launch.Body))
	_, err := s.buildInstanceConfig(t.Context(), variantChat, "llamacpp", "chat", nil, nil)
	require.ErrorIs(t, err, config.ErrModelNameConflict)
	require.Zero(t, calls.Load())

	// Removing from makes the name weights again without changing syntax.
	code, body := patchParams(t, s, map[string]any{"models": map[string]any{variantChat: map[string]any{"from": nil}}})
	require.Equal(t, http.StatusOK, code, body)
	require.NoError(t, s.providers.appMgr.ValidateModel(t.Context(), variantChat))
}

func TestVariants_ConflictBlocksGroupAndWorkerWarmReuse(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	calls := conflictRun(t, s, qwenModel)
	_, err := s.LookupModel(t.Context(), variantChat)
	require.NoError(t, err)
	addConflictingWeights(t, s, variantChat)

	raw := []byte(`{"model":"group","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw)))
	httperr.AttachResponder(s.responders.openai)(c)
	rec := llm.NewInferenceRecorder(c.Request.Context(), "group", "")
	resolved := &resolver.Resolved{OriginalName: "group", GroupName: "group", Strategy: "priority",
		Candidates: []fallback.Candidate{{Name: "variant", App: "llamacpp", Model: variantChat, OnDemand: true}}}
	s.providers.fallback.ProxyWithFallback(c, resolved, raw, rec, "group", false)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"code":"model_name_conflict"`)
	require.Zero(t, calls.Load())

	// Actual HTTP to the worker-facing inference handler with a published
	// physical conflict and a provider selected upstream.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c, _ := gin.CreateTestContext(w)
		c.Request = r.WithContext(context.WithValue(r.Context(), CtxKeyOriginalBody, raw))
		httperr.AttachResponder(s.responders.openai)(c)
		_ = s.HandleLocalModel(w, c.Request, body.Model, "chat")
	}))
	defer worker.Close()
	req := httptest.NewRequest(http.MethodPost, worker.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+variantChat+`"}`))
	req.RequestURI = ""
	req.Header.Set(constants.HeaderServingProvider, "llamacpp")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Zero(t, calls.Load())
}

func TestVariants_PatchRequiresCatalogEvidence(t *testing.T) {
	s := variantNode(t)
	e := s.newParamsExecutor().WithCatalog(func(context.Context, string) (string, bool, error) {
		return "", false, io.ErrUnexpectedEOF
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/zzrouter/v1/providers/llamacpp/parameters", strings.NewReader(`{"models":{"fast":{"from":"base"}}}`))
	c.Params = gin.Params{{Key: "name", Value: "llamacpp"}}
	httperr.AttachResponder(s.responders.problem)(c)
	e.HandleParametersPatch(c)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	cfg, ok := s.appsConfig.LookupApp("llamacpp")
	require.True(t, ok)
	_, _, variant := cfg.Variant("fast")
	require.False(t, variant, "an unavailable catalog cannot authorize reserving a weights name")
}

func TestVariants_ConflictFilteringPrecedesWireEndpointPreference(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	cfg, ok := s.appsConfig.LookupApp("llamacpp")
	require.True(t, ok)
	cfg.Capabilities.WireEndpoints = []string{"chat_completions", "messages", "responses"}
	require.NoError(t, s.appsConfig.UpdateApp("llamacpp", func(service *config.ServiceConfig) error {
		service.Capabilities.WireEndpoints = cfg.Capabilities.WireEndpoints
		return nil
	}))
	require.NoError(t, s.appsConfig.AddApp("safe", config.ServiceConfig{
		Enabled: new(true), Mode: "cloud", Protocol: config.ProtocolOpenAI,
		Runtime:      &config.AppRuntimeConfig{Endpoint: "https://example.test", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-token"}},
		Capabilities: &config.AppCapabilities{Models: []string{"safe-model"}, WireEndpoints: []string{"chat_completions", "messages_compat", "responses_compat"}},
	}))
	s.model.Cache.Invalidate()
	_, err := s.LookupModel(t.Context(), variantChat)
	require.NoError(t, err)
	addConflictingWeights(t, s, variantChat)
	require.NoError(t, s.model.Groups.Set("mixed", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{
		{Name: "native", App: "llamacpp", Model: variantChat},
		{Name: "translated", App: "safe", Model: "safe-model"},
	}}))
	s.model.Resolver = resolver.NewGroup(s.model.Groups, s.model.Resolver)
	for _, endpoint := range []string{"messages", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/"+endpoint, nil)
			resolved, ok := s.resolveTarget(c, "mixed", "")
			require.True(t, ok, w.Body.String())
			mode, ok := s.narrowToWireEndpoint(c, "mixed", resolved, endpoint, endpoint+"_compat")
			require.True(t, ok, w.Body.String())
			assert.Equal(t, wireModeCompat, mode)
			require.Len(t, resolved.Candidates, 1)
			assert.Equal(t, "translated", resolved.Candidates[0].Name)
		})
	}
	require.NoError(t, s.model.Groups.Set("conflicts", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{{Name: "native", App: "llamacpp", Model: variantChat}}}))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	httperr.AttachResponder(s.responders.openai)(c)
	_, ok = s.resolveTarget(c, "conflicts", "")
	require.False(t, ok)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
}

func TestVariants_ConflictFilteringPrecedesImagePreference(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/api/chat"} {
		t.Run(path, func(t *testing.T) {
			backends := map[string]*serviceBackend{
				"ollama": {wire: []string{"chat_completions"}, status: 200, reply: `{"model":"safe-model","choices":[],"done":true}`},
			}
			s := newServiceGroupNode(t, []string{"ollama"}, backends)
			prov_apps.WithModelValidator(func(_ context.Context, model string) error {
				if model == "fast" {
					return &config.ModelNameConflictError{Model: "fast"}
				}
				return nil
			})(s.providers.appMgr)
			require.NoError(t, s.model.Groups.Set("mixed", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{
				{Name: "conflicted", App: "ollama", Model: "fast"},
				{Name: "safe", App: "ollama", Model: "safe-model"},
			}}))
			items := []map[string]any{
				{"name": "fast", "node": s.node.Nodename(), "assigned_app": "ollama", "details": map[string]any{"features": map[string]string{"vision": "present"}}},
				{"name": "safe-model", "node": s.node.Nodename(), "assigned_app": "ollama", "details": map[string]any{"features": map[string]string{"vision": "unknown"}}},
			}
			raw, err := json.Marshal(map[string]any{"models": items})
			require.NoError(t, err)
			peer := &imageAdmissionCatalogPeer{body: raw}
			s.model.Cache = cache.New(cache.Config{Node: s.node, NodeConfig: &config.NodeConfig{}, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: func() *config.AppsConfig { return nil }, ClusterClient: func() mesh.ClusterClient { return peer }, ResolveEndpointToNodename: func(node string) string { return node }})
			require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
			body := `{"model":"mixed","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}],"stream":false}`
			if path == "/api/chat" {
				body = `{"model":"mixed","messages":[{"role":"user","content":"describe","images":["AAA"]}],"stream":false}`
			}
			w := postAsAdmin(t, s, path, body)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			hits := backends["ollama"].requests()
			require.Len(t, hits, 1)
			assert.Contains(t, string(hits[0].Body), `"model":"safe-model"`)
		})
	}
}

func TestVariants_ResponsesAffinityCannotBypassConflict(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	calls := &atomic.Int32{}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_next","object":"response"}`)
	}))
	t.Cleanup(engine.Close)
	// Affinity resolves an endpoint directly, rather than the on-demand run.
	endpoints := &config.AppsConfig{}
	require.NoError(t, endpoints.AddApp("llamacpp", config.ServiceConfig{Enabled: new(true), Mode: "service", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: engine.URL}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"responses"}}}))
	s.backend = backend.NewResolver(func() *config.AppsConfig { return endpoints })
	_, resolvable := s.backend.Resolve("llamacpp")
	require.True(t, resolvable)
	require.NoError(t, s.appsConfig.UpdateApp("llamacpp", func(sc *config.ServiceConfig) error {
		sc.Capabilities.WireEndpoints = []string{"responses"}
		return nil
	}))
	s.model.Cache.Invalidate()
	require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
	require.Equal(t, wireModeNative, s.targetWireMode(t.Context(), variantChat, "llamacpp", "", "responses", "responses_compat"))
	addConflictingWeights(t, s, variantChat)
	s.inference.affinity.Record("resp_pinned", "llamacpp")
	require.NoError(t, s.model.Groups.Set("pinned-group", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{{Name: "variant", App: "llamacpp", Model: variantChat}}}))
	for _, model := range []string{variantChat, "pinned-group"} {
		w := postAsAdmin(t, s, "/v1/responses", `{"model":"`+model+`","previous_response_id":"resp_pinned","input":"hi"}`)
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	}
	require.Zero(t, calls.Load())
}

func TestVariants_WorkerRescanReleasesRemovedWeightsConflict(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	addConflictingWeights(t, s, variantChat)
	require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
	require.ErrorIs(t, s.model.Cache.CheckVariantConflict(t.Context(), variantChat), config.ErrModelNameConflict)
	root, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	for _, dir := range []string{root, filepath.Join(root, "owner", "weights")} {
		require.NoError(t, os.Remove(filepath.Join(dir, strings.ToUpper(variantChat)+".gguf")))
	}
	// The old snapshot must still contain physical weights before the rescan.
	require.ErrorIs(t, s.model.Cache.CheckVariantConflict(t.Context(), variantChat), config.ErrModelNameConflict)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/zzrouter/v1/internal/models/rescan", nil)
	s.newInternalExecutor().HandleInternalRescanModels(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, s.model.Cache.CheckVariantConflict(t.Context(), variantChat))
	require.NoError(t, s.providers.appMgr.ValidateModel(t.Context(), variantChat))
	models, err := s.model.Cache.ListModels(t.Context(), "", "", "", variantChat)
	require.NoError(t, err)
	require.NotEmpty(t, models)
	for _, m := range models {
		assert.Equal(t, qwenModel, m.VariantOf)
	}
}

func TestVariants_WorkerPartialDeletionInvalidatesConflict(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	addConflictingWeights(t, s, variantChat)
	require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
	require.ErrorIs(t, s.model.Cache.CheckVariantConflict(t.Context(), variantChat), config.ErrModelNameConflict)
	root, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	paths := []string{filepath.Join(root, strings.ToUpper(variantChat)+".gguf"), filepath.Join(root, "owner", "weights", strings.ToUpper(variantChat)+".gguf"), filepath.Join(root, "missing.gguf")}
	body, err := json.Marshal(map[string]any{"paths": paths})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("DELETE", "/zzrouter/v1/internal/models", strings.NewReader(string(body)))
	s.newInternalExecutor().HandleInternalDeleteModels(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var result DeleteModelsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	assert.Equal(t, 2, result.Deleted)
	require.Len(t, result.Errors, 1)
	assert.Equal(t, false, s.model.Cache.GetCacheStats()["valid"])
	require.NoError(t, s.model.Cache.CheckVariantConflict(t.Context(), variantChat))
}

func TestRemoteRescanInvalidatesCoordinatorEvidenceOnSuccess(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s := variantNode(t)
			require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
			require.Equal(t, true, s.model.Cache.GetCacheStats()["valid"])
			router := &fakeRouter{perPath: map[string]struct {
				status int
				body   []byte
			}{"/zzrouter/v1/internal/models/rescan": {status: status, body: []byte(`{}`)}}}
			service := &ModelService{cache: s, router: router}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/zzrouter/v1/models/scan?node=remote", nil)
			service.handleRescanModels(c)
			require.Equal(t, status, w.Code, w.Body.String())
			assert.Equal(t, status >= http.StatusBadRequest, s.model.Cache.GetCacheStats()["valid"])
		})
	}
}

func TestVariants_UnpublishedColdLoadAndForcePreserveAddressedName(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(strconv.FormatBool(force), func(t *testing.T) {
			s := variantNode(t)
			defineVariants(t, s)
			_, err := s.LookupModel(t.Context(), variantChat)
			require.NoError(t, err)
			root, err := modelregistry.GetModelsRootDir()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, variantChat+".gguf"), []byte("GGUF"), 0600))
			if force {
				conflictRun(t, s, qwenModel)
			}
			_, err = s.newLoadExecutor().LoadLocalModel(t.Context(), &LoadModelRequest{Provider: "llamacpp", ModelName: variantChat, Force: force})
			require.ErrorIs(t, err, config.ErrModelNameConflict)
			if force {
				require.Len(t, s.providers.appMgr.Instances().List(), 1)
				require.Equal(t, instance.StatusRunning, s.providers.appMgr.Instances().List()[0].GetStatus())
			} else {
				require.Empty(t, s.providers.appMgr.Instances().List())
			}
		})
	}
}

func TestVariants_ColdAndForcedSourceAliasUseCanonicalMetadata(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(strconv.FormatBool(force), func(t *testing.T) {
			s := variantNode(t)
			root, err := modelregistry.GetModelsRootDir()
			require.NoError(t, err)
			dir := filepath.Join(root, "owner", "source")
			require.NoError(t, os.MkdirAll(dir, 0750))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "alias-Q4_K_M.gguf"), []byte("GGUF"), 0600))
			s.model.Cache.Invalidate()
			model, err := s.LookupModel(t.Context(), "owner/source")
			require.NoError(t, err)
			if force {
				conflictRun(t, s, model.Name)
			}
			result, err := s.newLoadExecutor().LoadLocalModel(t.Context(), &LoadModelRequest{Provider: "llamacpp", ModelName: "owner/source", Force: force})
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Zero(t, result.StatusCode)
			assert.Equal(t, model.Name, result.Model)
		})
	}
}

func TestVariants_CatalogFailureKeepsManagementStatuses(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	manager, err := prov_apps.NewProviderAppManager(s.appsConfig, prov_apps.WithModelValidator(func(context.Context, string) error { return errors.New("catalog unavailable") }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.Stop(t.Context()) })
	old := s.providers.appMgr
	s.providers.appMgr = manager
	t.Cleanup(func() { s.providers.appMgr = old })
	require.NoError(t, manager.ValidateModel(t.Context(), "cloud-model"))
	service := NewRunsService(nil).WithModelValidator(func(context.Context, string) error { return errors.New("catalog unavailable") })
	require.NoError(t, service.admitModel(t.Context(), "unrelated"))
	require.Equal(t, http.StatusInternalServerError, extractStatusCode(errors.New("plain error")))
	for _, tc := range []struct {
		model, provider string
		status          int
	}{
		{qwenModel, "missing-provider", http.StatusBadRequest},
		{"missing-model", "llamacpp", http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		raw, _ := json.Marshal(map[string]string{"model_name": tc.model, "provider": tc.provider})
		c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/runs/load", strings.NewReader(string(raw)))
		s.newLoadExecutor().HandleInternalLoadModel(c)
		require.Equal(t, tc.status, w.Code, w.Body.String())
	}
	_, err = s.newLoadExecutor().PreviewLocalRun(t.Context(), &PreviewRunRequest{Provider: "missing-provider", ModelName: qwenModel})
	require.Equal(t, http.StatusBadRequest, extractStatusCode(err))
	service.WithModelValidator(func(context.Context, string) error { return &config.ModelNameConflictError{Model: "reserved"} })
	require.Equal(t, http.StatusConflict, extractStatusCode(service.admitModel(t.Context(), "reserved")))
}

func TestVariants_InternalLoadNonConflictErrorsKeepHistorical500(t *testing.T) {
	s := variantNode(t)
	executor := s.newLoadExecutor()
	executor.launchOnDemand = func(context.Context, string, string, string, map[string]string, map[string]string) (*instance.Instance, error) {
		return nil, prov_apps.ErrProviderNotFound
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/runs/load", strings.NewReader(`{"model_name":"`+qwenModel+`","provider":"llamacpp"}`))
	executor.HandleInternalLoadModel(c)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
}

func TestVariants_ForcedLoadCarriesLocalAdmission(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	_, err := s.LookupModel(t.Context(), variantAgent)
	require.NoError(t, err)
	conflictRun(t, s, variantAgent)
	root, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	executor := s.newLoadExecutor()
	launch := executor.launchOnDemand
	executor.launchOnDemand = func(ctx context.Context, model, provider, endpoint string, params, env map[string]string) (*instance.Instance, error) {
		// The forced load already admitted this name before stopping the run.
		require.NoError(t, os.WriteFile(filepath.Join(root, variantAgent+".gguf"), []byte("GGUF"), 0o600))
		return launch(ctx, model, provider, endpoint, params, env)
	}
	result, err := executor.LoadLocalModel(cache.WithVariantAdmission(t.Context()), &LoadModelRequest{Provider: "llamacpp", ModelName: variantAgent, Force: true})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Zero(t, result.StatusCode)
	_, err = s.providers.appMgr.AdmitLocalModel(t.Context(), variantAgent)
	require.ErrorIs(t, err, config.ErrModelNameConflict, "the next operation must see the new conflict")
}

func TestVariants_PreviewGuardsUnpublishedLocalConflict(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	_, err := s.LookupModel(t.Context(), variantAgent)
	require.NoError(t, err)
	root, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, variantAgent+".gguf"), []byte("GGUF"), 0o600))
	_, err = s.newLoadExecutor().PreviewLocalRun(t.Context(), &PreviewRunRequest{Provider: "llamacpp", ModelName: variantAgent})
	require.Error(t, err)
	assert.Equal(t, http.StatusConflict, extractStatusCode(err))
}
