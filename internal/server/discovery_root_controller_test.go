package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// TestAPIDiscovery_NoMissingRoutes pins the contract that every
// /zzrouter/v1/* admin route registered on the public engine appears in
// the discovery doc, and the harness/internal/route table never leaks
// in. This is what catches future hand-curated drift if anyone reverts
// buildAPIDiscovery to a literal map.
func TestAPIDiscovery_NoMissingRoutes(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeAuthRequest(t, server, "GET", apiDiscoveryPrefix, TestAdminKey, nil)
	require.Equalf(t, http.StatusOK, resp.Code, "discovery endpoint failed: body=%s", string(resp.Body))

	var env struct {
		Data apiDiscoveryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))

	advertised := map[string]struct{}{}
	for _, entries := range env.Data.EndpointGroups {
		for _, e := range entries {
			advertised[e] = struct{}{}
		}
	}

	for _, r := range server.engine.Routes() {
		if !strings.HasPrefix(r.Path, apiDiscoveryPrefix) {
			continue
		}
		if strings.HasPrefix(r.Path, apiDiscoveryPrefix+"/internal/") {
			continue
		}
		if _, hidden := apiDiscoveryHidden[r.Path]; hidden {
			continue
		}
		// The invariant is "every route a client can CALL is advertised",
		// not "every route that is mounted". A retired route only answers
		// 410, so advertising it hands a caller a guaranteed dead end —
		// see TestAPIDiscoveryOmitsRetiredRoutes.
		if isRetiredRoute(r.Handler) {
			continue
		}
		key := r.Method + " " + uriTemplatePath(r.Path)
		_, found := advertised[key]
		assert.Truef(t, found, "admin route %s not advertised in /zzrouter/v1 discovery", key)
	}
}

// TestAPIDiscovery_HiddenRoutesNotAdvertised ensures the e2e harness
// gates (server/routes, state/snapshot, state/restore) stay out of the
// agent-facing discovery doc even though they live on the public engine.
func TestAPIDiscovery_HiddenRoutesNotAdvertised(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeAuthRequest(t, server, "GET", apiDiscoveryPrefix, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code)

	var env struct {
		Data apiDiscoveryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))

	for _, entries := range env.Data.EndpointGroups {
		for _, e := range entries {
			for hidden := range apiDiscoveryHidden {
				for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
					assert.NotEqualf(t, m+" "+hidden, e,
						"hidden harness route %s leaked into discovery", e)
				}
			}
		}
	}
}

// TestAPIDiscovery_KeyAgentEndpointsPresent pins the routes that are
// most load-bearing for an AI agent control plane. If any of these
// disappears from /zzrouter/v1 the agent loses a critical capability
// and we want to know at test time, not at agent-runtime.
func TestAPIDiscovery_KeyAgentEndpointsPresent(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeAuthRequest(t, server, "GET", apiDiscoveryPrefix, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code)

	var env struct {
		Data apiDiscoveryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))

	advertised := map[string]struct{}{}
	for _, entries := range env.Data.EndpointGroups {
		for _, e := range entries {
			advertised[e] = struct{}{}
		}
	}

	// Hand-picked from the AI-agent gap analysis: each is the canonical
	// way to perform a class of operation an agent would need.
	mustHave := []string{
		"GET /zzrouter/v1",                                 // self-bootstrap
		"POST /zzrouter/v1/runs/ensure",                    // idempotent hot-make
		"POST /zzrouter/v1/runs/preview",                   // dry-run launch plan
		"GET /zzrouter/v1/runs/capabilities",               // provider+endpoint catalog
		"GET /zzrouter/v1/jobs",                            // async progress
		"GET /zzrouter/v1/jobs/{id}/stream",                // SSE follow
		"GET /zzrouter/v1/providers/status",                // rate-limit + cooldown
		"GET /zzrouter/v1/pricing/status",                  // pricing presence probe
		"GET /zzrouter/v1/update/status",                   // auto-update probe
		"GET /zzrouter/v1/registries/huggingface/variants", // pre-pull discovery
		"GET /zzrouter/v1/keys/schema",                     // key field schema for agents
		"GET /zzrouter/v1/teams/schema",                    // team field schema for agents
		"GET /zzrouter/v1/cluster/hardware/gpus",           // flat per-GPU rows across cluster
		"GET /zzrouter/v1/spend/events",                    // SSE: quota breach events
	}
	for _, want := range mustHave {
		_, found := advertised[want]
		assert.Truef(t, found, "load-bearing agent endpoint %s missing from discovery", want)
	}
}

// TestAPIDiscovery_RootEndpointSelfAdvertised — the discovery doc must
// list itself, otherwise an agent that walks it cannot rediscover the
// entry point on a refresh.
func TestAPIDiscovery_RootEndpointSelfAdvertised(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeAuthRequest(t, server, "GET", apiDiscoveryPrefix, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code)

	var env struct {
		Data apiDiscoveryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))

	disc, ok := env.Data.EndpointGroups["discovery"]
	require.Truef(t, ok, "discovery group missing from doc; have %v", keysOf(env.Data.EndpointGroups))
	assert.Containsf(t, disc, "GET "+apiDiscoveryPrefix,
		"discovery doc must self-advertise its root entry")
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestURITemplatePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"no parameters", "/zzrouter/v1/models", "/zzrouter/v1/models"},
		{"single parameter", "/zzrouter/v1/deployments/:id", "/zzrouter/v1/deployments/{id}"},
		{"multiple parameters", "/zzrouter/v1/deployments/:id/nodes/:node", "/zzrouter/v1/deployments/{id}/nodes/{node}"},
		{"catch-all", "/zzrouter/v1/models/instance/*model", "/zzrouter/v1/models/instance/{model}"},
		{"mixed parameter and catch-all", "/zzrouter/v1/models/card/:provider/*id", "/zzrouter/v1/models/card/{provider}/{id}"},
		{"parameter mid-path", "/zzrouter/v1/keys/:id/usage", "/zzrouter/v1/keys/{id}/usage"},
		{"bare colon is not a parameter", "/zzrouter/v1/a:b", "/zzrouter/v1/a:b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, uriTemplatePath(tt.path))
		})
	}
}

// TestAPIDiscovery_InferenceEndpointsAreRegistered keeps the one curated
// map in this document honest. endpoint_groups is derived from the route
// table and cannot lie; the inference block names paths by hand because
// its keys are the catalog's capability vocabulary, not path segments.
// This pins each value to a route that actually exists.
func TestAPIDiscovery_InferenceEndpointsAreRegistered(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	registered := map[string]struct{}{}
	for _, r := range server.engine.Routes() {
		registered[r.Method+" "+r.Path] = struct{}{}
	}

	inference := describeInference(server)
	for capability, route := range inference.Endpoints {
		_, found := registered[route]
		assert.Truef(t, found, "discovery advertises %q for capability %q but no such route is registered",
			route, capability)
	}
	_, found := registered["GET /v1/models"]
	assert.True(t, found, "discovery advertises the catalog at GET /v1/models")
}

// The capability vocabulary the catalog publishes and the one discovery
// can address must be the same set, or an agent reads endpoints:["rerank"]
// off a model and has nowhere to send it.
func TestAPIDiscovery_InferenceCoversCatalogVocabulary(t *testing.T) {
	t.Parallel()

	all := OpenAIModelCapabilities{
		Chat: true, Completions: true, Embeddings: true, Rerank: true,
	}
	vocabulary := endpointsForCapabilities(all)
	for _, w := range wireServedEndpoints {
		vocabulary = append(vocabulary, w.name)
	}
	for _, capability := range vocabulary {
		_, found := inferenceEndpointPaths[capability]
		assert.Truef(t, found, "catalog publishes endpoint %q with no path in discovery", capability)
	}
}

// TestAPIDiscovery_StreamingGETsAreRegistered pins the other hand-kept
// list in this document. Pointing a subscriber at a route that no longer
// exists is worse than not warning it at all.
func TestAPIDiscovery_StreamingGETsAreRegistered(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	registered := map[string]struct{}{}
	for _, r := range server.engine.Routes() {
		registered[r.Method+" "+uriTemplatePath(r.Path)] = struct{}{}
	}

	for _, route := range streamingGETs {
		_, found := registered[route]
		assert.Truef(t, found, "discovery flags %q as streaming but no such route is registered", route)
	}

	// The events family is where the next one will appear, so hold it to
	// the list rather than trusting a future author to remember.
	advertised := make(map[string]struct{}, len(streamingGETs))
	for _, r := range streamingGETs {
		advertised[r] = struct{}{}
	}
	for _, r := range server.engine.Routes() {
		if !strings.HasSuffix(r.Handler, "EventsStream-fm") {
			continue
		}
		key := r.Method + " " + uriTemplatePath(r.Path)
		_, found := advertised[key]
		assert.Truef(t, found, "%s streams events but is not in streamingGETs", key)
	}
}

// A worker serves the compat surface on the cluster mTLS port, so the
// inference block must not name paths the port serving this document
// answers 404 for — the same rule describeCompatAuth already follows.
func TestAPIDiscovery_InferenceIsNotAdvertisedOnAWorker(t *testing.T) {
	t.Parallel()

	worker := &Server{config: &pkgConfig.NodeConfig{
		Cluster: pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeWorker},
	}}

	inference := describeInference(worker)
	assert.False(t, inference.ServedHere)
	assert.Empty(t, inference.Endpoints, "a worker must not name paths this port does not serve")
	assert.Empty(t, inference.Catalog)
	assert.Contains(t, inference.Note, "cluster mTLS port")
}
