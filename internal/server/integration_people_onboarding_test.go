package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func TestIntegrationPeopleOnboardingKeepsAvailabilitySeparateFromInference(t *testing.T) {
	endpoint := httptest.NewUnstartedServer(nil)
	t.Cleanup(endpoint.Close)
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord",
		OAuthIssuer: "http://" + endpoint.Listener.Addr().String(), SeedProvidersDir: true})
	_, err := s.configStore.SetProviderEnabled("vllm", "", true)
	require.NoError(t, err)
	routingCatalog(t, s, []map[string]any{replicaItem("coord", "vllm")})
	run := instance.NewInstance("onboarding-fixture", "vllm", "qwen", 8100, 0, 0)
	run.WireModel = "qwen"
	run.MarkRunning(false, nil)
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	t.Cleanup(func() { _ = s.providers.appMgr.Instances().Remove(run.ID) })
	s.httpStreamingClient = &http.Client{Transport: mcpRoundTripper(func(_ *http.Request) (*http.Response, error) {
		stream := "data: {\"choices\":[{\"delta\":{\"content\":\"Ready\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})}
	endpoint.Config.Handler = s.engine
	endpoint.Start()
	owner, _ := createBrowserOwner(t, s, endpoint)
	status, body := owner.call(t, "GET", "/zzrouter/v1/access/onboarding", nil, nil)
	require.Equal(t, http.StatusOK, status)
	for _, field := range []string{"provider_ready", "model_ready"} {
		require.Contains(t, peopleResponse[map[string]any](t, body), field)
	}
	progress := peopleResponse[onboardingProgress](t, body)
	require.False(t, progress.ResponseVerified)
	for _, field := range []string{"provider_ready", "model_ready", "response_verified", "app_connected"} {
		status, _ := owner.call(t, "PATCH", "/zzrouter/v1/access/onboarding", map[string]bool{field: true}, nil)
		require.Equal(t, http.StatusBadRequest, status, "browser cannot fabricate %s", field)
	}
	updated := peopleCall[map[string]any](t, owner, "PATCH", "/access/onboarding", map[string]string{"step": "test"})
	require.Contains(t, updated, "provider_ready")
	require.Contains(t, updated, "model_ready")
	require.Equal(t, "test", updated["step"])
	require.Equal(t, false, updated["response_verified"], "navigating to Chat is not successful inference")
	status, _ = owner.call(t, "POST", "/v1/chat/completions", map[string]any{}, nil)
	require.Equal(t, http.StatusBadRequest, status)
	require.False(t, peopleCall[onboardingProgress](t, owner, "GET", "/access/onboarding", nil).ResponseVerified)
	status, body = owner.call(t, "POST", "/v1/chat/completions", map[string]any{"model": "qwen", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), "Ready")
	completed := peopleCall[onboardingProgress](t, owner, "GET", "/access/onboarding", nil)
	require.True(t, completed.ResponseVerified, "only the completed owner inference records success")
	require.False(t, completed.AppConnected, "console inference is not an app-key request")
	require.Equal(t, progress.ProviderReady, completed.ProviderReady)
	require.Equal(t, progress.ModelReady, completed.ModelReady, "inference evidence does not rewrite inventory readiness")
}
