package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nodeHealthServer(t *testing.T) (*Server, string, *atomic.Int32, *atomic.Bool) {
	t.Helper()
	cfg := DefaultTestNodeConfig()
	cfg.ClusterMode = pkgConfig.ClusterModeCoordinator
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: cfg})
	require.NotNil(t, s.cluster.coordinator)
	s.cluster.coordinator.GetHealthMonitor().Stop()
	var sends atomic.Int32
	var broken atomic.Bool
	broken.Store(true)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		if broken.Load() {
			conn, _, err := http.NewResponseController(w).Hijack()
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"provider unavailable"}}`)
	}))
	t.Cleanup(peer.Close)
	r := s.cluster.coordinator.GetState().GetEndpoints()
	require.NoError(t, r.RegisterEndpoint(&mesh.Endpoint{URL: "http://worker.invalid:9080", ClusterURL: peer.URL, NodeName: "worker", Name: "worker"}))
	r.Observe("http://worker.invalid:9080", mesh.ProbeResult{OK: true, Quality: mesh.QualityFull, Snapshot: &mesh.Connection{NodeName: "worker", HealthReport: mesh.HealthReport{ClusterRole: "worker"}}})
	return s, peer.URL, &sends, &broken
}

func TestNodeBreaker_AllDispatchPaths(t *testing.T) {
	for _, path := range []string{"direct inference", "fallback inference", "jobs", "provider sync", "management"} {
		t.Run(path, func(t *testing.T) {
			s, address, sends, _ := nodeHealthServer(t)
			clock := clocktest.NewFakeClock(utils.Now())
			t.Cleanup(utils.SetClock(clock))
			var send func()
			switch path {
			case "direct inference":
				send = func() {
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
					s.proxyToRemoteNode(c, "worker", "vllm", []byte(`{"model":"m"}`))
					assert.Equal(t, http.StatusBadGateway, w.Code)
					assert.Contains(t, w.Body.String(), "backend_unreachable")
				}
			case "fallback inference":
				send = func() {
					resp, err := s.proxy.ForwardDetached(context.Background(), "POST", address+"/v1/chat/completions", nil, backend.Cluster(), []byte(`{"model":"m"}`))
					if resp != nil {
						require.NoError(t, resp.Body.Close())
					}
					require.Error(t, err)
					var requestErr *fallback.RequestError
					require.ErrorAs(t, err, &requestErr)
					if sends.Load() == 3 && errors.Is(err, fallback.ErrUnreachable) {
						assert.True(t, fallback.ReplaySafe("POST", err, 0, requestErr.WroteRequest))
					}
				}
			case "jobs":
				send = func() {
					resp, err := s.jobsProxy.open(context.Background(), "GET", address+"/zzrouter/v1/internal/jobs/missing/stream", "text/event-stream")
					if resp != nil {
						require.NoError(t, resp.Body.Close())
					}
					require.Error(t, err)
				}
			case "provider sync":
				require.NotNil(t, s.providers.syncFanOut)
				send = func() {
					assert.False(t, s.providers.syncFanOut.pushOne(s.providers.syncFanOut.dialClient(), address+"/zzrouter/v1/internal/sync/providers/vllm", "vllm", "worker", []byte(`{}`)))
				}
			case "management":
				send = func() {
					_, err := s.cluster.coordinator.GetClient().Unicast(context.Background(), "worker", "/zzrouter/v1/internal/runs", nil)
					require.Error(t, err)
				}
			}
			for i := 1; i <= 3; i++ {
				send()
				assert.EqualValues(t, i, sends.Load())
				assert.Equal(t, i, s.cluster.coordinator.GetState().GetCircuitBreakers().GetBreaker("http://worker.invalid:9080").Metrics()["failure_count"])
			}
			send()
			clock.Advance(time.Minute)
			send()
			assert.EqualValues(t, 3, sends.Load(), "open and cooldown-expired nodes must receive no ordinary sends")
		})
	}
}

func TestNodeStatus_RESTMCPParity(t *testing.T) {
	s, address, _, broken := nodeHealthServer(t)
	clock := clocktest.NewFakeClock(utils.Now())
	t.Cleanup(utils.SetClock(clock))
	client := s.coordWorkerMTLSClient()
	require.NotNil(t, client)
	get := func() error {
		req, err := http.NewRequestWithContext(context.Background(), "GET", address+"/inference", nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			require.NoError(t, resp.Body.Close())
		}
		return err
	}
	readREST := func(path string) map[string]any {
		resp := makeRequest(t, s, TestRequest{Method: "GET", Path: path, Headers: map[string]string{"X-API-Key": TestAdminKey}})
		require.Equal(t, http.StatusOK, resp.Code, string(resp.Body))
		var body map[string]any
		require.NoError(t, json.Unmarshal(resp.Body, &body))
		return body
	}
	call := func(name string, args map[string]any) map[string]any {
		result := mcpWireResponse(t, s, mcpWireRequest(t, "tools/call", "2026-07-28", map[string]any{"name": name, "arguments": args}), http.StatusOK)["result"].(map[string]any)
		require.NotEqual(t, true, result["isError"], result)
		return result["structuredContent"].(map[string]any)
	}
	worker := func(nodes []any) map[string]any {
		for _, node := range nodes {
			m := node.(map[string]any)
			if m["name"] == "worker" {
				return m
			}
		}
		t.Fatal("worker absent from node results")
		return nil
	}
	initial := worker(readREST("/zzrouter/v1/nodes")["data"].([]any))["last_seen_at"]
	require.NotNil(t, initial)
	clock.Advance(time.Minute)
	broken.Store(false)
	for range 3 {
		require.NoError(t, get())
	}
	assert.Equal(t, "healthy", worker(readREST("/zzrouter/v1/nodes")["data"].([]any))["health_status"], "provider errors prove the node is reachable")
	broken.Store(true)
	for range 3 {
		require.Error(t, get())
	}
	check := func(want string) {
		list := worker(readREST("/zzrouter/v1/nodes")["data"].([]any))
		detail := readREST("/zzrouter/v1/nodes/worker")["data"].(map[string]any)
		nodes := worker(call("zz_nodes", map[string]any{})["nodes"].([]any))
		mcpDetail := call("zz_nodes", map[string]any{"node": "worker"})
		overview := worker(call("zz_overview", map[string]any{})["nodes"].([]any))
		for _, node := range []map[string]any{list, detail, nodes, mcpDetail} {
			assert.Equal(t, want, node["health_status"])
			assert.NotEmpty(t, node["last_error"])
		}
		for _, node := range []map[string]any{list, detail} {
			assert.Equal(t, initial, node["last_seen_at"])
		}
		assert.Equal(t, want, overview["health"])
	}
	check("down")
	management := makeRequest(t, s, TestRequest{Method: "GET", Path: "/zzrouter/v1/runs?node=worker", Headers: map[string]string{"X-API-Key": TestAdminKey}})
	assert.Equal(t, http.StatusBadGateway, management.Code, string(management.Body))
	assert.Contains(t, string(management.Body), "node_unreachable")
	b := s.cluster.coordinator.GetState().GetCircuitBreakers().GetBreaker("http://worker.invalid:9080")
	clock.Advance(time.Minute)
	permit, err := b.Acquire(true)
	require.NoError(t, err)
	permit.Complete(fallback.Success)
	check("degraded")
}

func TestNodeBreaker_ReplayEvidence(t *testing.T) {
	s, address, _, _ := nodeHealthServer(t)
	client := s.cluster.coordinator.GetState().HTTPClient(&http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(r.Context()).GetConn(r.URL.Host)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	})})
	proxy := NewProxyClient(nil, func() *http.Client { return client }, nil, nil, nil, nil, nil)
	for range 3 {
		resp, err := proxy.ForwardDetached(context.Background(), "POST", address+"/v1/chat/completions", nil, backend.Cluster(), []byte(`{}`))
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		var requestErr *fallback.RequestError
		require.ErrorAs(t, err, &requestErr)
		assert.False(t, requestErr.WroteRequest)
		assert.True(t, fallback.ReplaySafe("POST", err, 0, requestErr.WroteRequest))
	}
	resp, err := proxy.ForwardDetached(context.Background(), "POST", address+"/v1/chat/completions", nil, backend.Cluster(), []byte(`{}`))
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.ErrorIs(t, err, fallback.ErrUnreachable)
	assert.True(t, fallback.ReplaySafe("POST", err, 0, true))
}
