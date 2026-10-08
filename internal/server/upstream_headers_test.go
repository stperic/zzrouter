package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/fallback"
)

type capturingRoundTripper struct{ got *http.Request }

func (c *capturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.got = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
		Request:    req,
	}, nil
}

// callerRequestHeaders is what a caller's request arrives with: a key in
// every header zzRouter reads one from, routing hints, connection headers,
// and the protocol headers an upstream does read.
func callerRequestHeaders() http.Header {
	h := http.Header{}
	for _, name := range nodeAPIKeyHeaders {
		h.Set(name, name+"-secret")
	}
	h.Set(constants.HeaderServingNode, "coordinator-host")
	h.Set(constants.HeaderServingProvider, "some-provider")
	h.Set("Connection", "keep-alive, X-Per-Hop")
	h.Set("X-Per-Hop", "1")
	h.Set("Proxy-Authorization", "Basic cHJveHk=")
	h.Set("Accept-Encoding", "gzip")
	h.Set("Content-Encoding", "identity")
	h.Set("anthropic-version", "2023-06-01")
	return h
}

// Each upstream gets exactly what it is owed: an engine no key, an
// external endpoint the caller's own key and no other, a credentialed
// one its own credential in place of the caller's, and a worker
// everything it needs to dispatch again. None gets the caller's
// connection headers, and all get the protocol headers.
func TestUpstreamHeaders(t *testing.T) {
	providerCred := &config.APIConfig{AuthType: config.AuthTypeAPIKey, AuthHeader: "X-API-Key", Token: "provider-key"}
	cases := []struct {
		name  string
		up    backend.Upstream
		keys  map[string]string // key header -> value it must carry; absent means stripped
		hints bool
	}{
		{"engine", backend.Engine(), nil, false},
		{"caller key", backend.CallerKey(), map[string]string{
			"X-API-Key":         "X-API-Key-secret",
			authorizationHeader: authorizationHeader + "-secret",
		}, false},
		{"credential", backend.Credential(providerCred), map[string]string{"X-API-Key": "provider-key"}, false},
		{"cluster", backend.Cluster(), map[string]string{
			"X-API-Key":         "X-API-Key-secret",
			"X-Admin-API-Key":   "X-Admin-API-Key-secret",
			"X-User-API-Key":    "X-User-API-Key-secret",
			"X-Cluster-API-Key": "X-Cluster-API-Key-secret",
			authorizationHeader: authorizationHeader + "-secret",
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := callerRequestHeaders()
			got := upstreamHeaders(src, tc.up)

			for _, name := range nodeAPIKeyHeaders {
				assert.Equal(t, tc.keys[name], got.Get(name), "key header %s", name)
			}
			for _, name := range constants.RoutingHintHeaders() {
				assert.Equal(t, tc.hints, got.Get(name) != "", "routing hint %s", name)
			}
			for _, name := range []string{"Connection", "X-Per-Hop", "Proxy-Authorization", "Accept-Encoding"} {
				assert.Empty(t, got.Get(name), "%s describes the caller's connection", name)
			}
			assert.Equal(t, "identity", got.Get("Content-Encoding"), "the body travels as it arrived")
			assert.Equal(t, "2023-06-01", got.Get("anthropic-version"))
			assert.Equal(t, "X-API-Key-secret", src.Get("X-API-Key"), "the caller's request is not modified")
		})
	}
}

// The rule holds on the wire, not only in the helper: a forward to a
// credentialed upstream sends its credential and keeps the query string
// (Anthropic clients send ?beta=true).
func TestForwardToBackend_SendsTheUpstreamsHeaders(t *testing.T) {
	rt := &capturingRoundTripper{}
	pc := NewProxyClient(&http.Client{Transport: rt}, nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", bytes.NewReader([]byte(`{}`)))
	req.Header = callerRequestHeaders()
	apiCfg := &config.APIConfig{AuthType: config.AuthTypeCustom, CustomHeaders: map[string]string{"x-api-key": "provider-key"}}
	target := backend.PassthroughTarget("https://api.example.test", req.URL.Path, req.URL.RawQuery)

	pc.ForwardToBackend(httptest.NewRecorder(), req, "anthropic", target, backend.Credential(apiCfg), []byte(`{}`))

	require.NotNil(t, rt.got)
	assert.Equal(t, "beta=true", rt.got.URL.RawQuery)
	for name, values := range rt.got.Header {
		for _, v := range values {
			assert.NotContains(t, v, "-secret", "header %s leaked a zzRouter key", name)
		}
	}
	assert.Empty(t, rt.got.Header.Get(constants.HeaderServingNode), "a third party learns no node names")
	assert.Equal(t, "provider-key", rt.got.Header.Get("X-Api-Key"))
	assert.Equal(t, "2023-06-01", rt.got.Header.Get("Anthropic-Version"))
}

// A worker's cluster port is reached over the mTLS client, and only it;
// with none, the forward fails rather than going out over plain HTTP.
func TestForwardDetached_ClusterGoesOverTheClusterClient(t *testing.T) {
	plain, mtls := &capturingRoundTripper{}, &capturingRoundTripper{}
	pc := NewProxyClient(&http.Client{Transport: plain},
		func() *http.Client { return &http.Client{Transport: mtls} }, nil, nil, nil, nil)

	resp, err := pc.ForwardDetached(t.Context(), http.MethodPost, "https://w:9091/v1/chat/completions",
		http.Header{}, backend.Cluster(), []byte(`{}`))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.NotNil(t, mtls.got)
	assert.Nil(t, plain.got)

	none := NewProxyClient(&http.Client{Transport: plain}, nil, nil, nil, nil, nil)
	_, err = none.ForwardDetached(t.Context(), http.MethodPost, "https://w:9091/v1/chat/completions", //nolint:bodyclose // no response on error
		http.Header{}, backend.Cluster(), []byte(`{}`))
	require.ErrorIs(t, err, errNoClusterClient)
	assert.ErrorIs(t, err, fallback.ErrUnreachable, "the chain passes over it to the next replica")
	assert.Nil(t, plain.got, "never over plain HTTP")
}

// A health target for a replica on another node is that node's advertised
// address, never one guessed from its name.
func TestDeploymentHealthTarget_NeverGuessesARemoteNode(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	base, _ := s.deploymentHealthTarget("vllm", "unknown-node")
	assert.Empty(t, base)
}

// A deployment's health probe carries the credential its provider
// declares, so a token-protected endpoint is not read as down; a replica
// on another node is probed at that node, which is sent none.
func TestDeploymentHealthTarget_CarriesTheDeclaredCredential(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	s.appsConfig = &config.AppsConfig{Version: "1.0", Name: "test"}
	enabled := true
	nas := config.NewOllamaConnectProvider("ollama-nas", "http://nas.lan:11434", "nas-token")
	require.NoError(t, s.appsConfig.AddApp("ollama-nas", config.ServiceConfig{
		Enabled: &enabled, Name: "ollama-nas", Mode: "external", Protocol: nas.Protocol,
		Runtime:      &config.AppRuntimeConfig{Endpoint: nas.Runtime.Endpoint, API: nas.Runtime.API},
		Capabilities: nas.Capabilities,
	}))

	base, header := s.deploymentHealthTarget("ollama-nas", "")
	assert.Equal(t, "http://nas.lan:11434", base)
	assert.Equal(t, "Bearer nas-token", header.Get("Authorization"))

	coord, err := mesh.NewCluster(&mesh.Config{}, "http://127.0.0.1:9090", nil, nil, nil)
	require.NoError(t, err)
	s.cluster.coordinator = coord
	require.NoError(t, s.cluster.coordinator.RegisterEndpoint(&mesh.Endpoint{Name: "worker-1", URL: "http://10.9.9.9:9090"}))
	base, header = s.deploymentHealthTarget("ollama-nas", "worker-1")
	assert.Equal(t, "http://10.9.9.9:9090", base)
	assert.Empty(t, header, "a remote node's health route is sent no provider credential")
}

// The provider status and env-apply probes reach the provider's own
// endpoint, so they carry the credential it declares.
func TestParamsHealthProbe_SendsTheDeclaredCredential(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer nas-token" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer daemon.Close()

	nas := config.NewOllamaConnectProvider("ollama-nas", daemon.URL, "nas-token")
	cfg := &config.ServiceConfig{Mode: "external", Runtime: &config.AppRuntimeConfig{Endpoint: daemon.URL, API: nas.Runtime.API}}
	e := &ParamsExecutor{httpClient: daemon.Client()}
	assert.True(t, e.checkHealth(t.Context(), cfg))
	assert.True(t, e.waitForHealth(t.Context(), cfg, time.Second))
}
