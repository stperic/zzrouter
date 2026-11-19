package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stretchr/testify/require"
)

func TestDownloadIdentifierSelection(t *testing.T) {
	cfg := featureTestConfig(t)
	for _, tc := range []struct{ model, registry, file, want, wantFile string }{
		{"qwen2.5", "ollama", "0.5b", "qwen2.5:0.5b", "0.5b"},
		{"qwen2.5:latest", "", "0.5b", "qwen2.5:0.5b", "0.5b"},
		{"acme/qwen:latest", "ollama", "0.5b", "acme/qwen:0.5b", "0.5b"},
		{"openai/gpt-oss:latest", "ollama", "20b", "openai/gpt-oss:20b", "20b"},
		{"openai/gpt-oss#old.gguf", "huggingface", "new.gguf", "openai/gpt-oss", "new.gguf"},
		{"registry.example:5000/acme/qwen:latest", "ollama", "0.5b", "registry.example:5000/acme/qwen:0.5b", "0.5b"},
		{"qwen2.5:latest#1.5b@worker", "ollama", "0.5b", "qwen2.5:0.5b", "0.5b"},
		{"qwen2.5#0.5b", "ollama", "", "qwen2.5:0.5b", "0.5b"},
		{"qwen2.5:cloud", "ollama", "0.5b", "qwen2.5:0.5b", "0.5b"},
		{"qwen2.5:0.5b", "ollama", "cloud", "qwen2.5:cloud", "cloud"},
		{"org/model#old.gguf", "hf", "new.gguf", "org/model", "new.gguf"},
		{"org/model:date#version@team", "openai", "", "org/model:date#version@team", ""},
	} {
		t.Run(tc.model+"/"+tc.file, func(t *testing.T) {
			id, err := parseDownloadIdentifier(tc.model, tc.registry, tc.file, cfg)
			require.NoError(t, err)
			require.Equal(t, tc.want, id.GetFullModelName())
			require.Equal(t, tc.wantFile, id.File)
			again, err := parseDownloadIdentifier(id.GetFullModelName(), id.Repository, id.File, cfg)
			require.NoError(t, err)
			require.Equal(t, tc.want, again.GetFullModelName())
		})
	}
}

func TestDeploymentSelectionPrecedesPresenceAndDispatch(t *testing.T) {
	s := deployServiceWithResult(t, mustJSON(t, internalDeploymentResult{Key: "selected", Model: "qwen2.5:0.5b", Status: "completed"}))
	d, err := s.Deploy(t.Context(), &DeployRequest{Model: "qwen2.5:latest", Registry: "ollama", File: "0.5b", Nodes: []string{"worker"}})
	require.NoError(t, err)
	require.Equal(t, "qwen2.5:0.5b", d.Model)
	require.Equal(t, "0.5b", d.File)
	calls := s.router.(*fakeRouter).calls
	require.Len(t, calls, 2)
	presence, err := url.Parse(calls[0].Path)
	require.NoError(t, err)
	require.Equal(t, "qwen2.5:0.5b", presence.Query().Get("model"))
	var sent internalDownloadRequest
	require.NoError(t, json.Unmarshal(calls[1].Body, &sent))
	require.Equal(t, "qwen2.5:0.5b", sent.Model)
	require.Equal(t, "worker", sent.Node)
}

func TestInternalDeploymentPullsSelectedOllamaTag(t *testing.T) {
	requested := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requested <- body.Name
		_, _ = w.Write([]byte("{\"status\":\"pulling\",\"digest\":\"sha256:fixture\",\"total\":1,\"completed\":1}\n{\"status\":\"success\"}\n"))
	}))
	t.Cleanup(upstream.Close)
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("ollama", config.ServiceConfig{Mode: "external", Protocol: config.ProtocolOllama, Enabled: new(true),
		Runtime:      &config.AppRuntimeConfig{Endpoint: upstream.URL, API: &config.APIConfig{AuthType: config.AuthTypeNone}},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	getter := func() *config.AppsConfig { return cfg }
	modelregistry.SetModelsRootDirOverride(t.TempDir())
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	registry, err := modelregistry.NewRegistry(getter)
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	downloads := modelregistry.NewDownloadTrackerWithPersistence("")
	t.Cleanup(downloads.Stop)
	reg := newTestJobsRegistry(t)
	target, ok := backend.NewResolver(getter).Resolve("ollama")
	require.True(t, ok)
	e := NewDeploymentsExecutor(func() string { return "worker" }, func(string) bool { return false }, func(string, string, string) bool { return true },
		func(string, string, string) string { return "incompatible" }, downloads, registry, func() (*backend.Resolved, error) { return target, nil }, getter, nil, func() error { return nil }, reg)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/deployments", bytes.NewBufferString(`{"model":"qwen2.5:cloud","registry":"ollama","file":"0.5b"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	e.HandleInternalDeploy(c)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var accepted internalDeploymentResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &accepted))
	require.Equal(t, "qwen2.5:0.5b", accepted.Model)
	require.Contains(t, accepted.Key, "qwen2.5:0.5b")
	require.Equal(t, jobs.PhaseDone, waitJob(t, reg, accepted.JobID).Phase)
	select {
	case model := <-requested:
		require.Equal(t, "qwen2.5:0.5b", model)
	default:
		t.Fatal("completed download did not reach Ollama")
	}
}

func TestDeploymentRejectsInvalidIdentifierBeforeDispatch(t *testing.T) {
	s := NewDeploymentsService(&fakeRouter{}, nil, nilAppsConfig)
	t.Cleanup(func() { s.Stop(context.Background()) })
	_, err := s.Deploy(t.Context(), &DeployRequest{Model: "worker::qwen@other", Nodes: []string{"worker"}})
	require.Error(t, err)
	require.Empty(t, s.router.(*fakeRouter).calls)
}
