package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stretchr/testify/require"
)

func TestIntegrationBrowserManagementStreamsAndMergePatch(t *testing.T) {
	endpoint := httptest.NewUnstartedServer(nil)
	t.Cleanup(endpoint.Close)
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, OAuthIssuer: "http://" + endpoint.Listener.Addr().String(), SeedProvidersDir: true})
	endpoint.Config.Handler = s.engine
	endpoint.Start()
	admin := newSessionBrowser(t, endpoint, TestAdminKey)
	user := newSessionBrowser(t, endpoint, TestUserKey)
	handle, err := s.jobs.Start(t.Context(), jobs.KindInstall, "browser-fixture", nil)
	require.NoError(t, err)
	handle.Progress(50, "Observed progress", jobs.Bytes{})
	handle.Done()
	streamPath := "/zzrouter/v1/jobs/" + handle.ID() + "/stream?from=0"
	status, body := admin.call(t, http.MethodGet, streamPath, nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), "event:progress\n")
	require.Contains(t, string(body), "event:done\n")
	status, _ = user.call(t, http.MethodGet, streamPath, nil, nil)
	require.Equal(t, http.StatusForbidden, status)
	status, _ = admin.call(t, http.MethodGet, streamPath, nil, map[string]string{"Origin": "https://foreign.example"})
	require.Equal(t, http.StatusForbidden, status)

	path := "/zzrouter/v1/providers/vllm/parameters"
	patch := map[string]any{"defaults": map[string]any{"parameters": map[string]any{"max-model-len": 4096}}}
	headers := map[string]string{"Content-Type": "application/merge-patch+json"}
	status, body = admin.call(t, http.MethodPatch, path, patch, headers)
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = admin.call(t, http.MethodGet, path+"?scope=defaults", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var configured struct {
		Parameters map[string]string `json:"parameters"`
	}
	require.NoError(t, json.Unmarshal(body, &configured))
	require.Equal(t, "4096", configured.Parameters["max-model-len"])
	for _, proof := range []map[string]string{
		{"Content-Type": "application/merge-patch+json", "X-CSRF-Token": ""},
		{"Content-Type": "application/merge-patch+json", "Origin": "https://foreign.example"},
	} {
		status, _ = admin.call(t, http.MethodPatch, path, patch, proof)
		require.Equal(t, http.StatusForbidden, status)
	}
	status, _ = user.call(t, http.MethodPatch, path, patch, headers)
	require.Equal(t, http.StatusForbidden, status)
	status, _ = user.call(t, http.MethodGet, path+"?scope=defaults", nil, nil)
	require.Equal(t, http.StatusForbidden, status)
}

func TestIntegrationBrowserCompleteConsoleAssets(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeDisabled)
	for _, asset := range []string{"models", "model-data", "model-discovery", "deployments", "provider-lifecycle", "provider-install", "parameters", "pricing", "event-stream", "run-logs", "logs", "system", "updates", "vendor/events"} {
		recorder := httptest.NewRecorder()
		s.engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/console/"+asset+".mjs", nil))
		require.Equal(t, http.StatusOK, recorder.Code, asset)
		require.Equal(t, "text/javascript; charset=utf-8", recorder.Header().Get("Content-Type"), asset)
		require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"), asset)
	}
}
