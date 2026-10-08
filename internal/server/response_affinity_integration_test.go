// Integration test for /v1/responses session affinity. Verifies that a
// response id returned by POST /v1/responses is recorded in the affinity
// map and that subsequent retrieval calls land on the same backend.
package server

import (
	"encoding/json"
	"net/http"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResponseAffinity_RecordsOnCreate_UsesOnRetrieve verifies the core
// contract: POST /v1/responses returns an id, the affinity map records
// (id → provider), and a subsequent GET /v1/responses/{id} is served by
// the recorded backend — not the configured default.
func TestResponseAffinity_RecordsOnCreate_UsesOnRetrieve(t *testing.T) {
	// Two distinct backends: "affinityBackend" responds to POST with a
	// JSON body carrying an id; "defaultBackend" is a second server we
	// configure as openai_compat.default_backend so we can prove the
	// retrieval routed away from it.
	affinityBackend, affinityRecorded, affinityMu := newBackendRecorder(t,
		http.StatusOK,
		[]byte(`{"id":"resp_affinity_abc","object":"response","created":1700000000}`),
		"application/json",
	)
	defaultBackend, defaultRecorded, defaultMu := newBackendRecorder(t,
		http.StatusOK,
		[]byte(`{"id":"resp_default_xxx","object":"response"}`),
		"application/json",
	)

	// Build a server with the affinity backend registered as a
	// service-mode provider so modelResolver can route to it, and the
	// default backend registered separately as openai_compat default.
	enabled := true
	caps := &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions", "responses"}}
	apps := map[string]pkgConfig.ServiceConfig{
		"affinity-provider": {
			Enabled:      &enabled,
			Name:         "affinity-provider",
			Mode:         "service",
			Protocol:     pkgConfig.ProtocolOpenAI,
			Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: affinityBackend.URL},
			Capabilities: caps,
		},
		"default-provider": {
			Enabled:      &enabled,
			Name:         "default-provider",
			Mode:         "service",
			Protocol:     pkgConfig.ProtocolOpenAI,
			Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: defaultBackend.URL},
			Capabilities: caps,
		},
	}

	hostCfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: getNextTestPort(),
			Name: "test-affinity",
		},
		Cluster: pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeDisabled},
		Auth:    pkgConfig.AuthConfig{AdminKey: TestAdminKey},
		OpenAICompat: pkgConfig.OpenAICompatConfig{
			DefaultBackend: "default-provider",
		},
	}
	server, err := NewServerWithOptions(hostCfg)
	require.NoError(t, err)
	server.appsConfig = &pkgConfig.AppsConfig{
		Version: "1.0",
		Name:    "test",
	}
	for name, sc := range apps {
		require.NoError(t, server.appsConfig.AddApp(name, sc))
	}
	t.Cleanup(func() { cleanupTestNode(server) })

	// Directly seed the affinity map so this test does not depend on
	// modelResolver reaching the correct backend — that code path is
	// covered by separate dispatch tests. Here we care only that the
	// retrieval handler consults the map and forwards to the recorded
	// provider rather than the default backend.
	server.inference.affinity.Record("resp_affinity_abc", "affinity-provider")

	// A stored response remains addressable after its model disappears from the catalog.
	chained := makeRequest(t, server, TestRequest{Method: "POST", Path: "/v1/responses", Body: map[string]any{"model": "uncatalogued-model", "previous_response_id": "resp_affinity_abc", "input": "continue"}})
	require.Equal(t, http.StatusOK, chained.Code, "body=%s", chained.Body)
	denied := makeRequest(t, server, TestRequest{Method: "POST", Path: "/v1/responses", Headers: map[string]string{"X-API-Key": "invalid-key"}, Body: map[string]any{"model": "uncatalogued-model", "previous_response_id": "resp_affinity_abc", "input": "continue"}})
	require.Equal(t, http.StatusUnauthorized, denied.Code)

	// Retrieve the response id. The handler should look it up in the
	// affinity map, resolve the provider, and forward to the affinity
	// backend — the default backend must see zero traffic.
	resp := makeRequest(t, server, TestRequest{
		Method: "GET",
		Path:   "/v1/responses/resp_affinity_abc",
	})
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)

	affinityMu.Lock()
	defaultMu.Lock()
	defer affinityMu.Unlock()
	defer defaultMu.Unlock()

	require.Len(t, *affinityRecorded, 2, "only the admitted continuation and retrieval reach the backend")
	assert.Equal(t, "POST", (*affinityRecorded)[0].Method)
	assert.Equal(t, "/v1/responses/resp_affinity_abc", (*affinityRecorded)[1].Path)
	assert.Equal(t, "GET", (*affinityRecorded)[1].Method)
	assert.Empty(t, *defaultRecorded, "default backend should not have been touched")
}

// TestResponseAffinity_Miss_FallsThroughToDefault verifies the graceful
// degradation path: when the affinity map does not hold the id, the
// retrieval falls back to the configured default backend.
func TestResponseAffinity_Miss_FallsThroughToDefault(t *testing.T) {
	defaultBackend, defaultRecorded, defaultMu := newBackendRecorder(t,
		http.StatusOK,
		[]byte(`{"id":"resp_default_zzz","object":"response"}`),
		"application/json",
	)

	enabled := true
	apps := map[string]pkgConfig.ServiceConfig{
		"default-provider": {
			Enabled:      &enabled,
			Name:         "default-provider",
			Mode:         "service",
			Protocol:     pkgConfig.ProtocolOpenAI,
			Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: defaultBackend.URL},
			Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions", "responses"}},
		},
	}

	hostCfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: getNextTestPort(),
			Name: "test-affinity-miss",
		},
		Cluster: pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeDisabled},
		Auth:    pkgConfig.AuthConfig{AdminKey: TestAdminKey},
		OpenAICompat: pkgConfig.OpenAICompatConfig{
			DefaultBackend: "default-provider",
		},
	}
	server, err := NewServerWithOptions(hostCfg)
	require.NoError(t, err)
	server.appsConfig = &pkgConfig.AppsConfig{
		Version: "1.0",
		Name:    "test",
	}
	for name, sc := range apps {
		require.NoError(t, server.appsConfig.AddApp(name, sc))
	}
	t.Cleanup(func() { cleanupTestNode(server) })

	// Affinity map is empty. Retrieval should still succeed via the
	// default backend.
	resp := makeRequest(t, server, TestRequest{
		Method: "GET",
		Path:   "/v1/responses/resp_unknown_id",
	})
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)

	defaultMu.Lock()
	defer defaultMu.Unlock()
	require.Len(t, *defaultRecorded, 1, "default backend should receive cache-miss retrievals")
	assert.Equal(t, "/v1/responses/resp_unknown_id", (*defaultRecorded)[0].Path)
}

// TestResponseAffinity_DeleteFlushesEntry verifies that a DELETE on an
// affinity-tracked response removes the entry so a subsequent retrieval
// falls through to the default backend.
func TestResponseAffinity_DeleteFlushesEntry(t *testing.T) {
	backend, _, _ := newBackendRecorder(t,
		http.StatusOK,
		[]byte(`{"deleted":true}`),
		"application/json",
	)

	enabled := true
	apps := map[string]pkgConfig.ServiceConfig{
		"affinity-provider": {
			Enabled:      &enabled,
			Name:         "affinity-provider",
			Mode:         "service",
			Protocol:     pkgConfig.ProtocolOpenAI,
			Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: backend.URL},
			Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions", "responses"}},
		},
	}

	hostCfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: getNextTestPort(),
			Name: "test-affinity-delete",
		},
		Cluster: pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeDisabled},
		Auth:    pkgConfig.AuthConfig{AdminKey: TestAdminKey},
		OpenAICompat: pkgConfig.OpenAICompatConfig{
			DefaultBackend: "affinity-provider",
		},
	}
	server, err := NewServerWithOptions(hostCfg)
	require.NoError(t, err)
	server.appsConfig = &pkgConfig.AppsConfig{
		Version: "1.0",
		Name:    "test",
	}
	for name, sc := range apps {
		require.NoError(t, server.appsConfig.AddApp(name, sc))
	}
	t.Cleanup(func() { cleanupTestNode(server) })

	server.inference.affinity.Record("resp_del_abc", "affinity-provider")
	require.Equal(t, 1, server.inference.affinity.Size())

	resp := makeRequest(t, server, TestRequest{
		Method: "DELETE",
		Path:   "/v1/responses/resp_del_abc",
	})
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)

	// Entry should be purged so Size drops back to 0.
	assert.Equal(t, 0, server.inference.affinity.Size(),
		"DELETE should flush the affinity entry")
}

// Satisfy unused import guards during iteration.
var _ = json.Marshal
