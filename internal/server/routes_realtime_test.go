// Integration tests for /v1/realtime WebSocket proxy.
//
// Acceptance bar: the handler upgrades the client, dials the configured
// backend, and bidirectionally proxies frames. Connection failure paths
// are covered by TestRealtime_NoBackendConfigured_Returns503 in
// integration_phase9_passthrough_test.go.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startRealtimeBackend spins up a test WebSocket server that echoes
// every text/binary message it receives. Returns the httptest.Server
// (caller is responsible for Close via t.Cleanup) and its ws:// URL
// base.
func startRealtimeBackend(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("backend upgrade failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, append([]byte("echo:"), data...)); err != nil {
				return
			}
		}
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(func() { ts.Close() })
	return ts, ts.URL
}

// startRealtimeZZRouter builds a zzrouter test server configured to
// proxy /v1/realtime to the given backend URL, then starts it on a
// real TCP listener so the WebSocket client library can dial it. The
// in-memory httptest transport used by makeRequest cannot carry a
// WebSocket upgrade, hence the real listener.
func startRealtimeZZRouter(t *testing.T, backendURL string) string {
	t.Helper()

	enabled := true
	apps := map[string]pkgConfig.ServiceConfig{
		"realtime-backend": {
			Enabled:      &enabled,
			Name:         "realtime-backend",
			Mode:         "service",
			Protocol:     pkgConfig.ProtocolOpenAI,
			Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: backendURL},
			Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		},
	}

	hostCfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: getNextTestPort(),
			Name: "test-realtime",
		},
		Cluster: pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeDisabled},
		Auth:    pkgConfig.AuthConfig{AdminKey: TestAdminKey},
		OpenAICompat: pkgConfig.OpenAICompatConfig{
			RealtimeBackend: "realtime-backend",
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

	// Serve the gin engine on a real httptest.Server so the gorilla
	// websocket dialer can upgrade against it.
	zz := httptest.NewServer(server.engine)
	t.Cleanup(func() {
		zz.Close()
		cleanupTestNode(server)
	})
	return zz.URL
}

// TestRealtime_UpgradeAndEchoRoundTrip verifies the full upgrade path:
// client → zzrouter → backend, send a text frame, receive the backend's
// echo on the way back through zzrouter.
func TestRealtime_UpgradeAndEchoRoundTrip(t *testing.T) {
	_, backendURL := startRealtimeBackend(t)
	zzURL := startRealtimeZZRouter(t, backendURL)

	// Convert the zzrouter http:// URL to ws:// and point it at
	// /v1/realtime.
	zzParsed, err := url.Parse(zzURL)
	require.NoError(t, err)
	zzParsed.Scheme = "ws"
	zzParsed.Path = "/v1/realtime"
	zzParsed.RawQuery = "model=test-realtime-model"

	dialer := websocket.Dialer{
		HandshakeTimeout: 2 * time.Second,
	}
	conn, resp, err := dialer.Dial(zzParsed.String(), nil) //nolint:bodyclose // ws upgrade has no body on success
	require.NoError(t, err, "upgrade failed (resp=%+v)", resp)
	defer func() { _ = conn.Close() }()

	// Send a text frame through zzrouter. The backend echoes it with
	// an "echo:" prefix, and that message should come back on the
	// same connection.
	const payload = "hello realtime"
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(payload)))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	mt, got, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.TextMessage, mt)
	assert.Equal(t, "echo:"+payload, string(got))
}

// TestRealtime_UpgradeBinaryFrameRoundTrip exercises the binary-frame
// path to make sure the proxy does not corrupt opcode or payload.
// Realtime audio uses binary frames in production.
func TestRealtime_UpgradeBinaryFrameRoundTrip(t *testing.T) {
	_, backendURL := startRealtimeBackend(t)
	zzURL := startRealtimeZZRouter(t, backendURL)

	zzParsed, _ := url.Parse(zzURL)
	zzParsed.Scheme = "ws"
	zzParsed.Path = "/v1/realtime"

	conn, _, err := websocket.DefaultDialer.Dial(zzParsed.String(), nil) //nolint:bodyclose // ws upgrade has no body on success
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	payload := []byte{0x00, 0xFF, 0x10, 0x20, 0xAA, 0xBB}
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, payload))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	mt, got, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.BinaryMessage, mt)
	assert.True(t, strings.HasPrefix(string(got), "echo:"), "expected echo prefix")
	assert.Equal(t, payload, got[len("echo:"):], "binary payload must round-trip intact")
}

// TestRealtime_SessionHelperEndpointsReachRouter verifies the HTTP
// helper endpoints (/v1/realtime/sessions, /v1/realtime/transcription_sessions,
// /v1/realtime/client_secrets) resolve via the shared model-field
// routing path. They exercise routeByModelField, which requires a
// registered provider for the requested model — we do not have model
// resolution stood up in this test, so we look for the model_not_found
// error shape emitted by the resolver as proof the handler was reached
// (a 404 from gin's NoRoute catch-all would use a different envelope
// and would not carry the model_not_found code).
func TestRealtime_SessionHelperEndpointsReachRouter(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	for _, path := range []string{
		"/v1/realtime/sessions",
		"/v1/realtime/transcription_sessions",
		"/v1/realtime/client_secrets",
	} {
		resp := makeRequest(t, server, TestRequest{
			Method: "POST",
			Path:   path,
			Body:   map[string]string{"model": "nonexistent-model"},
		})
		var env struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(resp.Body, &env),
			"%s: unexpected non-JSON response: %s", path, resp.Body)
		// The route was registered (so the request reached the
		// model-routing path) iff we see the resolver's error
		// code rather than gin's catch-all NoRoute shape.
		assert.Equal(t, "model_not_found", env.Error.Code,
			"%s: expected model_not_found from the resolver, got: %s", path, resp.Body)
	}
}

// The upstream dial carries what the forward rule says, like every other
// forward: this backend is an external endpoint, so the caller's own key
// and the protocol headers reach it, and the cluster key does not.
func TestRealtime_DialCarriesTheUpstreamsHeaders(t *testing.T) {
	got := make(chan http.Header, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(backend.Close)
	zzParsed, err := url.Parse(startRealtimeZZRouter(t, backend.URL))
	require.NoError(t, err)
	zzParsed.Scheme = "ws"
	zzParsed.Path = "/v1/realtime"

	h := http.Header{}
	h.Set("X-API-Key", TestAdminKey)
	h.Set("X-Cluster-API-Key", TestClusterKey)
	h.Set("OpenAI-Beta", "realtime=v1")
	conn, resp, err := (&websocket.Dialer{HandshakeTimeout: 2 * time.Second}).Dial(zzParsed.String(), h) //nolint:bodyclose // ws upgrade has no body on success
	require.NoError(t, err, "upgrade failed (resp=%+v)", resp)
	_ = conn.Close()

	seen := <-got
	assert.Equal(t, TestAdminKey, seen.Get("X-API-Key"))
	assert.Empty(t, seen.Get("X-Cluster-API-Key"), "the cluster key never leaves the cluster")
	assert.Equal(t, "realtime=v1", seen.Get("OpenAI-Beta"))
}

// Refused upgrades must never reach the backend, and a socket owns its
// concurrency slot until it closes.
func TestRealtime_EnforcesOpaqueAccessBeforeDial(t *testing.T) {
	var dials atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(backend.Close)
	s := createTestNodeWithDefaults(t)
	s.config.OpenAICompat.RealtimeBackend = "realtime-test"
	s.appsConfig = &pkgConfig.AppsConfig{Version: "1.0", Name: "test"}
	enabled := true
	require.NoError(t, s.appsConfig.AddApp("realtime-test", pkgConfig.ServiceConfig{
		Enabled: &enabled, Name: "realtime-test", Mode: "external", Protocol: pkgConfig.ProtocolOpenAI,
		Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: backend.URL},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	zz := httptest.NewServer(s.engine)
	t.Cleanup(zz.Close)
	endpoint := "ws" + strings.TrimPrefix(zz.URL, "http") + "/v1/realtime?model=test"
	createKey := func(body map[string]any) string {
		resp := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/keys", TestAdminKey, body)
		require.Equal(t, http.StatusCreated, resp.Code, string(resp.Body))
		var out struct {
			Data struct {
				Key string `json:"key"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(resp.Body, &out))
		return out.Data.Key
	}
	dial := func(key string) (*websocket.Conn, *http.Response, error) {
		h := http.Header{}
		if key != "" {
			h.Set("X-API-Key", key)
		}
		return websocket.DefaultDialer.Dial(endpoint, h) //nolint:bodyclose // caller closes failed handshake bodies
	}
	refuse := func(key string, status int) {
		before := dials.Load()
		conn, resp, err := dial(key)
		if conn != nil {
			_ = conn.Close()
		}
		require.Error(t, err)
		require.NotNil(t, resp)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, status, resp.StatusCode)
		assert.Equal(t, before, dials.Load(), "refused socket must not dial upstream")
	}
	refuse(createKey(map[string]any{"id": "spend", "name": "Spend", "spend_limit": 5}), http.StatusForbidden)
	refuse(createKey(map[string]any{"id": "tokens", "name": "Tokens", "tpm_limit": 1000}), http.StatusForbidden)
	team := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/teams", TestAdminKey,
		map[string]any{"id": "restricted", "name": "Restricted", "allowed_models": []string{"test"}})
	require.Equal(t, http.StatusCreated, team.Code, string(team.Body))
	refuse(createKey(map[string]any{"id": "member", "name": "Member", "team_id": "restricted"}), http.StatusForbidden)
	suspended := createKey(map[string]any{"id": "suspended", "name": "Suspended"})
	changed := makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/keys/suspended", TestAdminKey, map[string]any{"suspended": true})
	require.Equal(t, http.StatusOK, changed.Code, string(changed.Body))
	refuse(suspended, http.StatusUnauthorized)
	rpm := createKey(map[string]any{"id": "rpm", "name": "RPM", "rpm_limit": 1})
	conn, resp, err := dial(rpm) //nolint:bodyclose // successful WebSocket upgrade has no response body
	require.NoError(t, err, "response: %v", resp)
	require.NoError(t, conn.Close())
	refuse(rpm, http.StatusTooManyRequests)
	parallel := createKey(map[string]any{"id": "parallel", "name": "Parallel", "max_parallel_requests": 1})
	first, resp, err := dial(parallel) //nolint:bodyclose // successful WebSocket upgrade has no response body
	require.NoError(t, err, "response: %v", resp)
	defer func() { _ = first.Close() }()
	refuse(parallel, http.StatusTooManyRequests)
	require.NoError(t, first.Close())
	require.Eventually(t, func() bool {
		replacement, answer, dialErr := dial(parallel)
		if answer != nil && dialErr != nil {
			_ = answer.Body.Close()
		}
		if dialErr != nil {
			return false
		}
		_ = replacement.Close()
		return true
	}, time.Second, 10*time.Millisecond, "closed socket must release concurrency")
	// Remove the team gate so this specifically exercises cloud admission.
	changed = makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/teams/restricted", TestAdminKey, map[string]any{"allowed_models": []string{}})
	require.Equal(t, http.StatusOK, changed.Code, string(changed.Body))
	// An anonymous caller must not spend this node's cloud credential.
	require.NoError(t, s.appsConfig.AddApp("cloud-realtime", pkgConfig.ServiceConfig{
		Enabled: &enabled, Name: "cloud-realtime", Mode: "cloud", Protocol: pkgConfig.ProtocolOpenAI,
		Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: backend.URL, API: &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "test-cloud-token"}},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	s.config.OpenAICompat.RealtimeBackend = "cloud-realtime"
	refuse("", http.StatusUnauthorized)
}
