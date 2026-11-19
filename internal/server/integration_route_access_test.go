package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func TestIntegrationRouteAccessWireDeniesDirectModels(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord"}})
	_, enableErr := s.configStore.SetProviderEnabled("ollama", "", true)
	require.NoError(t, enableErr)
	require.NoError(t, s.model.Groups.Set("coding", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "local", App: "ollama", Model: "local"}}}))
	_, err := s.services.Teams.CreateTeam(t.Context(), &CreateTeamRequest{ID: "routes", Name: "Routes", RouteAccess: teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}}}, "admin")
	require.NoError(t, err)
	raw := loadTestKey(t, s, "route-client", keys.VirtualKey{Name: "Client", Role: "user", TeamID: "routes"})
	for _, tc := range []struct{ method, path, model string }{
		{"POST", "/v1/chat/completions", "local"}, {"POST", "/v1/chat/completions", "coding@worker"},
		{"POST", "/v1/messages/count_tokens", "local"}, {"POST", "/v1/completions", "local"}, {"POST", "/v1/embeddings", "local"},
		{"POST", "/api/chat", "local"}, {"POST", "/api/generate", "local"}, {"POST", "/api/embed", "local"},
	} {
		t.Run(tc.path+tc.model, func(t *testing.T) {
			response := makeAuthRequest(t, s, tc.method, tc.path, raw, map[string]any{"model": tc.model, "messages": []map[string]string{{"role": "user", "content": "hello"}}, "input": "hello", "prompt": "hello"})
			require.Equal(t, 403, response.Code, string(response.Body))
		})
	}
}

// Exercise the admitted route through the actual worker compatibility engine.
// The worker must retain the provider binding admitted for the virtual team.
func TestIntegrationRouteProviderBindingAcrossWorkerHop(t *testing.T) {
	coordinator := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord"}})
	require.NoError(t, coordinator.model.Groups.Set("coding", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "initial", App: "vllm", Model: "allowed", Node: "worker"}}}))
	_, err := coordinator.services.Teams.CreateTeam(t.Context(), &CreateTeamRequest{ID: "bound", Name: "Bound", RouteAccess: teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}}, ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"vllm"}}}, "admin")
	require.NoError(t, err)
	key := loadTestKey(t, coordinator, "bound-client", keys.VirtualKey{Name: "Bound client", Role: "user", TeamID: "bound"})
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "qwen.gguf"), []byte("GGUF"), 0o600))
	worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker, NodeName: "worker"}, modelsDir: root})
	for _, provider := range []string{"vllm", "llamacpp"} {
		_, err := worker.configStore.SetProviderEnabled(provider, "", true)
		require.NoError(t, err)
		_, err = coordinator.configStore.SetProviderEnabled(provider, "", true)
		require.NoError(t, err)
	}
	models := []map[string]any{{"name": "allowed", "node": "worker", "assigned_app": "vllm"}, replicaItem("worker", "llamacpp")}
	routingCatalog(t, coordinator, models)
	routingCatalog(t, worker, models)
	for index, provider := range []string{"vllm", "llamacpp"} {
		model := "allowed"
		if provider == "llamacpp" {
			model = "qwen"
		}
		run := instance.NewInstance("worker-"+provider, provider, model, 8100+index, 0, 0)
		run.WireModel = model
		run.MarkRunning(false, nil)
		require.NoError(t, worker.providers.appMgr.Instances().Register(run))
		t.Cleanup(func() { _ = worker.providers.appMgr.Instances().Remove(run.ID) })
	}
	engineCalls := map[string]int{}
	worker.httpStreamingClient = &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		engineCalls[r.URL.Port()]++
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Contains(t, []string{"allowed", "qwen"}, body["model"])
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"qwen","choices":[{"message":{"role":"assistant","content":"bound provider reply"}}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`))}, nil
	})}
	compat := buildWorkerCompatEngine(worker)
	require.NoError(t, coordinator.cluster.coordinator.RegisterEndpoint(&mesh.Endpoint{Name: "worker", URL: "http://worker.test:9090", ClusterURL: "https://worker.test:9091"}))
	coordinator.clusterMTLSClient = &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "vllm", r.Header.Get(constants.HeaderServingProvider))
		w := httptest.NewRecorder()
		compat.ServeHTTP(w, r)
		return w.Result(), nil
	})}
	for _, tc := range []struct {
		name, model string
		allowed     bool
	}{
		{name: "conflicting", model: "llamacpp/qwen"},
		{name: "bare", model: "allowed", allowed: true},
		{name: "matching", model: "vllm/allowed", allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, coordinator.model.Groups.Set("coding", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: tc.name, App: "vllm", Model: tc.model, Node: "worker"}}}))
			before := engineCalls["8100"]
			response := makeAuthRequest(t, coordinator, http.MethodPost, "/v1/chat/completions", key, map[string]any{"model": "coding", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
			if tc.allowed {
				require.Equal(t, http.StatusOK, response.Code, string(response.Body))
				require.Equal(t, before+1, engineCalls["8100"])
			} else {
				require.NotEqual(t, http.StatusOK, response.Code, string(response.Body))
				require.Equal(t, before, engineCalls["8100"])
			}
			require.Zero(t, engineCalls["8101"], "the team never permits the llama.cpp engine")
		})
	}
}
