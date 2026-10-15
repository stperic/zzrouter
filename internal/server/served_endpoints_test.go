package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/dispatch/chain"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func endpointRequest(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", TestAdminKey)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	return w
}

func TestRunEndpointRefusalHTTP(t *testing.T) {
	s := variantNode(t)
	cfg := s.configStore.Config()
	require.NoError(t, cfg.UpdateApp("llamacpp", func(svc *config.ServiceConfig) error {
		svc.Capabilities.WireEndpoints = []string{"chat_completions"}
		svc.Defaults.Endpoints, svc.Models, svc.Nodes = nil, nil, nil
		require.False(t, svc.IsEndpointAware())
		return nil
	}))
	s.providers.appMgr.ReloadConfig(cfg)
	for _, path := range []string{"/zzrouter/v1/runs", "/zzrouter/v1/runs/preview"} {
		w := endpointRequest(t, s, path, `{"provider":"llamacpp","model_name":"`+qwenModel+`","endpoint":"embeddings"}`)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Contains(t, w.Header().Get("Content-Type"), "application/problem+json")
		var problem struct {
			Status int
			Detail string
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem))
		require.Equal(t, http.StatusBadRequest, problem.Status)
		require.Contains(t, problem.Detail, "llamacpp")
		require.Contains(t, problem.Detail, "embeddings")
	}
	require.Empty(t, s.providers.appMgr.Instances().List())

	run := instance.NewInstance("warm-chat", "llamacpp", qwenModel, 8300, 0, 0)
	run.Endpoint = "chat"
	run.MarkRunning(false, nil)
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	defer func() { require.NoError(t, s.providers.appMgr.Instances().Remove(run.ID)) }()
	calls := 0
	s.proxy = NewProxyClient(&http.Client{Transport: mcpRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}, nil, s.inference.normalizers, nil, nil, nil, nil)
	w := endpointRequest(t, s, "/v1/embeddings", `{"model":"`+qwenModel+`","input":"ping"}`)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var out struct {
		Error struct{ Type, Message string }
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Equal(t, "invalid_request_error", out.Error.Type)
	require.Contains(t, out.Error.Message, "llamacpp")
	require.Contains(t, out.Error.Message, "embeddings")
	require.Zero(t, calls)
	require.Equal(t, instance.StatusRunning, run.GetStatus())
	require.Equal(t, []*instance.Instance{run}, s.providers.appMgr.Instances().List())
}

func TestOllamaRerankRefusalHTTP(t *testing.T) {
	r := newServedServerRig(t)
	run := r.served(t)
	w := httptest.NewRecorder()
	err := r.s.HandleLocalModel(w, inference("/v1/rerank", []byte(`{"model":"m","query":"ping","documents":["ping"]}`)), &resolver.Resolved{Provider: "ollama", ModelName: servedClientModel}, "reranking")
	require.ErrorIs(t, err, prov_apps.ErrParameterValidation)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "invalid_request_error")
	require.Contains(t, w.Body.String(), "reranking")
	require.Equal(t, instance.StatusRunning, run.GetStatus())
}

func TestChainHonorsEndpointHTTP(t *testing.T) {
	for _, onDemand := range []bool{true, false} {
		t.Run(map[bool]string{true: "on demand", false: "local run"}[onDemand], func(t *testing.T) {
			s := variantNode(t)
			if !onDemand {
				endpoints := &config.AppsConfig{}
				require.NoError(t, endpoints.AddApp("llamacpp", config.ServiceConfig{Enabled: new(true), Mode: "external", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: "http://engine:8301"}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions", "embeddings"}}}))
				s.backend = backend.NewResolver(func() *config.AppsConfig { return endpoints })
			}
			chat := instance.NewInstance("chat", "llamacpp", qwenModel, 8300, 0, 0)
			chat.Endpoint = "chat"
			chat.MarkRunning(false, nil)
			embeddings := instance.NewInstance("embeddings", "llamacpp", qwenModel, 8301, 0, 0)
			embeddings.Endpoint = "embeddings"
			embeddings.MarkRunning(false, nil)
			for _, run := range []*instance.Instance{chat, embeddings} {
				require.NoError(t, s.providers.appMgr.Instances().Register(run))
				defer func() { require.NoError(t, s.providers.appMgr.Instances().Remove(run.ID)) }()
			}
			calls := 0
			s.proxy = NewProxyClient(&http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, "8301", r.URL.Port())
				require.Equal(t, "/v1/embeddings", r.URL.Path)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[{"object":"embedding","embedding":[1],"index":0}]}`))}, nil
			})}, nil, s.inference.normalizers, nil, nil, nil, nil)
			resolved := &resolver.Resolved{OriginalName: "group", GroupName: "group", Strategy: "priority", Candidates: []fallback.Candidate{{Name: "embed", App: "llamacpp", Model: qwenModel, OnDemand: onDemand}}}
			engine := gin.New()
			engine.Use(httperr.AttachResponder(openaiproto.New()))
			engine.POST("/v1/embeddings", func(c *gin.Context) {
				body, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				s.dispatchTo(c, resolved, "group", body, "embeddings")
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewBufferString(`{"model":"group","input":"ping"}`))
			req.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, 1, calls)
			if onDemand {
				require.NoError(t, s.model.Groups.Set("embedding-group", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{{Name: "embed", App: "llamacpp", Model: qwenModel, OnDemand: true}}}))
				actual := endpointRequest(t, s, "/v1/embeddings", `{"model":"embedding-group","input":"ping"}`)
				require.Equal(t, http.StatusOK, actual.Code, actual.Body.String())
				require.Equal(t, 2, calls)
			}
			require.Zero(t, chat.GetActiveRequests())
		})
	}
}

func TestChainColdEndpointRefusalHTTP(t *testing.T) {
	s := variantNode(t)
	cfg := s.configStore.Config()
	require.NoError(t, cfg.UpdateApp("llamacpp", func(svc *config.ServiceConfig) error {
		svc.Capabilities.WireEndpoints = []string{"chat_completions"}
		return nil
	}))
	s.providers.appMgr.ReloadConfig(cfg)
	chat := instance.NewInstance("chat", "llamacpp", qwenModel, 8300, 0, 0)
	chat.Endpoint = "chat"
	chat.MarkRunning(false, nil)
	require.NoError(t, s.providers.appMgr.Instances().Register(chat))
	defer func() { require.NoError(t, s.providers.appMgr.Instances().Remove(chat.ID)) }()
	calls := 0
	s.proxy = NewProxyClient(&http.Client{Transport: mcpRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}, nil, s.inference.normalizers, nil, nil, nil, nil)
	resolved := &resolver.Resolved{OriginalName: "group", GroupName: "group", Strategy: "priority", Candidates: []fallback.Candidate{{Name: "embed", App: "llamacpp", Model: qwenModel, OnDemand: true}}}
	engine := gin.New()
	engine.Use(httperr.AttachResponder(openaiproto.New()))
	engine.POST("/v1/embeddings", func(c *gin.Context) {
		s.dispatchTo(c, resolved, "group", []byte(`{"model":"group","input":"ping"}`), "embeddings")
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "invalid_request_error")
	require.Contains(t, w.Body.String(), "embeddings")
	require.Zero(t, calls)
	require.Equal(t, []*instance.Instance{chat}, s.providers.appMgr.Instances().List())
}

type pendingEndpointDeps struct {
	*chainServerDeps
	endpoint string
}

func (d *pendingEndpointDeps) StartOnDemand(_, _, endpoint string) error {
	d.endpoint = endpoint
	return nil
}

func TestChainColdEndpointPendingHTTP(t *testing.T) {
	s := variantNode(t)
	deps := &pendingEndpointDeps{chainServerDeps: newChainServerDeps(s)}
	strategies := fallback.NewStrategyRegistry()
	strategies.Register(&fallback.PriorityStrategy{})
	proxy := chain.New(deps, fallback.NewCooldownManager(), fallback.NewCooldownManager(), strategies, nil, nil, nil)
	resolved := &resolver.Resolved{OriginalName: "group", GroupName: "group", Strategy: "priority", Candidates: []fallback.Candidate{{Name: "embed", App: "llamacpp", Model: qwenModel, OnDemand: true}}}
	engine := gin.New()
	engine.Use(httperr.AttachResponder(openaiproto.New()))
	engine.POST("/v1/embeddings", func(c *gin.Context) {
		proxy.ProxyWithFallback(c, resolved, []byte(`{"model":"group","input":"ping"}`), nil, "group", "embeddings", false)
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "model_loading")
	require.Equal(t, "60", w.Header().Get("Retry-After"))
	require.Equal(t, "embeddings", deps.endpoint)
}
